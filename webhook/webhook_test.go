package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lightninglabs/lightmeter/store"
	"github.com/stretchr/testify/require"
)

// sign computes the hex HMAC-SHA256 signature of the body.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)

	return hex.EncodeToString(mac.Sum(nil))
}

// TestVerifySignature checks the accepted signature header forms.
func TestVerifySignature(t *testing.T) {
	secret := "hunter2"
	body := []byte(`{"hello":"world"}`)
	sig := sign(secret, body)

	hdr := func(value string) http.Header {
		h := http.Header{}
		h.Set("X-Baseten-Signature", value)

		return h
	}

	// The v1= prefixed form, the bare form, and a multi-signature
	// header all verify.
	require.True(t, verifySignature(secret, body, hdr("v1="+sig)))
	require.True(t, verifySignature(secret, body, hdr(sig)))
	require.True(t, verifySignature(
		secret, body, hdr("v1=deadbeef, v1="+sig),
	))

	// Wrong signature, wrong secret, and a missing header all fail.
	require.False(t, verifySignature(
		secret, body, hdr("v1="+sign("wrong", body)),
	))
	require.False(t, verifySignature(secret, body, http.Header{}))
	require.False(t, verifySignature(secret, body, hdr("not-hex")))
}

// TestParseEvent checks the tolerant payload field extraction.
func TestParseEvent(t *testing.T) {
	// An OpenAI-style usage object.
	ev := parseEvent([]byte(`{
		"id": "ev-1",
		"model": "glm",
		"amount_usd": 1.25,
		"usage": {
			"prompt_tokens": 100,
			"completion_tokens": 50
		}
	}`))
	require.Equal(t, "ev-1", ev.EventID)
	require.Equal(t, "glm", ev.Model)
	require.EqualValues(t, 100, ev.InputTokens)
	require.EqualValues(t, 50, ev.OutputTokens)
	require.EqualValues(t, 150, ev.TotalTokens)
	require.InDelta(t, 1.25, ev.AmountUSD, 1e-9)

	// Top-level token counts and alternate key names.
	ev = parseEvent([]byte(`{
		"event_id": "ev-2",
		"model_id": "other",
		"input_tokens": 10,
		"output_tokens": 5,
		"cost_usd": 0.5
	}`))
	require.Equal(t, "ev-2", ev.EventID)
	require.Equal(t, "other", ev.Model)
	require.EqualValues(t, 15, ev.TotalTokens)
	require.InDelta(t, 0.5, ev.AmountUSD, 1e-9)

	// A non-JSON payload still yields an event with the raw bytes.
	raw := []byte("not json")
	ev = parseEvent(raw)
	require.Equal(t, raw, ev.Raw)
	require.Empty(t, ev.EventID)
}

// recordingSink captures ingested events.
type recordingSink struct {
	events []store.ProviderEvent
}

func (r *recordingSink) RecordProviderEvent(ev store.ProviderEvent) error {
	r.events = append(r.events, ev)

	return nil
}

// TestHandleWebhook exercises the receiver end to end via httptest.
func TestHandleWebhook(t *testing.T) {
	sink := &recordingSink{}
	secret := "hunter2"

	srv, err := NewServer(Config{
		ListenAddr: "127.0.0.1:0",
		Secret:     secret,
		Path:       "/webhooks/baseten",
		Sink:       sink,
	})
	require.NoError(t, err)

	body := []byte(`{"id":"ev-1","model":"glm","usage":` +
		`{"total_tokens":42}}`)

	post := func(sig string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(
			http.MethodPost, "/webhooks/baseten",
			bytes.NewReader(body),
		)
		if sig != "" {
			req.Header.Set("X-Baseten-Signature", sig)
		}
		w := httptest.NewRecorder()
		srv.srv.Handler.ServeHTTP(w, req)

		return w
	}

	// A signed payload is ingested.
	w := post("v1=" + sign(secret, body))
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Len(t, sink.events, 1)
	require.Equal(t, "ev-1", sink.events[0].EventID)
	require.EqualValues(t, 42, sink.events[0].TotalTokens)

	// Unsigned and mis-signed payloads are rejected.
	require.Equal(t, http.StatusUnauthorized, post("").Code)
	require.Equal(
		t, http.StatusUnauthorized,
		post("v1="+sign("wrong", body)).Code,
	)
	require.Len(t, sink.events, 1)
}

// staticReporter serves a fixed report.
type staticReporter struct {
	report store.ReconReport
}

func (s *staticReporter) ReconReport(time.Time) (store.ReconReport, error) {
	return s.report, nil
}

// TestHandleRecon checks the report endpoint, including its bearer-token
// gate.
func TestHandleRecon(t *testing.T) {
	srv, err := NewServer(Config{
		ListenAddr: "127.0.0.1:0",
		Secret:     "hunter2",
		Path:       "/webhooks/baseten",
		ReconToken: "recon-token",
		Sink:       &recordingSink{},
		Reporter: &staticReporter{
			report: store.ReconReport{
				"glm": &store.ReconRow{
					MeteredTokens:  750,
					ProviderTokens: 700,
					DeltaTokens:    50,
				},
			},
		},
	})
	require.NoError(t, err)

	get := func(target, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		srv.srv.Handler.ServeHTTP(w, req)

		return w
	}

	// The right token reads the report.
	w := get("/recon", "recon-token")
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"metered_tokens":750`)
	require.Contains(t, w.Body.String(), `"delta_tokens":50`)

	// A missing or wrong token is rejected.
	require.Equal(t, http.StatusUnauthorized, get("/recon", "").Code)
	require.Equal(
		t, http.StatusUnauthorized, get("/recon", "wrong").Code,
	)

	// A malformed since parameter is a client error.
	w = get("/recon?since=yesterday", "recon-token")
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// TestReconDisabledWithoutToken checks that the report endpoint does not
// exist at all when no token is configured.
func TestReconDisabledWithoutToken(t *testing.T) {
	srv, err := NewServer(Config{
		ListenAddr: "127.0.0.1:0",
		Secret:     "hunter2",
		Path:       "/webhooks/baseten",
		Sink:       &recordingSink{},
		Reporter:   &staticReporter{},
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/recon", nil)
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)
}

// TestNewServerPathValidation checks that a bad mount path fails cleanly
// instead of panicking inside ServeMux registration.
func TestNewServerPathValidation(t *testing.T) {
	_, err := NewServer(Config{
		ListenAddr: "127.0.0.1:0",
		Secret:     "hunter2",
		Path:       "webhooks-no-slash",
		Sink:       &recordingSink{},
	})
	require.Error(t, err)

	// An empty path falls back to the default rather than panicking.
	srv, err := NewServer(Config{
		ListenAddr: "127.0.0.1:0",
		Secret:     "hunter2",
		Sink:       &recordingSink{},
	})
	require.NoError(t, err)
	require.Equal(t, "/webhooks/baseten", srv.cfg.Path)
}

// TestSynthesizedEventID checks that a payload without an upstream event
// identifier gets a deterministic content-hash ID, so redelivered copies
// dedupe in the store.
func TestSynthesizedEventID(t *testing.T) {
	sink := &recordingSink{}
	secret := "hunter2"

	srv, err := NewServer(Config{
		ListenAddr: "127.0.0.1:0",
		Secret:     secret,
		Path:       "/webhooks/baseten",
		Sink:       sink,
	})
	require.NoError(t, err)

	body := []byte(`{"model":"glm","usage":{"total_tokens":7}}`)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(
			http.MethodPost, "/webhooks/baseten",
			bytes.NewReader(body),
		)
		req.Header.Set(
			"X-Baseten-Signature", "v1="+sign(secret, body),
		)
		w := httptest.NewRecorder()
		srv.srv.Handler.ServeHTTP(w, req)
		require.Equal(t, http.StatusNoContent, w.Code)
	}

	require.Len(t, sink.events, 2)
	require.True(
		t, strings.HasPrefix(sink.events[0].EventID, "sha256:"),
	)

	// Identical payloads synthesize the identical ID, which is what
	// lets the store's unique index collapse replays.
	require.Equal(t, sink.events[0].EventID, sink.events[1].EventID)
}
