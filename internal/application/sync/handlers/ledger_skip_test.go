package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/eshaffer321/itemize/internal/adapters/providers"
	"github.com/eshaffer321/monarch-go/v2/pkg/monarch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skippableOrder counts ledger reads and offers ledger-skip hints.
type skippableOrder struct {
	walmartTestOrder
	skippable   bool
	targets     []float64
	ledgerReads int
}

func (s *skippableOrder) GetFinalCharges() ([]float64, error) {
	s.ledgerReads++
	return s.walmartTestOrder.GetFinalCharges()
}
func (s *skippableOrder) LedgerSkippable() bool    { return s.skippable }
func (s *skippableOrder) ChargeTargets() []float64 { return s.targets }

var skipDate = time.Date(2025, 9, 1, 12, 0, 0, 0, time.UTC)

func newSkippable(total float64, charges ...float64) *skippableOrder {
	return &skippableOrder{
		walmartTestOrder: walmartTestOrder{
			id: "SYN-SKIP", date: skipDate, total: total, subtotal: total,
			items:   []providers.OrderItem{&walmartTestItem{name: "Lamp", price: total, quantity: 1}},
			charges: charges,
		},
		skippable: true,
		targets:   []float64{total},
	}
}

func txnAt(id string, amount float64, days int) *monarch.Transaction {
	return &monarch.Transaction{ID: id, Amount: amount, Date: walmartToMonarchDate(skipDate.AddDate(0, 0, days))}
}

func TestLedgerSkip_NoCandidateSkipsLedger(t *testing.T) {
	order := newSkippable(25.90, 25.90)
	h := newMultiChargeHandler(t, byNameCategorizer{"Lamp": "home"}, newRecordingMonarch())

	result, err := h.ProcessOrder(context.Background(), order,
		[]*monarch.Transaction{txnAt("other", -12.00, 1), txnAt("far", -25.90, 40), txnAt("refund", 25.90, 2)},
		map[string]bool{}, nil, nil, true)
	require.NoError(t, err)
	assert.True(t, result.Skipped)
	assert.Equal(t, "no Monarch transaction for $25.90", result.SkipReason)
	assert.Equal(t, 0, order.ledgerReads, "the throttled ledger call is skipped")
}

func TestLedgerSkip_SingleOrMultiChargeCandidateFetchesLedger(t *testing.T) {
	h := newMultiChargeHandler(t, byNameCategorizer{"Lamp": "home"}, newRecordingMonarch())

	single := newSkippable(25.90, 25.90)
	result, err := h.ProcessOrder(context.Background(), single, []*monarch.Transaction{txnAt("a", -25.90, 2)}, map[string]bool{}, nil, nil, true)
	require.NoError(t, err)
	assert.True(t, result.Processed)
	assert.Equal(t, 1, single.ledgerReads)

	multi := newSkippable(50.00, 20.00, 30.00)
	result, err = h.ProcessOrder(context.Background(), multi,
		[]*monarch.Transaction{txnAt("b", -20.00, 1), txnAt("c", -30.00, 6)}, map[string]bool{}, nil, nil, true)
	require.NoError(t, err)
	assert.True(t, result.Processed)
	assert.Equal(t, 1, multi.ledgerReads)
}

func TestLedgerSkip_NotSkippableOrdersAlwaysFetchLedger(t *testing.T) {
	order := newSkippable(25.90, 25.90)
	order.skippable = false // e.g. gift card used, or an item marked returned
	h := newMultiChargeHandler(t, byNameCategorizer{"Lamp": "home"}, newRecordingMonarch())

	_, err := h.ProcessOrder(context.Background(), order, nil, map[string]bool{}, nil, nil, true)
	require.NoError(t, err)
	assert.Equal(t, 1, order.ledgerReads)
}

func TestLedgerSkip_UsedTransactionsAreNotCandidates(t *testing.T) {
	order := newSkippable(25.90, 25.90)
	h := newMultiChargeHandler(t, byNameCategorizer{"Lamp": "home"}, newRecordingMonarch())

	result, err := h.ProcessOrder(context.Background(), order, []*monarch.Transaction{txnAt("a", -25.90, 0)},
		map[string]bool{"a": true}, nil, nil, true)
	require.NoError(t, err)
	assert.True(t, result.Skipped)
	assert.Equal(t, 0, order.ledgerReads)
}
