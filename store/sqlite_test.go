package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newTestStore opens a store backed by a file in a test temp dir.
func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()

	s, err := NewSQLiteStore(
		filepath.Join(t.TempDir(), "test.db"), 0,
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	return s
}

// TestBookAuthorizeDebit walks the basic bundle lifecycle.
func TestBookAuthorizeDebit(t *testing.T) {
	s := newTestStore(t)

	// Unknown tokens are not found.
	_, found, err := s.Authorize("unknown", 0)
	require.NoError(t, err)
	require.False(t, found)

	_, found, err = s.Debit("unknown", 10, 0)
	require.NoError(t, err)
	require.False(t, found)

	// Booking is idempotent.
	booked, err := s.Book("token1", "glm", 1000, 1500)
	require.NoError(t, err)
	require.True(t, booked)

	booked, err = s.Book("token1", "other", 9999, 9)
	require.NoError(t, err)
	require.False(t, booked)

	b, ok := s.Get("token1")
	require.True(t, ok)
	require.Equal(t, "glm", b.Model)
	require.EqualValues(t, 1500, b.PriceSats)
	require.EqualValues(t, 1000, b.RemainingTokens)
	require.False(t, b.Authorized)

	// Authorization reports the balance and marks the bundle used.
	remaining, found, err := s.Authorize("token1", 0)
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 1000, remaining)

	b, ok = s.Get("token1")
	require.True(t, ok)
	require.True(t, b.Authorized)

	// Debits clamp at zero.
	after, found, err := s.Debit("token1", 600, 0)
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 400, after.RemainingTokens)

	after, found, err = s.Debit("token1", 600, 0)
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 0, after.RemainingTokens)

	remaining, found, err = s.Authorize("token1", 0)
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 0, remaining)
}

// TestReservations checks that authorization reservations bound concurrent
// overdraw and are released on debit.
func TestReservations(t *testing.T) {
	s := newTestStore(t)

	_, err := s.Book("token", "glm", 100, 1500)
	require.NoError(t, err)

	// The first request sees the full balance and reserves its
	// estimate.
	available, found, err := s.Authorize("token", 100)
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 100, available)

	// A concurrent second request sees the balance net of the first
	// reservation.
	available, found, err = s.Authorize("token", 100)
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 0, available)

	// The report releases the reservation and debits actual usage.
	after, found, err := s.Debit("token", 40, 100)
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 60, after.RemainingTokens)

	available, _, err = s.Authorize("token", 100)
	require.NoError(t, err)
	require.EqualValues(t, 60, available)
}

// TestReservationExpiry checks that a reservation whose usage report
// never arrives ages out instead of permanently shrinking the bundle's
// available balance.
func TestReservationExpiry(t *testing.T) {
	s := newTestStore(t)

	_, err := s.Book("token", "glm", 1000, 1500)
	require.NoError(t, err)

	// Reserve most of the bundle.
	available, _, err := s.Authorize("token", 900)
	require.NoError(t, err)
	require.EqualValues(t, 1000, available)

	// A second request sees the shrunken availability.
	available, _, err = s.Authorize("token", 0)
	require.NoError(t, err)
	require.EqualValues(t, 100, available)

	// Backdate the first reservation past the age bound, as if its
	// usage report was lost. The next authorization sees the full
	// balance again.
	s.mtx.Lock()
	s.reserved["token"][0].at = time.Now().Add(
		-maxReservationAge - time.Minute,
	)
	s.mtx.Unlock()

	available, _, err = s.Authorize("token", 0)
	require.NoError(t, err)
	require.EqualValues(t, 1000, available)
}

// TestPersistence checks that balances survive a store reopen while
// reservations do not.
func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	s, err := NewSQLiteStore(path, 0)
	require.NoError(t, err)

	_, err = s.Book("token", "glm", 1000, 1500)
	require.NoError(t, err)
	_, _, err = s.Authorize("token", 100)
	require.NoError(t, err)
	_, _, err = s.Debit("token", 250, 0)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	reloaded, err := NewSQLiteStore(path, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, reloaded.Close()) }()

	b, ok := reloaded.Get("token")
	require.True(t, ok)
	require.EqualValues(t, 750, b.RemainingTokens)
	require.True(t, b.Authorized)

	// The reservation was in-memory only.
	require.EqualValues(t, 0, b.Reserved)
}

// TestExpireStale checks that only never-authorized bundles are reaped.
func TestExpireStale(t *testing.T) {
	s := newTestStore(t)

	_, err := s.Book("stale", "glm", 1000, 1500)
	require.NoError(t, err)
	_, err = s.Book("used", "glm", 1000, 1500)
	require.NoError(t, err)
	_, _, err = s.Authorize("used", 0)
	require.NoError(t, err)

	// Nothing is younger than a positive TTL.
	require.Equal(t, 0, s.ExpireStale(time.Hour))

	// A negative TTL puts the cutoff in the future, so the
	// never-authorized bundle is reaped and the used one survives.
	require.Equal(t, 1, s.ExpireStale(-time.Hour))

	_, ok := s.Get("stale")
	require.False(t, ok)
	_, ok = s.Get("used")
	require.True(t, ok)
}

// TestUnauthorizedEviction checks the cap on never-authorized bundles.
func TestUnauthorizedEviction(t *testing.T) {
	s, err := NewSQLiteStore(
		filepath.Join(t.TempDir(), "test.db"), 2,
	)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	// An authorized bundle never counts against the cap.
	_, err = s.Book("paid", "glm", 1000, 1500)
	require.NoError(t, err)
	_, _, err = s.Authorize("paid", 0)
	require.NoError(t, err)

	for i := 0; i < 4; i++ {
		_, err = s.Book(
			"spam"+string(rune('a'+i)), "glm", 1000, 1500,
		)
		require.NoError(t, err)
	}

	// Only the cap's worth of un-authorized bundles survive, plus the
	// authorized one.
	var unauthorized int
	for _, id := range []string{"spama", "spamb", "spamc", "spamd"} {
		if _, ok := s.Get(id); ok {
			unauthorized++
		}
	}
	require.Equal(t, 2, unauthorized)

	_, ok := s.Get("paid")
	require.True(t, ok)
}

// TestReconReport checks the usage ledger against ingested provider
// events, including redelivery dedupe.
func TestReconReport(t *testing.T) {
	s := newTestStore(t)

	_, err := s.Book("token", "glm", 1_000_000, 1500)
	require.NoError(t, err)
	_, _, err = s.Debit("token", 500, 0)
	require.NoError(t, err)
	_, _, err = s.Debit("token", 250, 0)
	require.NoError(t, err)

	// Two provider events, one delivered twice.
	ev := ProviderEvent{
		EventID:     "ev-1",
		Model:       "glm",
		TotalTokens: 600,
		Raw:         []byte(`{}`),
	}
	require.NoError(t, s.RecordProviderEvent(ev))
	require.NoError(t, s.RecordProviderEvent(ev))
	require.NoError(t, s.RecordProviderEvent(ProviderEvent{
		EventID:      "ev-2",
		Model:        "glm",
		InputTokens:  100,
		OutputTokens: 50,
		Raw:          []byte(`{}`),
	}))

	report, err := s.ReconReport(time.Now().Add(-time.Hour))
	require.NoError(t, err)

	row, ok := report["glm"]
	require.True(t, ok)
	require.EqualValues(t, 750, row.MeteredTokens)
	require.EqualValues(t, 750, row.ProviderTokens)
	require.EqualValues(t, 0, row.DeltaTokens)

	// A report window in the future is empty.
	report, err = s.ReconReport(time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Empty(t, report)
}
