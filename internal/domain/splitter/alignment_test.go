package splitter

import (
	"context"
	"testing"
	"time"

	"github.com/eshaffer321/itemize/internal/adapters/providers"
	"github.com/eshaffer321/itemize/internal/domain/categorizer"
	"github.com/eshaffer321/monarch-go/v2/pkg/monarch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingCategorizer returns a fixed result and counts calls.
type recordingCategorizer struct {
	result *categorizer.CategorizationResult
	calls  int
}

func (r *recordingCategorizer) CategorizeItems(_ context.Context, _ []categorizer.Item, _ []categorizer.Category) (*categorizer.CategorizationResult, error) {
	r.calls++
	return r.result, nil
}

func threeItemOrder() *mockOrder {
	return &mockOrder{
		id:       "SYN-1",
		date:     time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		total:    44,
		subtotal: 40,
		tax:      4,
		items: []providers.OrderItem{
			&mockOrderItem{name: "Spring Water", price: 10, quantity: 1},
			&mockOrderItem{name: "Athletic Shorts", price: 10, quantity: 1},
			&mockOrderItem{name: "Suitcase", price: 20, quantity: 1},
		},
	}
}

func splitByCategory(splits []*monarch.TransactionSplit) map[string]*monarch.TransactionSplit {
	out := make(map[string]*monarch.TransactionSplit, len(splits))
	for _, s := range splits {
		out[s.CategoryID] = s
	}
	return out
}

func TestCreateSplits_JoinsOnItemIndexNotPosition(t *testing.T) {
	// Results arrive out of order (e.g., cached items first); ItemIndex is authoritative.
	cat := &recordingCategorizer{result: &categorizer.CategorizationResult{Categorizations: []categorizer.ItemCategorization{
		{ItemIndex: 1, ItemName: "Athletic Shorts", CategoryID: "clothing", CategoryName: "Clothing"},
		{ItemIndex: 2, ItemName: "Suitcase", CategoryID: "travel", CategoryName: "Travel"},
		{ItemIndex: 0, ItemName: "Spring Water", CategoryID: "groceries", CategoryName: "Groceries"},
	}}}

	splits, err := NewSplitter(cat).CreateSplits(context.Background(), threeItemOrder(), &monarch.Transaction{ID: "t", Amount: -44}, nil, nil)
	require.NoError(t, err)
	byCat := splitByCategory(splits)
	require.Len(t, byCat, 3)
	assert.Contains(t, byCat["groceries"].Notes, "Spring Water")
	assert.Contains(t, byCat["clothing"].Notes, "Athletic Shorts")
	assert.Contains(t, byCat["travel"].Notes, "Suitcase")
	assert.InDelta(t, -11.0, byCat["groceries"].Amount, 0.001)
	assert.InDelta(t, -22.0, byCat["travel"].Amount, 0.001)
}

func TestCreateSplits_MissingCategorizationIsAnError(t *testing.T) {
	cat := &recordingCategorizer{result: &categorizer.CategorizationResult{Categorizations: []categorizer.ItemCategorization{
		{ItemIndex: 0, ItemName: "Spring Water", CategoryID: "groceries", CategoryName: "Groceries"},
		{ItemIndex: 2, ItemName: "Suitcase", CategoryID: "travel", CategoryName: "Travel"},
	}}}

	_, err := NewSplitter(cat).CreateSplits(context.Background(), threeItemOrder(), &monarch.Transaction{ID: "t", Amount: -44}, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Athletic Shorts")
}

func TestCreateSplits_DuplicateItemsStayInTheirOwnCategory(t *testing.T) {
	order := &mockOrder{id: "SYN-2", total: 30, subtotal: 30, items: []providers.OrderItem{
		&mockOrderItem{name: "Hangers", price: 5, quantity: 1},
		&mockOrderItem{name: "Bananas", price: 20, quantity: 1},
		&mockOrderItem{name: "Hangers", price: 5, quantity: 1},
	}}
	// No ItemIndex (external categorizer): falls back to name matching in order.
	cat := &recordingCategorizer{result: &categorizer.CategorizationResult{Categorizations: []categorizer.ItemCategorization{
		{ItemName: "Bananas", CategoryID: "groceries", CategoryName: "Groceries"},
		{ItemName: "Hangers", CategoryID: "home", CategoryName: "Home"},
		{ItemName: "Hangers", CategoryID: "home", CategoryName: "Home"},
	}}}

	splits, err := NewSplitter(cat).CreateSplits(context.Background(), order, &monarch.Transaction{ID: "t", Amount: -30}, nil, nil)
	require.NoError(t, err)
	byCat := splitByCategory(splits)
	require.Len(t, byCat, 2)
	assert.InDelta(t, -10.0, byCat["home"].Amount, 0.001)
	assert.NotContains(t, byCat["groceries"].Notes, "Hangers")
}

func TestSplitter_RecategorizesWhenSameOrderHasDifferentItems(t *testing.T) {
	// A refund view shares the order ID but carries only the refunded items.
	full := threeItemOrder()
	refund := &mockOrder{id: full.id, total: -20, subtotal: 20, items: []providers.OrderItem{
		&mockOrderItem{name: "Suitcase", price: 20, quantity: 1},
	}}
	cat := &recordingCategorizer{result: &categorizer.CategorizationResult{Categorizations: []categorizer.ItemCategorization{
		{ItemIndex: 0, ItemName: "Spring Water", CategoryID: "groceries", CategoryName: "Groceries"},
		{ItemIndex: 1, ItemName: "Athletic Shorts", CategoryID: "clothing", CategoryName: "Clothing"},
		{ItemIndex: 2, ItemName: "Suitcase", CategoryID: "travel", CategoryName: "Travel"},
	}}}
	s := NewSplitter(cat)
	_, err := s.CreateSplits(context.Background(), full, &monarch.Transaction{ID: "t", Amount: -44}, nil, nil)
	require.NoError(t, err)

	cat.result = &categorizer.CategorizationResult{Categorizations: []categorizer.ItemCategorization{
		{ItemIndex: 0, ItemName: "Suitcase", CategoryID: "travel", CategoryName: "Travel"},
	}}
	categoryID, _, err := s.GetSingleCategoryInfo(context.Background(), refund, nil)
	require.NoError(t, err)
	assert.Equal(t, "travel", categoryID)
	assert.Equal(t, 2, cat.calls)
}

func TestAllocateSplits_ProportionalToChargeAmount(t *testing.T) {
	items := []CategorizedItem{
		{Name: "Spring Water", Price: 10, CategoryID: "groceries", CategoryName: "Groceries"},
		{Name: "Athletic Shorts", Price: 10, CategoryID: "clothing", CategoryName: "Clothing"},
		{Name: "Suitcase", Price: 20, CategoryID: "travel", CategoryName: "Travel"},
	}
	splits, err := AllocateSplits(items, -5.01)
	require.NoError(t, err)
	require.Len(t, splits, 3)
	// Deterministic order: first appearance of each category.
	assert.Equal(t, []string{"groceries", "clothing", "travel"}, []string{splits[0].CategoryID, splits[1].CategoryID, splits[2].CategoryID})
	sum := 0.0
	for _, s := range splits {
		sum += s.Amount
		assert.Less(t, s.Amount, 0.0)
	}
	assert.InDelta(t, -5.01, sum, 1e-9)
	assert.InDelta(t, -2.50, splits[2].Amount, 0.011)
}
