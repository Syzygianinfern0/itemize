package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eshaffer321/monarch-go/v2/pkg/monarch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveTransactions is a fake Monarch transaction service whose list query
// returns the current state of a small set of transactions.
type liveTransactions struct {
	monarch.TransactionService
	current     map[string]*monarch.Transaction
	splits      map[string][]*monarch.TransactionSplit
	writes      []string
	listQueries int
}

func (l *liveTransactions) Query() monarch.TransactionQueryBuilder {
	l.listQueries++
	return &liveQuery{owner: l}
}

func (l *liveTransactions) GetSplits(_ context.Context, id string) ([]*monarch.TransactionSplit, error) {
	return l.splits[id], nil
}

func (l *liveTransactions) Update(_ context.Context, id string, _ *monarch.UpdateTransactionParams) (*monarch.Transaction, error) {
	l.writes = append(l.writes, "update "+id)
	return l.current[id], nil
}

func (l *liveTransactions) UpdateSplits(_ context.Context, id string, _ []*monarch.TransactionSplit) error {
	l.writes = append(l.writes, "splits "+id)
	return nil
}

type liveQuery struct {
	monarch.TransactionQueryBuilder
	owner *liveTransactions
}

func (q *liveQuery) Between(_, _ time.Time) monarch.TransactionQueryBuilder { return q }
func (q *liveQuery) Limit(int) monarch.TransactionQueryBuilder              { return q }
func (q *liveQuery) Offset(int) monarch.TransactionQueryBuilder             { return q }
func (q *liveQuery) Execute(context.Context) (*monarch.TransactionList, error) {
	list := &monarch.TransactionList{}
	for _, txn := range q.owner.current {
		copied := *txn
		list.Transactions = append(list.Transactions, &copied)
	}
	return list, nil
}

var prewriteDay = time.Date(2025, 2, 3, 0, 0, 0, 0, time.UTC)

func seenTransaction() *monarch.Transaction {
	return &monarch.Transaction{
		ID:        "t1",
		Amount:    -12.34,
		Date:      monarch.Date{Time: prewriteDay},
		UpdatedAt: monarch.Date{Time: prewriteDay.Add(3 * time.Hour)},
		Merchant:  &monarch.Merchant{Name: "Walmart"},
	}
}

func verifyingAdapter(live *liveTransactions) *monarchAdapter {
	a := &monarchAdapter{client: &monarch.Client{Transactions: live}, verifyWrites: true}
	a.rememberSeen(seenTransaction())
	return a
}

func categoryParams() *monarch.UpdateTransactionParams {
	category, notes := "groceries", "Groceries:\n- Bread $2.00"
	return &monarch.UpdateTransactionParams{CategoryID: &category, Notes: &notes}
}

func TestPreWrite_UnchangedTransactionIsWritten(t *testing.T) {
	live := &liveTransactions{current: map[string]*monarch.Transaction{"t1": seenTransaction()}}
	a := verifyingAdapter(live)

	require.NoError(t, a.UpdateTransaction(context.Background(), "t1", categoryParams()))
	// A second write to the same transaction in this run is not re-checked.
	require.NoError(t, a.UpdateSplits(context.Background(), "t1", []*monarch.TransactionSplit{{Amount: -12.34}}))
	assert.Equal(t, []string{"update t1", "splits t1"}, live.writes)
	assert.Equal(t, 1, live.listQueries)
}

func TestPreWrite_RefusesWhenOwnerEditedTransaction(t *testing.T) {
	edited := seenTransaction()
	edited.UpdatedAt = monarch.Date{Time: prewriteDay.Add(5 * time.Hour)}
	live := &liveTransactions{current: map[string]*monarch.Transaction{"t1": edited}}

	err := verifyingAdapter(live).UpdateTransaction(context.Background(), "t1", categoryParams())
	assert.True(t, errors.Is(err, ErrTransactionChanged))
	assert.Empty(t, live.writes)
}

func TestPreWrite_RefusesWhenAmountOrDateChanged(t *testing.T) {
	changed := seenTransaction()
	changed.Amount = -15.00
	live := &liveTransactions{current: map[string]*monarch.Transaction{"t1": changed}}
	err := verifyingAdapter(live).UpdateTransaction(context.Background(), "t1", categoryParams())
	assert.True(t, errors.Is(err, ErrTransactionChanged))

	moved := seenTransaction()
	moved.Date = monarch.Date{Time: prewriteDay.AddDate(0, 0, 1)}
	live = &liveTransactions{current: map[string]*monarch.Transaction{"t1": moved}}
	err = verifyingAdapter(live).UpdateTransaction(context.Background(), "t1", categoryParams())
	assert.True(t, errors.Is(err, ErrTransactionChanged))
	assert.Empty(t, live.writes)
}

func TestPreWrite_RefusesWhenTransactionAlreadySplit(t *testing.T) {
	live := &liveTransactions{
		current: map[string]*monarch.Transaction{"t1": seenTransaction()},
		splits:  map[string][]*monarch.TransactionSplit{"t1": {{ID: "s1", Amount: -6}, {ID: "s2", Amount: -6.34}}},
	}
	err := verifyingAdapter(live).UpdateSplits(context.Background(), "t1", []*monarch.TransactionSplit{{Amount: -12.34}})
	assert.True(t, errors.Is(err, ErrTransactionChanged))
	assert.Empty(t, live.writes)

	// Reconciliation re-applies itemize's own splits and is allowed to replace them.
	err = verifyingAdapter(live).UpdateSplits(withExistingSplitsAllowed(context.Background()), "t1", []*monarch.TransactionSplit{{Amount: -12.34}})
	require.NoError(t, err)
}

func TestPreWrite_RefusesWhenTransactionDisappeared(t *testing.T) {
	live := &liveTransactions{current: map[string]*monarch.Transaction{}}
	err := verifyingAdapter(live).UpdateTransaction(context.Background(), "t1", categoryParams())
	assert.True(t, errors.Is(err, ErrTransactionChanged))
	assert.Empty(t, live.writes)
}

func TestPreWrite_RefusesTransactionNeverSeen(t *testing.T) {
	live := &liveTransactions{current: map[string]*monarch.Transaction{"t1": seenTransaction()}}
	err := verifyingAdapter(live).UpdateTransaction(context.Background(), "unknown", categoryParams())
	assert.True(t, errors.Is(err, ErrTransactionChanged))
	assert.Empty(t, live.writes)
}
