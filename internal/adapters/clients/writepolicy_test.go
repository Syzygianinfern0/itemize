package clients

import (
	"context"
	"errors"
	"testing"

	"github.com/eshaffer321/monarch-go/v2/pkg/monarch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTransactions records which writes reach the underlying service.
type fakeTransactions struct {
	monarch.TransactionService
	calls []string
}

func (f *fakeTransactions) Update(_ context.Context, id string, _ *monarch.UpdateTransactionParams) (*monarch.Transaction, error) {
	f.calls = append(f.calls, "update "+id)
	return &monarch.Transaction{ID: id}, nil
}

func (f *fakeTransactions) Delete(_ context.Context, id string) error {
	f.calls = append(f.calls, "delete "+id)
	return nil
}

func (f *fakeTransactions) Create(_ context.Context, _ *monarch.CreateTransactionParams) (*monarch.Transaction, error) {
	f.calls = append(f.calls, "create")
	return &monarch.Transaction{}, nil
}

func (f *fakeTransactions) UpdateSplits(_ context.Context, id string, _ []*monarch.TransactionSplit) error {
	f.calls = append(f.calls, "splits "+id)
	return nil
}

func guardedClient(allow bool) (*monarch.Client, *fakeTransactions) {
	fake := &fakeTransactions{}
	client := &monarch.Client{Transactions: fake}
	ApplyWritePolicy(client, allow)
	return client, fake
}

func TestWritePolicy_RefusesDeleteCreateAndAmountChangesByDefault(t *testing.T) {
	client, fake := guardedClient(false)
	ctx := context.Background()
	amount := -19.20

	err := client.Transactions.Delete(ctx, "t1")
	assert.True(t, errors.Is(err, ErrDestructiveWriteForbidden))

	_, err = client.Transactions.Update(ctx, "t1", &monarch.UpdateTransactionParams{Amount: &amount})
	assert.True(t, errors.Is(err, ErrDestructiveWriteForbidden))

	_, err = client.Transactions.Create(ctx, &monarch.CreateTransactionParams{})
	assert.True(t, errors.Is(err, ErrDestructiveWriteForbidden))

	assert.Empty(t, fake.calls, "refused writes must never reach Monarch")
}

func TestWritePolicy_AllowsCategoryNotesAndSplits(t *testing.T) {
	client, fake := guardedClient(false)
	ctx := context.Background()
	category, notes := "groceries", "Groceries:\n- Bread $2.00"

	_, err := client.Transactions.Update(ctx, "t1", &monarch.UpdateTransactionParams{CategoryID: &category, Notes: &notes})
	require.NoError(t, err)
	require.NoError(t, client.Transactions.UpdateSplits(ctx, "t1", []*monarch.TransactionSplit{{Amount: -1}}))

	assert.Equal(t, []string{"update t1", "splits t1"}, fake.calls)
}

func TestWritePolicy_DestructiveWritesPassWhenExplicitlyAllowed(t *testing.T) {
	client, fake := guardedClient(true)
	ctx := context.Background()
	amount := -5.0

	require.NoError(t, client.Transactions.Delete(ctx, "t2"))
	_, err := client.Transactions.Update(ctx, "t2", &monarch.UpdateTransactionParams{Amount: &amount})
	require.NoError(t, err)
	assert.Equal(t, []string{"delete t2", "update t2"}, fake.calls)
}

func TestWritePolicy_ApplyingTwiceDoesNotNest(t *testing.T) {
	client, _ := guardedClient(false)
	ApplyWritePolicy(client, false)
	policy, ok := client.Transactions.(*policyTransactions)
	require.True(t, ok)
	_, nested := policy.TransactionService.(*policyTransactions)
	assert.False(t, nested)
}
