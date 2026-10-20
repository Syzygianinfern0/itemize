package sync

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/eshaffer321/itemize/internal/adapters/clients"
	"github.com/eshaffer321/monarch-go/v2/pkg/monarch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pagedTransactions returns `total` transactions in pages by offset/limit.
type pagedTransactions struct {
	monarch.TransactionService
	total   int
	offsets []int
}

func (p *pagedTransactions) Query() monarch.TransactionQueryBuilder {
	return &pagedQuery{owner: p}
}

type pagedQuery struct {
	monarch.TransactionQueryBuilder
	owner         *pagedTransactions
	offset, limit int
}

func (q *pagedQuery) Between(_, _ time.Time) monarch.TransactionQueryBuilder { return q }
func (q *pagedQuery) Limit(n int) monarch.TransactionQueryBuilder           { q.limit = n; return q }
func (q *pagedQuery) Offset(n int) monarch.TransactionQueryBuilder          { q.offset = n; return q }
func (q *pagedQuery) Execute(context.Context) (*monarch.TransactionList, error) {
	q.owner.offsets = append(q.owner.offsets, q.offset)
	list := &monarch.TransactionList{}
	for i := q.offset; i < q.owner.total && i < q.offset+q.limit; i++ {
		merchant := "Other Store"
		if i%2 == 0 {
			merchant = "Walmart"
		}
		list.Transactions = append(list.Transactions, &monarch.Transaction{
			ID: fmt.Sprintf("t%d", i), Amount: -1, Merchant: &monarch.Merchant{Name: merchant},
			Date: monarch.Date{Time: time.Now()},
		})
	}
	return list, nil
}

func TestFetchMonarchTransactions_PagesPast500(t *testing.T) {
	txns := &pagedTransactions{total: 1234}
	provider := new(MockProvider)
	provider.On("DisplayName").Return("Walmart")
	o := NewOrchestrator(provider, &clients.Clients{Monarch: &monarch.Client{Transactions: txns}}, nil,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	got, err := o.fetchMonarchTransactions(context.Background(), Options{LookbackDays: 365})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 500, 1000}, txns.offsets)
	assert.Len(t, got, 617, "every Walmart transaction across all pages")
	assert.Len(t, o.monarchAdapter.snapshots, 1234, "all fetched transactions are snapshotted for the pre-write check")
}

func TestFetchMonarchTransactions_ExactMultipleOf500(t *testing.T) {
	txns := &pagedTransactions{total: 1000}
	provider := new(MockProvider)
	provider.On("DisplayName").Return("Walmart")
	o := NewOrchestrator(provider, &clients.Clients{Monarch: &monarch.Client{Transactions: txns}}, nil,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	got, err := o.fetchMonarchTransactions(context.Background(), Options{LookbackDays: 30})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 500, 1000}, txns.offsets)
	assert.Len(t, got, 500)
}

// plaidOnlyTransactions returns one PayPal-intermediated Walmart charge whose
// merchant Monarch normalized to PayPal.
type plaidOnlyTransactions struct{ pagedTransactions }

func (p *plaidOnlyTransactions) Query() monarch.TransactionQueryBuilder { return &plaidOnlyQuery{} }

type plaidOnlyQuery struct{ pagedQuery }

func (q *plaidOnlyQuery) Between(_, _ time.Time) monarch.TransactionQueryBuilder { return q }
func (q *plaidOnlyQuery) Limit(int) monarch.TransactionQueryBuilder              { return q }
func (q *plaidOnlyQuery) Offset(int) monarch.TransactionQueryBuilder             { return q }
func (q *plaidOnlyQuery) Execute(context.Context) (*monarch.TransactionList, error) {
	return &monarch.TransactionList{Transactions: []*monarch.Transaction{
		{ID: "pp", Amount: -4.5, PlaidName: "PAYPAL *WALMART COM", Merchant: &monarch.Merchant{Name: "PayPal"}},
		{ID: "other", Amount: -3, PlaidName: "PAYPAL *SOMESHOP", Merchant: &monarch.Merchant{Name: "PayPal"}},
	}}, nil
}

func TestFetchMonarchTransactions_IncludesRetailerInBankDescription(t *testing.T) {
	provider := new(MockProvider)
	provider.On("DisplayName").Return("Walmart")
	o := NewOrchestrator(provider, &clients.Clients{Monarch: &monarch.Client{Transactions: &plaidOnlyTransactions{}}}, nil,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	got, err := o.fetchMonarchTransactions(context.Background(), Options{LookbackDays: 30})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "pp", got[0].ID)
}
