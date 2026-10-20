package splitter

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/eshaffer321/itemize/internal/adapters/providers"
	"github.com/eshaffer321/itemize/internal/domain/categorizer"
	"github.com/eshaffer321/monarch-go/v2/pkg/monarch"
)

// Categorizer interface for dependency injection
type Categorizer interface {
	CategorizeItems(ctx context.Context, items []categorizer.Item, categories []categorizer.Category) (*categorizer.CategorizationResult, error)
}

// CategorizedItem is one order item together with the category assigned to it.
type CategorizedItem struct {
	Name         string  `json:"name"`
	Price        float64 `json:"price"`
	Quantity     float64 `json:"quantity"`
	CategoryID   string  `json:"category_id"`
	CategoryName string  `json:"category_name"`
	Confidence   float64 `json:"confidence"`
}

// Splitter creates transaction splits from categorized orders
type Splitter struct {
	categorizer Categorizer
	lastItems   []CategorizedItem // Cache last categorization
	lastKey     string            // Order ID + item fingerprint of the cached categorization
}

// NewSplitter creates a new splitter
func NewSplitter(cat Categorizer) *Splitter {
	return &Splitter{
		categorizer: cat,
	}
}

// CategorizeOrder returns the order's items paired with their categories, in
// item order. The last result is cached so the split, single-category and
// per-charge paths for one order share a single LLM call. The cache key covers
// the item list, because refund views reuse the order ID with other items.
func (s *Splitter) CategorizeOrder(
	ctx context.Context,
	order providers.Order,
	categories []categorizer.Category,
) ([]CategorizedItem, error) {
	orderItems := order.GetItems()
	key := cacheKey(order.GetID(), orderItems)
	if s.lastItems != nil && s.lastKey == key {
		return s.lastItems, nil
	}

	items := make([]categorizer.Item, len(orderItems))
	for i, orderItem := range orderItems {
		items[i] = categorizer.Item{
			Name:     orderItem.GetName(),
			Price:    orderItem.GetPrice(),
			Quantity: int(orderItem.GetQuantity()),
		}
	}

	result, err := s.categorizer.CategorizeItems(ctx, items, categories)
	if err != nil {
		return nil, err
	}

	aligned, err := AlignCategorizations(orderItems, result)
	if err != nil {
		return nil, err
	}
	s.lastItems = aligned
	s.lastKey = key
	return aligned, nil
}

func cacheKey(orderID string, items []providers.OrderItem) string {
	var b strings.Builder
	b.WriteString(orderID)
	for _, item := range items {
		fmt.Fprintf(&b, "\x00%s\x01%.2f\x01%g", item.GetName(), item.GetPrice(), item.GetQuantity())
	}
	return b.String()
}

// AlignCategorizations pairs each order item with its categorization.
//
// Results are joined on ItemIndex when every item has exactly one result with
// a valid index. Otherwise (e.g. a categorizer that does not set indexes) each
// item takes the first unused result with the same name. Pairing by position
// is never used: categorizers may reorder or drop entries, and a positional
// join silently attaches categories to the wrong items. An item left without a
// category is an error.
func AlignCategorizations(orderItems []providers.OrderItem, result *categorizer.CategorizationResult) ([]CategorizedItem, error) {
	if result == nil {
		return nil, fmt.Errorf("no categorization result")
	}

	byIndex := make([]*categorizer.ItemCategorization, len(orderItems))
	indexed := len(result.Categorizations) == len(orderItems)
	for i := range result.Categorizations {
		cat := &result.Categorizations[i]
		if cat.ItemIndex < 0 || cat.ItemIndex >= len(orderItems) || byIndex[cat.ItemIndex] != nil {
			indexed = false
			break
		}
		byIndex[cat.ItemIndex] = cat
	}

	if !indexed {
		byIndex = make([]*categorizer.ItemCategorization, len(orderItems))
		used := make([]bool, len(result.Categorizations))
		for i, item := range orderItems {
			name := normalizeName(item.GetName())
			for j := range result.Categorizations {
				if !used[j] && normalizeName(result.Categorizations[j].ItemName) == name {
					used[j] = true
					byIndex[i] = &result.Categorizations[j]
					break
				}
			}
		}
	}

	aligned := make([]CategorizedItem, len(orderItems))
	var missing []string
	for i, item := range orderItems {
		cat := byIndex[i]
		if cat == nil || cat.CategoryID == "" {
			missing = append(missing, fmt.Sprintf("%q", item.GetName()))
			continue
		}
		aligned[i] = CategorizedItem{
			Name:         item.GetName(),
			Price:        item.GetPrice(),
			Quantity:     item.GetQuantity(),
			CategoryID:   cat.CategoryID,
			CategoryName: cat.CategoryName,
			Confidence:   cat.Confidence,
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("no category for %d item(s): %s", len(missing), strings.Join(missing, ", "))
	}
	return aligned, nil
}

func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// CreateSplits creates transaction splits for a multi-category order
//
// Returns:
//   - nil, nil: Single category detected - caller should update transaction category instead
//   - splits, nil: Multiple categories - splits ready to apply
//   - nil, error: Error occurred during categorization or split creation
//
// For single-category orders, the caller should use Monarch's Update API to set
// the category and notes rather than creating splits.
func (s *Splitter) CreateSplits(
	ctx context.Context,
	order providers.Order,
	transaction *monarch.Transaction,
	categories []categorizer.Category,
	monarchCategories []*monarch.TransactionCategory,
) ([]*monarch.TransactionSplit, error) {
	items, err := s.CategorizeOrder(ctx, order, categories)
	if err != nil {
		return nil, err
	}

	// If only one category, return nil (caller should update transaction instead)
	if CountCategories(items) <= 1 {
		return nil, nil
	}

	return AllocateSplits(items, transaction.Amount)
}

// CountCategories returns the number of distinct categories among items.
func CountCategories(items []CategorizedItem) int {
	seen := make(map[string]bool)
	for _, item := range items {
		seen[item.CategoryID] = true
	}
	return len(seen)
}

type categoryGroup struct {
	categoryID   string
	categoryName string
	items        []CategorizedItem
	subtotal     float64
}

// groupByCategory groups items by category, in order of first appearance.
func groupByCategory(items []CategorizedItem) []*categoryGroup {
	var groups []*categoryGroup
	index := make(map[string]*categoryGroup)
	for _, item := range items {
		group := index[item.CategoryID]
		if group == nil {
			group = &categoryGroup{categoryID: item.CategoryID, categoryName: item.CategoryName}
			index[item.CategoryID] = group
			groups = append(groups, group)
		}
		group.items = append(group.items, item)
		group.subtotal += item.Price
	}
	return groups
}

// AllocateSplits splits amount across the items' categories in proportion to
// each category's item subtotal. Tax, tip and fees are therefore shared out
// proportionally too. The splits carry amount's sign and sum to it exactly
// (rounding is settled cent by cent, largest remainder first).
//
// The same rule serves a whole-order transaction and one of several charges
// for an order: each charge is split in the order's category proportions.
func AllocateSplits(items []CategorizedItem, amount float64) ([]*monarch.TransactionSplit, error) {
	groups := groupByCategory(items)
	if len(groups) == 0 {
		return nil, fmt.Errorf("no items to split")
	}

	total := 0.0
	for _, group := range groups {
		total += group.subtotal
	}
	if math.Abs(total) < 0.005 {
		return nil, fmt.Errorf("cannot allocate splits: item subtotal is zero")
	}

	sign := -1.0
	if amount > 0 {
		sign = 1.0
	}
	totalCents := int64(math.Round(math.Abs(amount) * 100))

	cents := make([]int64, len(groups))
	remainders := make([]float64, len(groups))
	allocated := int64(0)
	for i, group := range groups {
		exact := float64(totalCents) * group.subtotal / total
		cents[i] = int64(math.Floor(exact))
		remainders[i] = exact - float64(cents[i])
		allocated += cents[i]
	}
	order := make([]int, len(groups))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return remainders[order[a]] > remainders[order[b]] })
	for k := 0; allocated < totalCents; k++ {
		cents[order[k%len(order)]]++
		allocated++
	}

	splits := make([]*monarch.TransactionSplit, len(groups))
	for i, group := range groups {
		splits[i] = &monarch.TransactionSplit{
			Amount:     sign * float64(cents[i]) / 100,
			CategoryID: group.categoryID,
			Notes:      groupNotes(group),
		}
	}
	return splits, nil
}

// groupNotes builds the newline-delimited item list for one split.
func groupNotes(group *categoryGroup) string {
	itemDetails := make([]string, 0, len(group.items))
	for _, item := range group.items {
		if item.Quantity > 1 {
			itemDetails = append(itemDetails, fmt.Sprintf("- %s (x%d) $%.2f", item.Name, int(item.Quantity), item.Price))
		} else {
			itemDetails = append(itemDetails, fmt.Sprintf("- %s $%.2f", item.Name, item.Price))
		}
	}
	noteContent := strings.Join(itemDetails, "\n")
	if len(group.items) > 3 {
		return fmt.Sprintf("%s: (%d items)\n%s", group.categoryName, len(group.items), noteContent)
	}
	return fmt.Sprintf("%s:\n%s", group.categoryName, noteContent)
}

// SingleCategoryInfo returns the category and notes for items that all share
// one category.
func SingleCategoryInfo(items []CategorizedItem) (categoryID string, notes string, err error) {
	if len(items) == 0 {
		return "", "", fmt.Errorf("no categorizations returned")
	}

	itemDetails := make([]string, 0, len(items))
	for _, item := range items {
		if item.Quantity > 1 {
			itemDetails = append(itemDetails, fmt.Sprintf("- %s (x%.0f) $%.2f", item.Name, item.Quantity, item.Price))
		} else {
			itemDetails = append(itemDetails, fmt.Sprintf("- %s $%.2f", item.Name, item.Price))
		}
	}

	notes = fmt.Sprintf("%s:\n%s", items[0].CategoryName, strings.Join(itemDetails, "\n"))
	return items[0].CategoryID, notes, nil
}

// GetSingleCategoryInfo extracts category and notes for single-category orders
// This should only be called after CreateSplits returns nil (indicating single category)
// It uses the cached categorization result from CreateSplits to avoid duplicate AI calls
func (s *Splitter) GetSingleCategoryInfo(
	ctx context.Context,
	order providers.Order,
	categories []categorizer.Category,
) (categoryID string, notes string, err error) {
	items, err := s.CategorizeOrder(ctx, order, categories)
	if err != nil {
		return "", "", err
	}
	return SingleCategoryInfo(items)
}
