package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/lightninglabs/aperture/meterd"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// ledgerRow is one usage_events row with its ledger columns.
type ledgerRow struct {
	tokens, remaining          int64
	source                     sql.NullString
	prompt, cached, completion sql.NullInt64
	total                      sql.NullInt64
	inRate, outRate, permille  sql.NullInt64
}

// ledgerRows reads the token's usage_events rows in order.
func ledgerRows(t *testing.T, s *SQLiteStore, tokenID string) []ledgerRow {
	t.Helper()

	rows, err := s.db.Query(`
		SELECT tokens, remaining_tokens, source, prompt_tokens,
			cached_tokens, completion_tokens, total_tokens,
			input_msat_per_ktok, output_msat_per_ktok,
			cached_input_permille
		FROM usage_events WHERE token_id = ? ORDER BY id`, tokenID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()

	var out []ledgerRow
	for rows.Next() {
		var r ledgerRow
		require.NoError(t, rows.Scan(
			&r.tokens, &r.remaining, &r.source, &r.prompt,
			&r.cached, &r.completion, &r.total, &r.inRate,
			&r.outRate, &r.permille,
		))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())

	return out
}

// TestUsageLedger checks that debits store the usage behind them and the
// balance after them.
func TestUsageLedger(t *testing.T) {
	s := newTestStore(t)

	_, err := s.Book("token", "glm", 10000, 1500)
	require.NoError(t, err)

	after, found, err := s.DebitUsage("token", 0, meterd.UsageRecord{
		Tokens:                      587,
		Source:                      meterd.UsageSplit,
		PromptTokens:                1000,
		CachedTokens:                800,
		CompletionTokens:            300,
		Weighted:                    true,
		InputMsatPerThousandTokens:  1000000,
		OutputMsatPerThousandTokens: 2000000,
		CachedInputPermille:         100,
	})
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 9413, after.RemainingTokens)

	_, _, err = s.DebitUsage("token", 0, meterd.UsageRecord{
		Tokens:      42,
		Source:      meterd.UsageTotal,
		TotalTokens: 42,
	})
	require.NoError(t, err)

	_, _, err = s.DebitUsage("token", 0, meterd.UsageRecord{
		Tokens: 100,
		Source: meterd.UsageEstimate,
	})
	require.NoError(t, err)

	// A plain debit still records the balance after it.
	_, _, err = s.Debit("token", 71, 0)
	require.NoError(t, err)

	// A split whose rates did not resolve keeps the split without rates.
	_, _, err = s.DebitUsage("token", 0, meterd.UsageRecord{
		Tokens:           5,
		Source:           meterd.UsageSplit,
		PromptTokens:     3,
		CompletionTokens: 2,
	})
	require.NoError(t, err)

	// An empty debit writes no row, as before.
	_, _, err = s.DebitUsage("token", 0, meterd.UsageRecord{
		Source: meterd.UsageEstimate,
	})
	require.NoError(t, err)

	rows := ledgerRows(t, s, "token")
	require.Len(t, rows, 5)

	split := rows[0]
	require.EqualValues(t, 587, split.tokens)
	require.EqualValues(t, 9413, split.remaining)
	require.Equal(t, "split", split.source.String)
	require.EqualValues(t, 1000, split.prompt.Int64)
	require.EqualValues(t, 800, split.cached.Int64)
	require.EqualValues(t, 300, split.completion.Int64)
	require.False(t, split.total.Valid)
	require.EqualValues(t, 1000000, split.inRate.Int64)
	require.EqualValues(t, 2000000, split.outRate.Int64)
	require.EqualValues(t, 100, split.permille.Int64)

	total := rows[1]
	require.Equal(t, "total", total.source.String)
	require.EqualValues(t, 42, total.total.Int64)
	require.False(t, total.prompt.Valid)
	require.EqualValues(t, 9371, total.remaining)

	estimate := rows[2]
	require.Equal(t, "estimate", estimate.source.String)
	require.False(t, estimate.prompt.Valid)
	require.False(t, estimate.total.Valid)

	plain := rows[3]
	require.False(t, plain.source.Valid)
	require.EqualValues(t, 71, plain.tokens)
	require.EqualValues(t, 9200, plain.remaining)

	unweighted := rows[4]
	require.Equal(t, "split", unweighted.source.String)
	require.EqualValues(t, 3, unweighted.prompt.Int64)
	require.False(t, unweighted.inRate.Valid)
	require.EqualValues(t, 9195, unweighted.remaining)

	// The ledger and the reconciliation report agree on the total.
	report, err := s.ReconReport(time.Unix(0, 0))
	require.NoError(t, err)
	require.EqualValues(t, 587+42+100+71+5, report["glm"].MeteredTokens)
}

// TestRecordPayment checks that a token's first payment binding wins and
// survives a restart.
func TestRecordPayment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := NewSQLiteStore(StoreConfig{Path: path})
	require.NoError(t, err)

	first := lntypes.Hash{1}
	second := lntypes.Hash{2}

	_, ok := s.PaymentHash("token")
	require.False(t, ok)

	require.NoError(t, s.RecordPayment("token", first))
	require.NoError(t, s.RecordPayment("token", second))

	got, ok := s.PaymentHash("token")
	require.True(t, ok)
	require.Equal(t, first, got)
	require.NoError(t, s.Close())

	// After a restart the in-memory cache is empty, and the database
	// still keeps the first binding.
	s, err = NewSQLiteStore(StoreConfig{Path: path})
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	require.NoError(t, s.RecordPayment("token", second))
	got, ok = s.PaymentHash("token")
	require.True(t, ok)
	require.Equal(t, first, got)
}

// TestMigrateLedgerColumns opens a database created by a release without the
// ledger columns and checks that its rows survive the migration.
func TestMigrateLedgerColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	old, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = old.Exec(`
		CREATE TABLE usage_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			token_id TEXT NOT NULL,
			model TEXT NOT NULL,
			tokens INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		);
		INSERT INTO usage_events (token_id, model, tokens, created_at)
		VALUES ('token', 'glm', 91, 1);`)
	require.NoError(t, err)
	require.NoError(t, old.Close())

	// Opening twice proves the migration is idempotent.
	for range 2 {
		s, err := NewSQLiteStore(StoreConfig{Path: path})
		require.NoError(t, err)
		require.NoError(t, s.Close())
	}

	s, err := NewSQLiteStore(StoreConfig{Path: path})
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	var (
		tokens    int64
		remaining sql.NullInt64
		source    sql.NullString
	)
	err = s.db.QueryRow(`
		SELECT tokens, remaining_tokens, source FROM usage_events
		WHERE token_id = 'token'`).Scan(&tokens, &remaining, &source)
	require.NoError(t, err)
	require.EqualValues(t, 91, tokens)
	require.False(t, remaining.Valid)
	require.False(t, source.Valid)
}
