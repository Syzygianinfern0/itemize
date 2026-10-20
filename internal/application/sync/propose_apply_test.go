package sync

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/eshaffer321/itemize/internal/adapters/clients"
	"github.com/eshaffer321/itemize/internal/adapters/providers"
	"github.com/eshaffer321/itemize/internal/domain/categorizer"
	"github.com/eshaffer321/itemize/internal/infrastructure/storage"
	"github.com/eshaffer321/monarch-go/v2/pkg/monarch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noLLM fails the test if the categorizer reaches the LLM (cache only).
type noLLM struct{ t *testing.T }

func (n noLLM) CreateChatCompletion(context.Context, categorizer.ChatCompletionRequest) (*categorizer.ChatCompletionResponse, error) {
	n.t.Fatal("unexpected LLM call")
	return nil, errors.New("unexpected")
}

var proposeCategories = []categorizer.Category{{ID: "groceries", Name: "Groceries"}, {ID: "home", Name: "Home"}}

func newProposeOrchestrator(t *testing.T, live *liveTransactions) (*Orchestrator, *storage.MockRepository) {
	t.Helper()
	store := storage.NewMockRepository()
	cache := categorizer.NewMemoryCache()
	cache.Set("bananas", "groceries")
	cache.Set("paper towels", "home")
	cl := &clients.Clients{
		Monarch:     &monarch.Client{Transactions: live},
		Categorizer: categorizer.NewCategorizer(noLLM{t}, cache, "test-model"),
	}
	o := NewOrchestrator(nil, cl, store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	require.NoError(t, o.configureMode(Options{Propose: true}))
	o.categoryNames = map[string]string{"groceries": "Groceries", "home": "Home"}
	return o, store
}

func proposeOrder() *mockSimpleOrder {
	return &mockSimpleOrder{
		id: "SYN-PROPOSE-1", date: prewriteDay, total: 12.34, subtotal: 12.34, providerName: "costco",
		items: []providers.OrderItem{
			&mockOrderItem{name: "Bananas", price: 4.34, quantity: 1},
			&mockOrderItem{name: "Paper Towels", price: 8.00, quantity: 1},
		},
	}
}

func proposeOnce(t *testing.T) (*Orchestrator, *storage.MockRepository, *liveTransactions) {
	t.Helper()
	live := &liveTransactions{current: map[string]*monarch.Transaction{"t1": seenTransaction()}}
	o, store := newProposeOrchestrator(t, live)
	txns := []*monarch.Transaction{seenTransaction()}
	o.monarchAdapter.rememberSeen(txns...)

	processed, _, err := o.processOrder(context.Background(), proposeOrder(), txns, map[string]bool{}, proposeCategories, nil, Options{Propose: true})
	require.NoError(t, err)
	require.True(t, processed)
	return o, store, live
}

func TestPropose_StoresProposalAndWritesNothing(t *testing.T) {
	_, store, live := proposeOnce(t)

	assert.Empty(t, live.writes, "a propose run must not write to Monarch")
	list, err := store.ListProposals(storage.ProposalFilters{})
	require.NoError(t, err)
	require.Len(t, list.Proposals, 1)
	p := list.Proposals[0]
	assert.Equal(t, storage.ProposalPending, p.Status)
	assert.Equal(t, storage.ProposalKindSplits, p.Kind)
	assert.Equal(t, "t1", p.TransactionID)
	assert.Equal(t, -12.34, p.Amount)
	require.Len(t, p.Splits, 2)
	assert.Equal(t, "Groceries", p.Splits[0].CategoryName)
	require.Len(t, p.Items, 2, "categorized items are stored for review")
	assert.Equal(t, "home", p.Items[1].CategoryID)
	assert.Contains(t, string(p.Before), `"updated_at"`)
	assert.Contains(t, p.Reason, "matched Monarch transaction")

	record, err := store.GetRecord("SYN-PROPOSE-1")
	require.NoError(t, err)
	assert.Equal(t, "proposed", record.Status)
	assert.True(t, record.DryRun)
	assert.False(t, store.IsProcessed("SYN-PROPOSE-1"))
}

func TestPropose_SkipsOrdersWithReviewedProposals(t *testing.T) {
	o, store, _ := proposeOnce(t)
	list, _ := store.ListProposals(storage.ProposalFilters{})
	_, err := store.SetProposalStatus([]int64{list.Proposals[0].ID}, storage.ProposalRejected, []string{storage.ProposalPending})
	require.NoError(t, err)

	processed, skipped, err := o.processOrder(context.Background(), proposeOrder(), []*monarch.Transaction{seenTransaction()}, map[string]bool{}, proposeCategories, nil, Options{Propose: true})
	require.NoError(t, err)
	assert.False(t, processed)
	assert.True(t, skipped)
}

func approveAll(t *testing.T, store *storage.MockRepository) {
	t.Helper()
	list, err := store.ListProposals(storage.ProposalFilters{Status: storage.ProposalPending})
	require.NoError(t, err)
	var ids []int64
	for _, p := range list.Proposals {
		ids = append(ids, p.ID)
	}
	_, err = store.SetProposalStatus(ids, storage.ProposalApproved, []string{storage.ProposalPending})
	require.NoError(t, err)
}

func TestApply_WritesOnlyApprovedProposalsAndMarksOrderProcessed(t *testing.T) {
	_, store, live := proposeOnce(t)
	applier := NewApplier(&monarch.Client{Transactions: live}, store, nil)

	// Nothing approved yet: nothing is written.
	result, err := applier.Apply(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 0, result.Applied)
	assert.Empty(t, live.writes)

	approveAll(t, store)
	result, err = applier.Apply(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Applied)
	assert.Equal(t, []string{"splits t1"}, live.writes)

	list, _ := store.ListProposals(storage.ProposalFilters{})
	assert.Equal(t, storage.ProposalApplied, list.Proposals[0].Status)
	assert.True(t, store.IsProcessed("SYN-PROPOSE-1"), "applied orders are deduplicated")

	calls, err := store.GetAPICallsByOrderID("SYN-PROPOSE-1")
	require.NoError(t, err)
	assert.NotEmpty(t, calls, "applied writes are in the audit log")
}

func TestApply_StaleWhenTransactionEditedAfterProposal(t *testing.T) {
	_, store, live := proposeOnce(t)
	approveAll(t, store)
	live.current["t1"].UpdatedAt = monarch.Date{Time: prewriteDay.Add(24 * time.Hour)}

	result, err := NewApplier(&monarch.Client{Transactions: live}, store, nil).Apply(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Stale)
	assert.Empty(t, live.writes)
	list, _ := store.ListProposals(storage.ProposalFilters{})
	assert.Equal(t, storage.ProposalStale, list.Proposals[0].Status)
	assert.Contains(t, list.Proposals[0].Error, "edited")
	assert.False(t, store.IsProcessed("SYN-PROPOSE-1"))
}

func TestApply_ExplicitIDThatIsNotApprovedIsSkipped(t *testing.T) {
	_, store, live := proposeOnce(t)
	list, _ := store.ListProposals(storage.ProposalFilters{})

	result, err := NewApplier(&monarch.Client{Transactions: live}, store, nil).Apply(context.Background(), []int64{list.Proposals[0].ID})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Skipped)
	assert.Empty(t, live.writes)
}

func TestPropose_CannotCombineWithDryRun(t *testing.T) {
	o, _ := newProposeOrchestrator(t, &liveTransactions{})
	assert.Error(t, o.configureMode(Options{Propose: true, DryRun: true}))
}
