package handlers

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/eshaffer321/itemize/internal/adapters/providers"
	"github.com/eshaffer321/itemize/internal/domain/categorizer"
	"github.com/eshaffer321/itemize/internal/domain/splitter"
	"github.com/eshaffer321/monarch-go/v2/pkg/monarch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// byNameCategorizer assigns categories from a fixed item-name map.
type byNameCategorizer map[string]string

func (c byNameCategorizer) CategorizeItems(_ context.Context, items []categorizer.Item, _ []categorizer.Category) (*categorizer.CategorizationResult, error) {
	result := &categorizer.CategorizationResult{}
	for i, item := range items {
		id := c[item.Name]
		result.Categorizations = append(result.Categorizations, categorizer.ItemCategorization{
			ItemIndex: i, ItemName: item.Name, CategoryID: id, CategoryName: id,
		})
	}
	return result, nil
}

// recordingMonarch records every write a handler asks for.
type recordingMonarch struct {
	updates map[string]*monarch.UpdateTransactionParams
	splits  map[string][]*monarch.TransactionSplit
}

func newRecordingMonarch() *recordingMonarch {
	return &recordingMonarch{
		updates: map[string]*monarch.UpdateTransactionParams{},
		splits:  map[string][]*monarch.TransactionSplit{},
	}
}

func (r *recordingMonarch) UpdateTransaction(_ context.Context, id string, params *monarch.UpdateTransactionParams) error {
	r.updates[id] = params
	return nil
}

func (r *recordingMonarch) UpdateSplits(_ context.Context, id string, splits []*monarch.TransactionSplit) error {
	r.splits[id] = splits
	return nil
}

// failingConsolidator fails the test if consolidation is attempted.
type failingConsolidator struct{ t *testing.T }

func (f failingConsolidator) ConsolidateTransactions(context.Context, []*monarch.Transaction, providers.Order, bool) (*ConsolidationResult, error) {
	f.t.Fatal("consolidation (amount rewrite + delete) must not run by default")
	return nil, nil
}

func newMultiChargeHandler(t *testing.T, cats byNameCategorizer, mon *recordingMonarch) *WalmartHandler {
	h := createTestWalmartHandler(t, nil, nil, nil)
	h.consolidator = failingConsolidator{t}
	h.splitter = splitter.NewSplitter(cats)
	h.monarch = mon
	return h
}

func sumSplits(splits []*monarch.TransactionSplit) float64 {
	total := 0.0
	for _, s := range splits {
		total += s.Amount
	}
	return math.Round(total*100) / 100
}

func assertNoAmountChange(t *testing.T, mon *recordingMonarch) {
	t.Helper()
	for id, params := range mon.updates {
		assert.Nil(t, params.Amount, "amount of %s must not change", id)
	}
}

var threeCategoryItems = []providers.OrderItem{
	&walmartTestItem{name: "Gym Shorts", price: 7.00, quantity: 1},
	&walmartTestItem{name: "Sparkling Water", price: 9.00, quantity: 1},
	&walmartTestItem{name: "Duffel Bag", price: 28.00, quantity: 1},
}

var threeCategoryMap = byNameCategorizer{"Gym Shorts": "clothing", "Sparkling Water": "groceries", "Duffel Bag": "travel"}

func TestWalmartMultiCharge_RefundNettedTotalSplitsEachChargeWithoutConsolidating(t *testing.T) {
	// Walmart's order total ($19.26) is net of a later $30.74 refund; the three
	// charges ($2.00 tip, $6.00, $42.00) are what the bank actually billed.
	date := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	order := &walmartTestOrder{
		id: "SYN-MULTI-REFUND", date: date, total: 19.26, subtotal: 44.00, tax: 4.00,
		items: threeCategoryItems, charges: []float64{2.00, 6.00, 42.00},
	}
	txns := []*monarch.Transaction{
		{ID: "c1", Amount: -2.00, Date: walmartToMonarchDate(date)},
		{ID: "c2", Amount: -6.00, Date: walmartToMonarchDate(date)},
		{ID: "c3", Amount: -42.00, Date: walmartToMonarchDate(date)},
		{ID: "refund", Amount: 30.74, Date: walmartToMonarchDate(date.AddDate(0, 0, 10))},
	}
	mon := newRecordingMonarch()
	h := newMultiChargeHandler(t, threeCategoryMap, mon)

	used := map[string]bool{}
	result, err := h.ProcessOrder(context.Background(), order, txns, used, nil, nil, false)
	require.NoError(t, err)
	require.True(t, result.Processed)

	assertNoAmountChange(t, mon)
	require.Len(t, mon.splits, 3, "every charge's transaction gets its own splits")
	for _, txn := range txns[:3] {
		splits := mon.splits[txn.ID]
		require.Len(t, splits, 3)
		assert.Equal(t, txn.Amount, sumSplits(splits), "splits of %s must sum to its own amount", txn.ID)
		assert.Contains(t, splits[0].Notes, "of 3 ($")
	}
	// Proportional: the travel item is 28/44 of every charge.
	assert.InDelta(t, -42.00*28/44, splitByID(mon.splits["c3"], "travel"), 0.011)
	assert.False(t, used["refund"], "the refund stays a separate transaction")
	require.Len(t, result.Charges, 3)
}

func TestWalmartMultiCharge_SingleCategoryChargesSummingToGrossTotal(t *testing.T) {
	date := time.Date(2025, 4, 1, 12, 0, 0, 0, time.UTC)
	order := &walmartTestOrder{
		id: "SYN-MULTI-GROSS", date: date, total: 50.00, subtotal: 46.00, tax: 4.00,
		items: []providers.OrderItem{
			&walmartTestItem{name: "Apples", price: 20, quantity: 1},
			&walmartTestItem{name: "Bread", price: 26, quantity: 1},
		},
		charges: []float64{30.00, 20.00},
	}
	txns := []*monarch.Transaction{
		{ID: "a", Amount: -30.00, Date: walmartToMonarchDate(date)},
		{ID: "b", Amount: -20.00, Date: walmartToMonarchDate(date.AddDate(0, 0, 1))},
	}
	mon := newRecordingMonarch()
	h := newMultiChargeHandler(t, byNameCategorizer{"Apples": "groceries", "Bread": "groceries"}, mon)

	result, err := h.ProcessOrder(context.Background(), order, txns, map[string]bool{}, nil, nil, false)
	require.NoError(t, err)
	require.True(t, result.Processed)

	assertNoAmountChange(t, mon)
	require.Len(t, mon.updates, 2)
	assert.Empty(t, mon.splits)
	assert.Equal(t, "groceries", *mon.updates["a"].CategoryID)
	assert.Contains(t, *mon.updates["a"].Notes, "Charge 1 of 2 ($30.00 of $50.00")
	assert.Contains(t, *mon.updates["b"].Notes, "Charge 2 of 2 ($20.00 of $50.00")
	assert.Contains(t, *mon.updates["b"].Notes, "- Apples $20.00")
}

func TestWalmartMultiCharge_TipChargeIsSplitOnItsOwn(t *testing.T) {
	date := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	order := &walmartTestOrder{
		id: "SYN-MULTI-TIP", date: date, total: 51.52, subtotal: 44.00, tax: 3.52,
		items: threeCategoryItems, charges: []float64{47.52, 4.00},
	}
	txns := []*monarch.Transaction{
		{ID: "goods", Amount: -47.52, Date: walmartToMonarchDate(date)},
		{ID: "tip", Amount: -4.00, Date: walmartToMonarchDate(date)},
	}
	mon := newRecordingMonarch()
	h := newMultiChargeHandler(t, threeCategoryMap, mon)

	result, err := h.ProcessOrder(context.Background(), order, txns, map[string]bool{}, nil, nil, false)
	require.NoError(t, err)
	require.True(t, result.Processed)

	assertNoAmountChange(t, mon)
	assert.Equal(t, -4.00, sumSplits(mon.splits["tip"]))
	assert.Equal(t, -47.52, sumSplits(mon.splits["goods"]))
}

func TestWalmartMultiCharge_DryRunWritesNothingButReportsEveryCharge(t *testing.T) {
	date := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	order := &walmartTestOrder{
		id: "SYN-MULTI-DRY", date: date, total: 50, subtotal: 44, tax: 6,
		items: threeCategoryItems, charges: []float64{20, 30},
	}
	txns := []*monarch.Transaction{
		{ID: "x", Amount: -20, Date: walmartToMonarchDate(date)},
		{ID: "y", Amount: -30, Date: walmartToMonarchDate(date)},
	}
	mon := newRecordingMonarch()
	h := newMultiChargeHandler(t, threeCategoryMap, mon)

	result, err := h.ProcessOrder(context.Background(), order, txns, map[string]bool{}, nil, nil, true)
	require.NoError(t, err)
	assert.Empty(t, mon.updates)
	assert.Empty(t, mon.splits)
	require.Len(t, result.Charges, 2)
	assert.Equal(t, -30.0, sumSplits(result.Charges[1].Splits))
}

func TestWalmartMultiCharge_AggregateSubsetSplitsEachTransaction(t *testing.T) {
	date := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	order := &walmartTestOrder{
		id: "SYN-MULTI-SUBSET", date: date, total: 75.61, subtotal: 70, tax: 5.61,
		items: threeCategoryItems, charges: []float64{0.01, 4.08, 71.52},
	}
	txns := []*monarch.Transaction{
		{ID: "big", Amount: -75.60, Date: walmartToMonarchDate(date)},
		{ID: "penny", Amount: -0.01, Date: walmartToMonarchDate(date)},
	}
	mon := newRecordingMonarch()
	h := newMultiChargeHandler(t, threeCategoryMap, mon)

	result, err := h.ProcessOrder(context.Background(), order, txns, map[string]bool{}, nil, nil, false)
	require.NoError(t, err)
	require.True(t, result.Processed)
	assertNoAmountChange(t, mon)
	assert.Equal(t, -75.60, sumSplits(mon.splits["big"]))
	// A one-cent charge cannot be split three ways; it goes to one category.
	assert.Equal(t, "travel", *mon.updates["penny"].CategoryID)
}

func TestWalmartMultiCharge_InterruptedConsolidationIsLeftForReview(t *testing.T) {
	date := time.Now()
	consolidated := &monarch.Transaction{
		ID: "partial", Amount: -70.28, Date: walmartToMonarchDate(date),
		Notes: "Multi-delivery order (2 charges: $6.06, $64.22)",
	}
	order := &walmartTestOrder{
		id: "SYN-INTERRUPTED", date: date, total: 70.28, subtotal: 65, tax: 5.28,
		items: threeCategoryItems, charges: []float64{6.06, 64.22},
	}
	txns := []*monarch.Transaction{consolidated, {ID: "leftover", Amount: -64.22, Date: walmartToMonarchDate(date)}}
	mon := newRecordingMonarch()
	h := newMultiChargeHandler(t, threeCategoryMap, mon)

	result, err := h.ProcessOrder(context.Background(), order, txns, map[string]bool{}, nil, nil, false)
	require.NoError(t, err)
	assert.True(t, result.Skipped)
	assert.Contains(t, result.SkipReason, "interrupted consolidation")
	assert.Empty(t, mon.updates)
	assert.Empty(t, mon.splits)
}

func splitByID(splits []*monarch.TransactionSplit, categoryID string) float64 {
	for _, s := range splits {
		if s.CategoryID == categoryID {
			return s.Amount
		}
	}
	return math.NaN()
}

func TestAmazonMultiCharge_SplitsEachChargeWithoutConsolidating(t *testing.T) {
	date := time.Now()
	order := &mockAmazonOrder{
		id: "SYN-AMZ-MULTI", date: date, total: 60, subtotal: 60,
		items: []providers.OrderItem{
			&mockItem{name: "Toy Car", price: 20},
			&mockItem{name: "Notebook", price: 40},
		},
		bankCharges: []float64{25, 35},
	}
	txns := []*monarch.Transaction{
		{ID: "amz-1", Amount: -25, Date: toMonarchDate(date)},
		{ID: "amz-2", Amount: -35, Date: toMonarchDate(date)},
	}
	mon := newRecordingMonarch()
	h := NewAmazonHandler(
		createTestWalmartHandler(t, nil, nil, nil).matcher,
		failingConsolidator{t},
		splitter.NewSplitter(byNameCategorizer{"Toy Car": "toys", "Notebook": "office"}),
		mon,
		nil,
	)

	result, err := h.ProcessOrder(context.Background(), order, txns, map[string]bool{}, nil, nil, false)
	require.NoError(t, err)
	require.True(t, result.Processed)
	require.Len(t, result.Charges, 2)
	for _, txn := range txns {
		assert.Equal(t, txn.Amount, sumSplits(mon.splits[txn.ID]))
		assert.Nil(t, mon.updates[txn.ID].Amount)
	}
}
