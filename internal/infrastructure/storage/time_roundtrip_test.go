package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Walmart order dates carry a numeric offset with no zone abbreviation, so
// time.String() renders them as "... -0400 -0400", which did not scan back.
func TestRecordTimesWithNumericZoneRoundTrip(t *testing.T) {
	s, err := NewStorage(filepath.Join(t.TempDir(), "times.db"))
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	orderDate := time.Date(2026, 10, 12, 0, 24, 11, 0, time.FixedZone("", -4*3600))
	processedAt := time.Now() // carries a monotonic clock reading
	require.NoError(t, s.SaveRecord(&ProcessingRecord{
		OrderID: "SYN-TIME", Provider: "walmart", OrderDate: orderDate, ProcessedAt: processedAt, Status: "dry-run",
	}))

	got, err := s.GetRecord("SYN-TIME")
	require.NoError(t, err)
	assert.True(t, got.OrderDate.Equal(orderDate), "order date %v", got.OrderDate)
	assert.WithinDuration(t, processedAt, got.ProcessedAt, time.Microsecond)

	attempts, err := s.GetAttemptsByOrderID("SYN-TIME")
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.True(t, attempts[0].OrderDate.Equal(orderDate))

	list, err := s.ListOrders(OrderFilters{})
	require.NoError(t, err)
	require.Len(t, list.Orders, 1)
	assert.True(t, list.Orders[0].OrderDate.Equal(orderDate))
}

func TestMigrationRewritesLegacyTimeStrings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	s, err := NewStorage(path)
	require.NoError(t, err)
	// Simulate rows written by older builds (time.String() output).
	_, err = s.db.Exec(`INSERT INTO processing_records (order_id, provider, order_date, processed_at, status,
		order_total, order_subtotal, order_tax, order_tip, transaction_amount, split_count, item_count, match_confidence, dry_run)
		VALUES ('SYN-LEGACY', 'walmart', '2026-10-12 00:24:11 -0400 -0400', '2026-10-20 01:24:37.11121 -0500 CDT m=+216.316915209', 'dry-run', 0, 0, 0, 0, 0, 0, 0, 0, 1)`)
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO processing_attempts (order_id, provider, order_date, processed_at, status,
		order_total, order_subtotal, order_tax, order_tip, transaction_amount, split_count, item_count, match_confidence, dry_run)
		VALUES ('SYN-LEGACY', 'walmart', '2026-08-30 14:31:33 -0400 -0400', '2026-10-20 01:25:01.137731 -0500 CDT m=+240.344290626', 'dry-run', 0, 0, 0, 0, 0, 0, 0, 0, 1)`)
	require.NoError(t, err)

	require.NoError(t, rewriteLegacyTimes(s.db))

	got, err := s.GetRecord("SYN-LEGACY")
	require.NoError(t, err)
	assert.True(t, got.OrderDate.Equal(time.Date(2026, 10, 12, 4, 24, 11, 0, time.UTC)))
	assert.True(t, got.ProcessedAt.Equal(time.Date(2026, 10, 20, 6, 24, 37, 111210000, time.UTC)))
	attempts, err := s.GetAttemptsByOrderID("SYN-LEGACY")
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	require.NoError(t, s.Close())
}
