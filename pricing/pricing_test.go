package pricing

import (
	"context"
	"fmt"
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

	// An unknown model falls back to the default model.
	model, _, err = table.ResolveModel("bogus")
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

	// The discovered-only model is absent, so it resolves to the
	// default model rather than itself.
	model, _, err := table.ResolveModel("new-model")
	require.NoError(t, err)
	require.Equal(t, "glm", model)
}

// TestTableRequiresBTCUSD checks that a table cannot be built without an
// initial BTC-USD rate.
func TestTableRequiresBTCUSD(t *testing.T) {
	cfg := testTableConfig(nil)
	cfg.BTCUSD = StaticBTCUSD(0)

	_, err := NewTable(context.Background(), cfg)
	require.Error(t, err)
}
