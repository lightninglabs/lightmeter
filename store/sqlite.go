// Package store implements meterd.Store on sqlite, adding the durable
// usage ledger and the provider-event table the reconciliation report is
// computed from.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lightninglabs/aperture/meterd"

	// Register the pure-Go sqlite driver.
	_ "modernc.org/sqlite"
)

// schema is the full database schema. Every statement is idempotent so the
// daemon can apply it on every start.
const schema = `
CREATE TABLE IF NOT EXISTS bundles (
	token_id TEXT PRIMARY KEY,
	model TEXT NOT NULL,
	price_sats INTEGER NOT NULL,
	remaining_tokens INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	authorized INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS bundles_unauthorized_idx
	ON bundles (authorized, created_at);

CREATE TABLE IF NOT EXISTS usage_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	token_id TEXT NOT NULL,
	model TEXT NOT NULL,
	tokens INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS usage_events_created_idx
	ON usage_events (created_at);

CREATE TABLE IF NOT EXISTS provider_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	event_id TEXT UNIQUE,
	model TEXT,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens INTEGER NOT NULL DEFAULT 0,
	amount_usd REAL NOT NULL DEFAULT 0,
	received_at INTEGER NOT NULL,
	raw TEXT NOT NULL
);
`

// ProviderEvent is one upstream billing event ingested through the webhook
// receiver.
type ProviderEvent struct {
	// EventID is the upstream event identifier, used to dedupe webhook
	// redeliveries. Events without an identifier are always stored.
	EventID string

	// Model is the model the event bills for, when the payload names
	// one.
	Model string

	// InputTokens and OutputTokens are the billed token counts, when
	// the payload splits them.
	InputTokens  int64
	OutputTokens int64

	// TotalTokens is the billed total token count.
	TotalTokens int64

	// AmountUSD is the billed USD amount, when the payload carries one.
	AmountUSD float64

	// Raw is the verbatim payload, retained for audit.
	Raw []byte
}

// ReconRow compares the tokens this daemon debited against the tokens the
// upstream provider billed, for one model.
type ReconRow struct {
	// MeteredTokens is the token count debited from client bundles.
	MeteredTokens int64 `json:"metered_tokens"`

	// ProviderTokens is the token count the provider billed.
	ProviderTokens int64 `json:"provider_tokens"`

	// DeltaTokens is metered minus provider. A large negative value
	// means the provider billed usage this daemon never charged for.
	DeltaTokens int64 `json:"delta_tokens"`
}

// ReconReport maps a model to its reconciliation row.
type ReconReport map[string]*ReconRow

// maxReservationAge bounds how long an authorization reservation is held
// before it expires on its own. The pricesrpc proto does not correlate a
// usage report with the authorization that preceded it, so the release on
// debit is approximate; without an age bound, the residue of mismatched
// estimates accumulates until a paid bundle reads as exhausted. No real
// inference request outlives this bound.
const maxReservationAge = 10 * time.Minute

// reservation is one authorization's estimated token cost, held until the
// matching usage report releases it or it ages out.
type reservation struct {
	amount int64
	at     time.Time
}

// StoreConfig configures a SQLiteStore.
type StoreConfig struct {
	// Path is the sqlite database file to open, created when missing.
	Path string

	// MaxUnauthorized caps the number of never-authorized (un-paid)
	// bundles retained at once. A non-positive value disables the cap.
	MaxUnauthorized int

	// MinEvictionAge is the age below which a never-authorized bundle is
	// immune to count-based eviction. Payment happens at the proxy, so a
	// just-paid bundle looks un-paid until its first authorized request;
	// the floor keeps a mint-spam burst from evicting it in that window.
	// A non-positive value disables the floor.
	MinEvictionAge time.Duration

	// Retention bounds how long usage and provider billing events are
	// kept before the periodic sweep prunes them. The bundles table is
	// never pruned. A non-positive value keeps events forever.
	Retention time.Duration
}

// SQLiteStore keeps bundle balances, the usage ledger and provider billing
// events in a single sqlite database. In-flight reservations are kept in
// memory only, mirroring the JSON store: they track live requests and are
// deliberately dropped on restart.
type SQLiteStore struct {
	// db is the writer handle, bounded to a single connection so the
	// metering path never contends with itself.
	db *sql.DB

	// rdb is a read-only handle used by the reconciliation report, so
	// long analytical scans never hold up the metering path. WAL mode
	// lets readers run concurrently with the writer.
	rdb *sql.DB

	mtx      sync.Mutex
	reserved map[string][]reservation

	cfg StoreConfig

	// lastPrune is when the event retention sweep last ran.
	lastPrune time.Time
}

// A compile-time check that SQLiteStore satisfies meterd.Store.
var _ meterd.Store = (*SQLiteStore)(nil)

// NewSQLiteStore opens (creating when necessary) the configured database
// and applies the schema.
func NewSQLiteStore(cfg StoreConfig) (*SQLiteStore, error) {
	path := cfg.Path
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("unable to open database %s: %w",
			path, err)
	}

	// The daemon is the sole writer; a single connection sidesteps
	// SQLITE_BUSY contention between the pool's connections.
	db.SetMaxOpenConns(1)

	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA busy_timeout = 5000;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("unable to apply %q: %w", p,
				err)
		}
	}

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("unable to apply schema: %w", err)
	}

	// A second handle serves the reconciliation report's read-only
	// scans without taking the metering mutex or the writer
	// connection.
	rdb, err := sql.Open("sqlite", path)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("unable to open reader handle for "+
			"%s: %w", path, err)
	}
	rdb.SetMaxOpenConns(1)
	if _, err := rdb.Exec("PRAGMA busy_timeout = 5000;"); err != nil {
		_ = db.Close()
		_ = rdb.Close()
		return nil, fmt.Errorf("unable to configure reader "+
			"handle: %w", err)
	}

	return &SQLiteStore{
		db:       db,
		rdb:      rdb,
		reserved: make(map[string][]reservation),
		cfg:      cfg,
	}, nil
}

// Close closes the underlying database handles.
func (s *SQLiteStore) Close() error {
	rErr := s.rdb.Close()
	if err := s.db.Close(); err != nil {
		return err
	}

	return rErr
}

// reservedLocked returns the token's total live reservation, dropping any
// entries older than maxReservationAge on the way. The caller must hold
// the mutex.
func (s *SQLiteStore) reservedLocked(tokenID string) int64 {
	entries := s.reserved[tokenID]
	if len(entries) == 0 {
		return 0
	}

	cutoff := time.Now().Add(-maxReservationAge)

	var (
		live  []reservation
		total int64
	)
	for _, r := range entries {
		if r.at.Before(cutoff) {
			continue
		}

		live = append(live, r)
		total += r.amount
	}

	if len(live) == 0 {
		delete(s.reserved, tokenID)
	} else {
		s.reserved[tokenID] = live
	}

	return total
}

// releaseLocked releases up to the given amount of the token's
// reservation, oldest entries first. The caller must hold the mutex.
func (s *SQLiteStore) releaseLocked(tokenID string, amount int64) {
	entries := s.reserved[tokenID]
	for len(entries) > 0 && amount > 0 {
		if entries[0].amount <= amount {
			amount -= entries[0].amount
			entries = entries[1:]
			continue
		}

		entries[0].amount -= amount
		amount = 0
	}

	if len(entries) == 0 {
		delete(s.reserved, tokenID)
	} else {
		s.reserved[tokenID] = entries
	}
}

// Book records a fresh bundle for the given token ID. Booking is
// idempotent: a token that already has a bundle is left untouched, and the
// boolean return value reports whether a new bundle was created.
func (s *SQLiteStore) Book(tokenID, model string, tokens,
	priceSats int64) (bool, error) {

	s.mtx.Lock()
	defer s.mtx.Unlock()

	res, err := s.db.Exec(`
		INSERT INTO bundles (
			token_id, model, price_sats, remaining_tokens,
			created_at, authorized
		) VALUES (?, ?, ?, ?, ?, 0)
		ON CONFLICT (token_id) DO NOTHING`,
		tokenID, model, priceSats, tokens, time.Now().Unix(),
	)
	if err != nil {
		return false, err
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}

	// Bound the number of never-authorized bundles so unauthenticated
	// challenge-mint spam cannot grow the table without limit.
	if err := s.evictOldestUnauthorizedLocked(); err != nil {
		return true, err
	}

	return true, nil
}

// evictOldestUnauthorizedLocked deletes never-authorized bundles beyond the
// configured cap, oldest first. Bundles younger than the eviction age floor
// still count against the cap but are never candidates, so a mint-spam
// burst cannot push out a just-paid bundle before its first authorized
// request. The caller must hold the mutex.
func (s *SQLiteStore) evictOldestUnauthorizedLocked() error {
	if s.cfg.MaxUnauthorized <= 0 {
		return nil
	}

	var total int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM bundles WHERE authorized = 0`,
	).Scan(&total)
	if err != nil {
		return err
	}

	excess := total - s.cfg.MaxUnauthorized
	if excess <= 0 {
		return nil
	}

	// Everything is evictable without a floor; with one, only bundles
	// created before the cutoff are.
	evictBefore := time.Now().Unix() + 1
	if s.cfg.MinEvictionAge > 0 {
		evictBefore = time.Now().Add(-s.cfg.MinEvictionAge).Unix()
	}

	_, err = s.db.Exec(`
		DELETE FROM bundles WHERE token_id IN (
			SELECT token_id FROM bundles
			WHERE authorized = 0 AND created_at < ?
			ORDER BY created_at ASC
			LIMIT ?
		)`,
		evictBefore, excess,
	)

	return err
}

// Authorize marks the token's bundle as used and, when the request is going
// to be allowed, reserves the given estimated per-request cost against its
// balance to bound concurrent overdraw. It returns the tokens available
// after existing reservations and whether the token is known.
func (s *SQLiteStore) Authorize(tokenID string, estimate int64) (int64, bool,
	error) {

	s.mtx.Lock()
	defer s.mtx.Unlock()

	var (
		remaining  int64
		authorized bool
	)
	err := s.db.QueryRow(`
		SELECT remaining_tokens, authorized
		FROM bundles WHERE token_id = ?`,
		tokenID,
	).Scan(&remaining, &authorized)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil

	case err != nil:
		return 0, false, err
	}

	// The available balance already nets out the tokens reserved by
	// other in-flight requests, so N concurrent requests on a
	// near-empty bundle cannot all authorize.
	available := remaining - s.reservedLocked(tokenID)

	if !authorized {
		_, err := s.db.Exec(
			`UPDATE bundles SET authorized = 1
			 WHERE token_id = ?`,
			tokenID,
		)
		if err != nil {
			return 0, false, err
		}
	}

	// Reserve the estimate only when the request is going to be
	// allowed, so a denied request does not tie up balance.
	if available > 0 && estimate > 0 {
		s.reserved[tokenID] = append(
			s.reserved[tokenID],
			reservation{amount: estimate, at: time.Now()},
		)
	}

	return available, true, nil
}

// Get returns a copy of the token's bundle.
func (s *SQLiteStore) Get(tokenID string) (meterd.Bundle, bool) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	b, err := s.getLocked(tokenID)
	if err != nil {
		return meterd.Bundle{}, false
	}

	return b, true
}

// getLocked reads the token's bundle row. The caller must hold the mutex.
func (s *SQLiteStore) getLocked(tokenID string) (meterd.Bundle, error) {
	var (
		b         meterd.Bundle
		createdAt int64
	)
	err := s.db.QueryRow(`
		SELECT model, price_sats, remaining_tokens, created_at,
			authorized
		FROM bundles WHERE token_id = ?`,
		tokenID,
	).Scan(
		&b.Model, &b.PriceSats, &b.RemainingTokens, &createdAt,
		&b.Authorized,
	)
	if err != nil {
		return meterd.Bundle{}, err
	}

	b.CreatedAt = time.Unix(createdAt, 0).UTC()
	b.Reserved = s.reservedLocked(tokenID)

	return b, nil
}

// Debit subtracts the actual tokens consumed from the token's balance,
// clamping at zero, releases the given reservation estimate taken at
// authorization time, and appends the consumption to the durable usage
// ledger the reconciliation report reads from. The balance update and the
// ledger append commit atomically, so a crash cannot debit money without
// its ledger row.
func (s *SQLiteStore) Debit(tokenID string, tokens,
	releaseEstimate int64) (meterd.Bundle, bool, error) {

	s.mtx.Lock()
	defer s.mtx.Unlock()

	// Release the reservation taken on authorization.
	if releaseEstimate > 0 {
		s.releaseLocked(tokenID, releaseEstimate)
	}

	if tokens <= 0 {
		b, err := s.getLocked(tokenID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return meterd.Bundle{}, false, nil

		case err != nil:
			return meterd.Bundle{}, false, err
		}

		return b, true, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return meterd.Bundle{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`
		UPDATE bundles
		SET remaining_tokens = MAX(0, remaining_tokens - ?)
		WHERE token_id = ?`,
		tokens, tokenID,
	)
	if err != nil {
		return meterd.Bundle{}, false, err
	}

	n, err := res.RowsAffected()
	if err != nil {
		return meterd.Bundle{}, false, err
	}
	if n == 0 {
		return meterd.Bundle{}, false, nil
	}

	var (
		b         meterd.Bundle
		createdAt int64
	)
	err = tx.QueryRow(`
		SELECT model, price_sats, remaining_tokens, created_at,
			authorized
		FROM bundles WHERE token_id = ?`,
		tokenID,
	).Scan(
		&b.Model, &b.PriceSats, &b.RemainingTokens, &createdAt,
		&b.Authorized,
	)
	if err != nil {
		return meterd.Bundle{}, false, err
	}
	b.CreatedAt = time.Unix(createdAt, 0).UTC()
	b.Reserved = s.reservedLocked(tokenID)

	// Append the consumption to the usage ledger. The ledger is what
	// the reconciliation report compares against the provider's
	// billing events.
	_, err = tx.Exec(`
		INSERT INTO usage_events (
			token_id, model, tokens, created_at
		) VALUES (?, ?, ?, ?)`,
		tokenID, b.Model, tokens, time.Now().Unix(),
	)
	if err != nil {
		return meterd.Bundle{}, false, err
	}

	if err := tx.Commit(); err != nil {
		return meterd.Bundle{}, false, err
	}

	return b, true, nil
}

// ExpireStale removes bundles that were never used by an authorized request
// within the given TTL and returns the number of removed bundles. The
// periodic janitor drives this, so it doubles as the sweep that ages out
// stale reservations across all tokens.
func (s *SQLiteStore) ExpireStale(ttl time.Duration) int {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	for tokenID := range s.reserved {
		s.reservedLocked(tokenID)
	}

	// Piggyback the event retention sweep on the janitor's cadence,
	// pruning at most once per hour. Bundle balances are never pruned;
	// only the append-only event tables are bounded.
	if s.cfg.Retention > 0 && time.Since(s.lastPrune) >= time.Hour {
		pruneBefore := time.Now().Add(-s.cfg.Retention).Unix()
		_, err := s.db.Exec(
			`DELETE FROM usage_events WHERE created_at < ?`,
			pruneBefore,
		)
		if err == nil {
			_, err = s.db.Exec(
				`DELETE FROM provider_events
				 WHERE received_at < ?`,
				pruneBefore,
			)
		}
		if err == nil {
			s.lastPrune = time.Now()
		}
	}

	cutoff := time.Now().Add(-ttl).Unix()

	res, err := s.db.Exec(`
		DELETE FROM bundles
		WHERE authorized = 0 AND created_at < ?`,
		cutoff,
	)
	if err != nil {
		return 0
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0
	}

	return int(n)
}

// Flush is a no-op: sqlite persists every statement durably on its own.
func (s *SQLiteStore) Flush() error {
	return nil
}

// RecordProviderEvent stores one upstream billing event. Events carrying an
// identifier are deduped on redelivery.
func (s *SQLiteStore) RecordProviderEvent(ev ProviderEvent) error {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	// A NULL event_id bypasses the UNIQUE constraint, so events
	// without an identifier are always stored.
	var eventID any
	if ev.EventID != "" {
		eventID = ev.EventID
	}

	_, err := s.db.Exec(`
		INSERT INTO provider_events (
			event_id, model, input_tokens, output_tokens,
			total_tokens, amount_usd, received_at, raw
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (event_id) DO NOTHING`,
		eventID, ev.Model, ev.InputTokens, ev.OutputTokens,
		ev.TotalTokens, ev.AmountUSD, time.Now().Unix(),
		string(ev.Raw),
	)

	return err
}

// ReconReport compares, per model, the tokens this daemon debited from
// client bundles against the tokens the provider billed since the given
// time. It runs on the read-only handle without the metering mutex, so an
// expensive analytical scan can never stall the payment path.
func (s *SQLiteStore) ReconReport(since time.Time) (ReconReport, error) {
	report := make(ReconReport)
	row := func(model string) *ReconRow {
		if r, ok := report[model]; ok {
			return r
		}

		r := &ReconRow{}
		report[model] = r

		return r
	}

	metered, err := s.rdb.Query(`
		SELECT model, SUM(tokens) FROM usage_events
		WHERE created_at >= ? GROUP BY model`,
		since.Unix(),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = metered.Close() }()

	for metered.Next() {
		var (
			model  string
			tokens int64
		)
		if err := metered.Scan(&model, &tokens); err != nil {
			return nil, err
		}

		row(model).MeteredTokens = tokens
	}
	if err := metered.Err(); err != nil {
		return nil, err
	}

	// A provider event without a token split still carries the total,
	// so the total column is authoritative and falls back to the sum
	// of the split when zero.
	billed, err := s.rdb.Query(`
		SELECT COALESCE(model, ''),
			SUM(MAX(total_tokens, input_tokens + output_tokens))
		FROM provider_events
		WHERE received_at >= ? GROUP BY model`,
		since.Unix(),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = billed.Close() }()

	for billed.Next() {
		var (
			model  string
			tokens int64
		)
		if err := billed.Scan(&model, &tokens); err != nil {
			return nil, err
		}

		row(model).ProviderTokens = tokens
	}
	if err := billed.Err(); err != nil {
		return nil, err
	}

	for _, r := range report {
		r.DeltaTokens = r.MeteredTokens - r.ProviderTokens
	}

	return report, nil
}
