// Package webhook ingests the upstream provider's billing webhooks and
// serves the reconciliation report computed from them. The receiver
// verifies an HMAC-SHA256 signature over the raw payload, stores the event
// verbatim, and extracts token counts on a best-effort basis: payload
// schemas vary between providers, and the raw payload is always retained
// for audit.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/lightninglabs/lightmeter/store"
)

// maxPayloadBytes bounds the webhook payload size read into memory.
const maxPayloadBytes = 1 << 20

// EventSink stores ingested provider billing events.
type EventSink interface {
	// RecordProviderEvent stores one upstream billing event.
	RecordProviderEvent(ev store.ProviderEvent) error
}

// Reporter computes the reconciliation report.
type Reporter interface {
	// ReconReport compares metered against provider-billed tokens per
	// model since the given time.
	ReconReport(since time.Time) (store.ReconReport, error)
}

// Config configures the webhook server.
type Config struct {
	// ListenAddr is the address the HTTP server listens on.
	ListenAddr string

	// Secret is the shared HMAC secret payloads are signed with.
	Secret string

	// Path is the URL path the webhook receiver is mounted on.
	Path string

	// Sink receives verified events.
	Sink EventSink

	// Reporter serves the reconciliation report. The report endpoint
	// is disabled when nil.
	Reporter Reporter

	// Logger receives ingest diagnostics. A default logger is used
	// when nil.
	Logger *slog.Logger
}

// Server is the webhook HTTP server.
type Server struct {
	cfg Config
	log *slog.Logger
	srv *http.Server
}

// NewServer builds a webhook server from the given config.
func NewServer(cfg Config) (*Server, error) {
	if cfg.ListenAddr == "" {
		return nil, fmt.Errorf("a listen address is required")
	}
	if cfg.Secret == "" {
		return nil, fmt.Errorf("a webhook secret is required")
	}
	if cfg.Sink == nil {
		return nil, fmt.Errorf("an event sink is required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{
		cfg: cfg,
		log: logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+cfg.Path, s.handleWebhook)
	if cfg.Reporter != nil {
		mux.HandleFunc("GET /recon", s.handleRecon)
	}

	s.srv = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	return s, nil
}

// Start launches the HTTP listener.
func (s *Server) Start() {
	go func() {
		err := s.srv.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			s.log.Error("Webhook server exited", "err", err)
		}
	}()

	s.log.Info("Webhook server listening", "addr", s.cfg.ListenAddr,
		"path", s.cfg.Path)
}

// Stop shuts the HTTP listener down.
func (s *Server) Stop() {
	_ = s.srv.Close()
}

// handleWebhook verifies and stores one billing event.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadBytes+1))
	if err != nil {
		http.Error(w, "unable to read body",
			http.StatusBadRequest)
		return
	}
	if len(body) > maxPayloadBytes {
		http.Error(w, "payload too large",
			http.StatusRequestEntityTooLarge)
		return
	}

	if !verifySignature(s.cfg.Secret, body, r.Header) {
		s.log.Warn("Webhook with a missing or invalid signature",
			"remote", r.RemoteAddr)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	ev := parseEvent(body)
	if err := s.cfg.Sink.RecordProviderEvent(ev); err != nil {
		s.log.Error("Unable to store provider event", "err", err)
		http.Error(w, "unable to store event",
			http.StatusInternalServerError)
		return
	}

	s.log.Info("Ingested provider billing event", "event_id",
		ev.EventID, "model", ev.Model, "total_tokens",
		ev.TotalTokens, "amount_usd", ev.AmountUSD)

	w.WriteHeader(http.StatusNoContent)
}

// handleRecon serves the reconciliation report. The window defaults to the
// last 24 hours and can be overridden with a ?since=RFC3339 timestamp.
func (s *Server) handleRecon(w http.ResponseWriter, r *http.Request) {
	since := time.Now().Add(-24 * time.Hour)
	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			http.Error(w, "invalid since timestamp, want RFC3339",
				http.StatusBadRequest)
			return
		}
		since = parsed
	}

	report, err := s.cfg.Reporter.ReconReport(since)
	if err != nil {
		s.log.Error("Unable to compute recon report", "err", err)
		http.Error(w, "unable to compute report",
			http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Since  time.Time         `json:"since"`
		Models store.ReconReport `json:"models"`
	}{
		Since:  since,
		Models: report,
	})
}

// verifySignature checks the payload's HMAC-SHA256 signature. Baseten sends
// the signature in the X-Baseten-Signature header as one or more
// comma-separated values of the form v1=<hex>; the bare hex form is also
// accepted. Comparison is constant time.
func verifySignature(secret string, body []byte, hdr http.Header) bool {
	header := hdr.Get("X-Baseten-Signature")
	if header == "" {
		header = hdr.Get("Baseten-Signature")
	}
	if header == "" {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)

	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "v1=")

		got, err := hex.DecodeString(part)
		if err != nil {
			continue
		}

		if hmac.Equal(got, want) {
			return true
		}
	}

	return false
}

// parseEvent extracts the billing fields from a payload on a best-effort
// basis. Unknown shapes still produce an event carrying the raw payload.
func parseEvent(body []byte) store.ProviderEvent {
	ev := store.ProviderEvent{Raw: body}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ev
	}

	ev.EventID = firstString(payload, "event_id", "id")
	ev.Model = firstString(payload, "model", "model_id")
	ev.AmountUSD = firstNumber(
		payload, "amount_usd", "cost_usd", "amount",
	)

	// Token counts commonly live in a nested usage object, but some
	// payloads inline them at the top level.
	counts := payload
	if usage, ok := payload["usage"].(map[string]any); ok {
		counts = usage
	}

	ev.InputTokens = int64(firstNumber(
		counts, "prompt_tokens", "input_tokens",
	))
	ev.OutputTokens = int64(firstNumber(
		counts, "completion_tokens", "output_tokens",
	))
	ev.TotalTokens = int64(firstNumber(counts, "total_tokens"))
	if ev.TotalTokens == 0 {
		ev.TotalTokens = ev.InputTokens + ev.OutputTokens
	}

	return ev
}

// firstString returns the first of the given keys holding a non-empty
// string.
func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := m[key].(string); ok && s != "" {
			return s
		}
	}

	return ""
}

// firstNumber returns the first of the given keys holding a number.
func firstNumber(m map[string]any, keys ...string) float64 {
	for _, key := range keys {
		if f, ok := m[key].(float64); ok {
			return f
		}
	}

	return 0
}
