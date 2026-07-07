// Package pricing turns the upstream provider's USD prices into the msat
// rate table quoted to L402 clients. The Table implements meterd.RateSource:
// meterd owns the metering mechanics, this package owns the economics.
package pricing

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/lightninglabs/aperture/meterd"
)

// ModelUSD holds the seller-side USD economics of a single model.
type ModelUSD struct {
	// InputUSDPerMTok is the upstream USD price per million input
	// tokens.
	InputUSDPerMTok float64

	// OutputUSDPerMTok is the upstream USD price per million output
	// tokens.
	OutputUSDPerMTok float64

	// BundleTokens overrides the table-wide bundle size for this model
	// when positive.
	BundleTokens int64
}

// TableConfig configures a Table.
type TableConfig struct {
	// MarginPercent is the markup applied on top of the upstream USD
	// price, in percent.
	MarginPercent float64

	// Models maps a model identifier to its USD pricing. These entries
	// are authoritative and survive upstream refreshes.
	Models map[string]*ModelUSD

	// DefaultUSD prices models discovered from the upstream model list
	// that have no explicit entry in Models. When nil, discovered
	// models without an entry are not sellable.
	DefaultUSD *ModelUSD

	// DefaultModel is the model used to price requests whose model is
	// missing or unknown.
	DefaultModel string

	// BundleTokens is the table-wide number of tokens sold per bundle.
	BundleTokens int64

	// BTCUSD supplies the BTC-USD rate. Required.
	BTCUSD BTCUSDSource

	// Lister supplies the upstream model list. Discovery is disabled
	// when nil, in which case only the configured Models are sellable.
	Lister ModelLister

	// RefreshInterval is how often the table refreshes the rate and the
	// model list. Refreshing is disabled when zero; the initial
	// snapshot then serves forever.
	RefreshInterval time.Duration

	// Logger receives refresh diagnostics. A default logger is used
	// when nil.
	Logger *slog.Logger
}

// modelEntry is one row of the computed rate table snapshot.
type modelEntry struct {
	rates        *meterd.ModelConfig
	bundleTokens int64
}

// Table is a refreshing rate table that satisfies meterd.RateSource. It
// converts the configured USD prices into msat-per-token rates at the
// current BTC-USD rate with the configured margin applied, and folds in the
// models discovered from the upstream provider.
type Table struct {
	cfg TableConfig
	log *slog.Logger

	mtx     sync.RWMutex
	entries map[string]*modelEntry

	quit chan struct{}
	wg   sync.WaitGroup
}

// A compile-time check that Table satisfies meterd.RateSource.
var _ meterd.RateSource = (*Table)(nil)

// NewTable builds a Table and computes its initial snapshot. The initial
// BTC-USD read must succeed, since a daemon with no rate table cannot quote
// anything.
func NewTable(ctx context.Context, cfg TableConfig) (*Table, error) {
	if cfg.BTCUSD == nil {
		return nil, fmt.Errorf("a BTC-USD source is required")
	}
	if cfg.BundleTokens <= 0 {
		return nil, fmt.Errorf("bundle tokens must be positive, "+
			"got %d", cfg.BundleTokens)
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	t := &Table{
		cfg:  cfg,
		log:  logger,
		quit: make(chan struct{}),
	}

	if err := t.refresh(ctx); err != nil {
		return nil, fmt.Errorf("unable to compute the initial rate "+
			"table: %w", err)
	}

	return t, nil
}

// Start launches the periodic refresher when a refresh interval is
// configured.
func (t *Table) Start() {
	if t.cfg.RefreshInterval <= 0 {
		return
	}

	t.wg.Add(1)
	go t.refresher()
}

// Stop shuts the refresher down.
func (t *Table) Stop() {
	close(t.quit)
	t.wg.Wait()
}

// refresher periodically recomputes the rate table. A failed refresh keeps
// the previous snapshot, so a flaky upstream degrades to stale prices
// rather than an outage.
func (t *Table) refresher() {
	defer t.wg.Done()

	ticker := time.NewTicker(t.cfg.RefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(
				context.Background(), time.Minute,
			)
			if err := t.refresh(ctx); err != nil {
				t.log.Error("Rate table refresh failed, "+
					"keeping the previous snapshot",
					"err", err)
			}
			cancel()

		case <-t.quit:
			return
		}
	}
}

// refresh recomputes the full rate table snapshot: read the BTC-USD rate,
// list the upstream models when discovery is enabled, and convert every
// priced model into msat rates.
func (t *Table) refresh(ctx context.Context) error {
	btcUSD, err := t.cfg.BTCUSD.Rate(ctx)
	if err != nil {
		return fmt.Errorf("unable to read the BTC-USD rate: %w", err)
	}

	// The configured models are authoritative. Discovered models fill
	// in around them at the default USD rates.
	usd := make(map[string]*ModelUSD, len(t.cfg.Models))
	for name, m := range t.cfg.Models {
		usd[name] = m
	}

	if t.cfg.Lister != nil {
		discovered, err := t.cfg.Lister.ListModels(ctx)
		switch {
		case err != nil:
			// Discovery failing is not fatal: the configured
			// models still price fine.
			t.log.Warn("Upstream model discovery failed",
				"err", err)

		case t.cfg.DefaultUSD == nil:
			// Without default USD rates there is nothing to
			// price discovered models at.

		default:
			for _, name := range discovered {
				if _, ok := usd[name]; !ok {
					usd[name] = t.cfg.DefaultUSD
				}
			}
		}
	}

	entries := make(map[string]*modelEntry, len(usd))
	for name, m := range usd {
		bundleTokens := t.cfg.BundleTokens
		if m.BundleTokens > 0 {
			bundleTokens = m.BundleTokens
		}

		entries[name] = &modelEntry{
			rates: &meterd.ModelConfig{
				InputMsatPerToken: MsatPerToken(
					m.InputUSDPerMTok,
					t.cfg.MarginPercent, btcUSD,
				),
				OutputMsatPerToken: MsatPerToken(
					m.OutputUSDPerMTok,
					t.cfg.MarginPercent, btcUSD,
				),
			},
			bundleTokens: bundleTokens,
		}
	}

	t.mtx.Lock()
	t.entries = entries
	t.mtx.Unlock()

	t.log.Info("Rate table refreshed", "models", len(entries),
		"btc_usd", btcUSD)

	return nil
}

// ResolveModel maps a possibly empty or unknown model identifier to a
// priced model and its current rates, falling back to the default model.
func (t *Table) ResolveModel(model string) (string, *meterd.ModelConfig,
	error) {

	t.mtx.RLock()
	defer t.mtx.RUnlock()

	if model != "" {
		if entry, ok := t.entries[model]; ok {
			return model, entry.rates, nil
		}
	}

	if t.cfg.DefaultModel != "" {
		if entry, ok := t.entries[t.cfg.DefaultModel]; ok {
			return t.cfg.DefaultModel, entry.rates, nil
		}
	}

	return "", nil, fmt.Errorf("unknown model %q and no default model "+
		"configured", model)
}

// BundleTokens returns the bundle size sold for the given model, falling
// back to the table-wide size for unknown models.
func (t *Table) BundleTokens(model string) int64 {
	t.mtx.RLock()
	defer t.mtx.RUnlock()

	if entry, ok := t.entries[model]; ok {
		return entry.bundleTokens
	}

	return t.cfg.BundleTokens
}

// MsatPerToken converts an upstream USD-per-million-token price into a
// millisatoshi-per-token rate at the given BTC-USD rate, with the margin
// applied. The result rounds up, and any positive USD price yields at least
// one msat so a real cost never converts to free:
//
//	msat = usdPerMTok / 1e6 [USD/tok] / btcUSD [BTC/USD] * 1e8 [sat/BTC]
//	       * 1e3 [msat/sat] * (1 + margin/100)
//	     = usdPerMTok * 1e5 * (1 + margin/100) / btcUSD.
func MsatPerToken(usdPerMTok, marginPercent, btcUSD float64) int64 {
	if usdPerMTok <= 0 || btcUSD <= 0 {
		return 0
	}

	msat := usdPerMTok * 100_000 * (1 + marginPercent/100) / btcUSD

	n := int64(math.Ceil(msat))
	if n < 1 {
		n = 1
	}

	return n
}
