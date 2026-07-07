package pricing

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMsatPerToken checks the USD to msat conversion math.
func TestMsatPerToken(t *testing.T) {
	// $0.60 per MTok at $100k per BTC with no margin: 0.6 * 1e5 / 1e5
	// = 0.6 msat, rounded up to 1.
	require.EqualValues(t, 1, MsatPerToken(0.60, 0, 100_000))

	// $2.00 per MTok at $100k per BTC: exactly 2 msat.
	require.EqualValues(t, 2, MsatPerToken(2.00, 0, 100_000))

	// A 25% margin on $2.00: 2.5 msat, rounded up to 3.
	require.EqualValues(t, 3, MsatPerToken(2.00, 25, 100_000))

	// A tiny but positive USD price never converts to free.
	require.EqualValues(t, 1, MsatPerToken(0.0001, 0, 100_000))

	// Zero or negative inputs price at zero.
	require.EqualValues(t, 0, MsatPerToken(0, 25, 100_000))
	require.EqualValues(t, 0, MsatPerToken(1, 25, 0))
}

// fakeLister returns a fixed model list, or an error.
type fakeLister struct {
	models []string
	err    error
}

func (f *fakeLister) ListModels(context.Context) ([]string, error) {
	return f.models, f.err
}

// testTableConfig returns a config with one explicit model and default USD
// rates for discovered models.
func testTableConfig(lister ModelLister) TableConfig {
	return TableConfig{
		MarginPercent: 25,
		Models: map[string]*ModelUSD{
			"glm": {
				InputUSDPerMTok:  2.00,
				OutputUSDPerMTok: 4.00,
				BundleTokens:     500_000,
			},
		},
		DefaultUSD: &ModelUSD{
			InputUSDPerMTok:  1.00,
			OutputUSDPerMTok: 2.00,
		},
		DefaultModel: "glm",
		BundleTokens: 1_000_000,
		BTCUSD:       StaticBTCUSD(100_000),
		Lister:       lister,
	}
}

// TestTableResolveAndBundles checks model resolution, per-model bundle
// overrides and the default-model fallback.
func TestTableResolveAndBundles(t *testing.T) {
	lister := &fakeLister{models: []string{"glm", "new-model"}}
	table, err := NewTable(context.Background(), testTableConfig(lister))
	require.NoError(t, err)

	// The explicit model prices at its configured rates with margin:
	// input 2.00 * 1.25 = 2.5 msat -> 3.
	model, rates, err := table.ResolveModel("glm")
	require.NoError(t, err)
	require.Equal(t, "glm", model)
	require.EqualValues(t, 3, rates.InputMsatPerToken)
	require.EqualValues(t, 5, rates.OutputMsatPerToken)

	// The discovered model prices at the default USD rates: input
	// 1.00 * 1.25 = 1.25 msat -> 2.
	model, rates, err = table.ResolveModel("new-model")
	require.NoError(t, err)
	require.Equal(t, "new-model", model)
	require.EqualValues(t, 2, rates.InputMsatPerToken)

	// An unknown model fails closed, per the RateSource contract, while
	// a request naming no model falls back to the default.
	_, _, err = table.ResolveModel("bogus")
	require.ErrorContains(t, err, "unknown model")

	model, _, err = table.ResolveModel("")
	require.NoError(t, err)
	require.Equal(t, "glm", model)

	// Per-model bundle override vs the table-wide default.
	require.EqualValues(t, 500_000, table.BundleTokens("glm"))
	require.EqualValues(t, 1_000_000, table.BundleTokens("new-model"))
	require.EqualValues(t, 1_000_000, table.BundleTokens("bogus"))
}

// TestTableDiscoveryFailure checks that a failing lister does not fail the
// table: the configured models still price.
func TestTableDiscoveryFailure(t *testing.T) {
	lister := &fakeLister{err: fmt.Errorf("upstream down")}
	table, err := NewTable(context.Background(), testTableConfig(lister))
	require.NoError(t, err)

	_, _, err = table.ResolveModel("glm")
	require.NoError(t, err)

	// The discovered-only model is absent, so it fails closed rather
	// than riding the default model's rates.
	_, _, err = table.ResolveModel("new-model")
	require.ErrorContains(t, err, "unknown model")
}

// TestTableRequiresBTCUSD checks that a table cannot be built without an
// initial BTC-USD rate.
func TestTableRequiresBTCUSD(t *testing.T) {
	cfg := testTableConfig(nil)
	cfg.BTCUSD = StaticBTCUSD(0)

	_, err := NewTable(context.Background(), cfg)
	require.Error(t, err)
}

// TestMsatPerTokenUnusableInputs checks that NaN, infinite, and
// out-of-range inputs never produce a price. Zero means "no price", and
// the refresh turns that into an error for positively priced models.
func TestMsatPerTokenUnusableInputs(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)

	require.EqualValues(t, 0, MsatPerToken(2.0, 0, nan))
	require.EqualValues(t, 0, MsatPerToken(2.0, 0, inf))
	require.EqualValues(t, 0, MsatPerToken(nan, 0, 100_000))
	require.EqualValues(t, 0, MsatPerToken(inf, 25, 100_000))
	require.EqualValues(t, 0, MsatPerToken(2.0, nan, 100_000))

	// A conversion that would exceed the int64 range yields no price
	// instead of a platform-defined wrap.
	require.EqualValues(t, 0, MsatPerToken(1e300, 0, 1e-300))
}

// TestTableRejectsUnusableRates checks that a snapshot with an unusable
// or overflowing conversion is rejected wholesale.
func TestTableRejectsUnusableRates(t *testing.T) {
	// A NaN static rate fails the initial snapshot.
	cfg := testTableConfig(nil)
	cfg.BTCUSD = StaticBTCUSD(math.NaN())
	_, err := NewTable(context.Background(), cfg)
	require.Error(t, err)

	// An overflowing bundle price fails the snapshot too: a huge USD
	// rate against a tiny BTC-USD rate pushes the per-token msat rate
	// times the bundle size beyond the int64 guard ceiling.
	cfg = testTableConfig(nil)
	cfg.BTCUSD = StaticBTCUSD(0.000001)
	cfg.Models["glm"].InputUSDPerMTok = 1_000_000
	_, err = NewTable(context.Background(), cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "overflows")
}

// TestTableKeepsVanishedModels checks that a model that disappears from
// the upstream listing keeps its last-known entry, so outstanding paid
// bundles booked against it still resolve.
func TestTableKeepsVanishedModels(t *testing.T) {
	lister := &fakeLister{models: []string{"glm", "new-model"}}
	table, err := NewTable(context.Background(), testTableConfig(lister))
	require.NoError(t, err)

	model, _, err := table.ResolveModel("new-model")
	require.NoError(t, err)
	require.Equal(t, "new-model", model)

	// The upstream listing shrinks; the vanished model still resolves
	// to itself at its last-known rates after the refresh.
	lister.models = []string{"glm"}
	require.NoError(t, table.refresh(context.Background()))

	model, rates, err := table.ResolveModel("new-model")
	require.NoError(t, err)
	require.Equal(t, "new-model", model)
	require.EqualValues(t, 2, rates.InputMsatPerToken)
}
