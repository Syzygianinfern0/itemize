package sync

import (
	"context"
	"testing"

	"github.com/eshaffer321/itemize/internal/adapters/providers"
	"github.com/eshaffer321/itemize/internal/domain/categorizer"
	"github.com/eshaffer321/itemize/internal/infrastructure/storage"
	"github.com/eshaffer321/monarch-go/v2/pkg/monarch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storeOrder is a minimal Walmart order for return matching.
type storeOrder struct{ mockSimpleOrder }

func (s *storeOrder) GetFinalCharges() ([]float64, error) { return []float64{s.total}, nil }
func (s *storeOrder) IsMultiDelivery() (bool, error)      { return false, nil }

func TestInferredReturns_ProposesRefundCategorizedLikeReturnedItem(t *testing.T) {
	refundDate := prewriteDay.AddDate(0, 0, 40)
	refund := &monarch.Transaction{ID: "r1", Amount: 27.50, Date: monarch.Date{Time: refundDate}, Merchant: &monarch.Merchant{Name: "Walmart"}}
	unrelated := &monarch.Transaction{ID: "r2", Amount: 3.33, Date: monarch.Date{Time: refundDate}, Merchant: &monarch.Merchant{Name: "Walmart"}}
	live := &liveTransactions{current: map[string]*monarch.Transaction{"r1": refund, "r2": unrelated}}
	o, store := newProposeOrchestrator(t, live)
	o.monarchAdapter.rememberSeen(refund, unrelated)

	purchase := &storeOrder{mockSimpleOrder{
		id: "SYN-STORE-1", date: prewriteDay, subtotal: 40, tax: 4, total: 44, providerName: "walmart",
		items: []providers.OrderItem{
			&mockOrderItem{name: "Bananas", price: 15, quantity: 1},
			&mockOrderItem{name: "Paper Towels", price: 25, quantity: 1},
		},
	}}
	cats := []categorizer.Category{{ID: "groceries", Name: "Groceries"}, {ID: "home", Name: "Home"}}

	result := &Result{}
	used := map[string]bool{}
	o.processInferredReturns(context.Background(), []providers.Order{purchase}, []*monarch.Transaction{refund, unrelated}, used, cats, nil, Options{Propose: true}, result)

	assert.Equal(t, 1, result.RefundProcessedCount)
	assert.Equal(t, 1, result.RefundSkippedCount, "a refund matching no purchase is left alone")
	assert.True(t, used["r1"])
	assert.Empty(t, live.writes)

	proposals, err := store.GetOrderProposals("return:r1")
	require.NoError(t, err)
	require.Len(t, proposals, 1)
	p := proposals[0]
	assert.Equal(t, storage.ProposalKindCategory, p.Kind)
	assert.Equal(t, "home", p.CategoryID, "the refund mirrors the returned paper towels")
	assert.Contains(t, p.Notes, "Return of "+prewriteDay.Format("2006-01-02")+" purchase (matched by price)")
	assert.Contains(t, p.NotePrefix, "matched by price")
	assert.Equal(t, 27.50, p.Amount)

	record, err := store.GetRecord("SYN-STORE-1")
	require.NoError(t, err)
	assert.Nil(t, record, "the purchase's own record is untouched")
	returnRecord, err := store.GetRecord("return:r1")
	require.NoError(t, err)
	assert.Equal(t, refundDate.Format("2006-01-02"), returnRecord.OrderDate.Format("2006-01-02"))
}
