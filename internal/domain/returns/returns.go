// Package returns infers which purchase a store refund came from.
//
// Walmart does not expose in-store returns through the endpoints itemize
// uses: the refund appears in the bank feed as a positive transaction, and the
// original receipt is unchanged. This package finds the purchased item (or
// whole order) whose price, with that order's tax rate, equals the refund.
// Items the provider itself marked as returned are preferred and matched
// with a looser tax bound, since item-level tax rates differ.
package returns

import (
	"fmt"
	"math"
	"time"

	"github.com/eshaffer321/itemize/internal/adapters/providers"
)

// Candidate is a purchase a refund may have come from.
type Candidate struct {
	Order      providers.Order
	Items      []providers.OrderItem // the returned item(s); all items when WholeOrder
	WholeOrder bool
	Marked     bool    // the provider marked these items as returned
	Expected   float64 // refund amount this candidate predicts (0 for marked range matches)
}

// Finder searches orders for refund candidates.
type Finder struct {
	WindowDays       int     // how far before the refund the purchase may be
	Tolerance        float64 // amount tolerance for exact (unmarked) matches
	MarkedMaxTaxRate float64 // marked items match refunds up to price*(1+rate)
}

// DefaultFinder matches Walmart's 90-day return window.
var DefaultFinder = Finder{WindowDays: 90, Tolerance: 0.015, MarkedMaxTaxRate: 0.12}

// ReturnMarked is implemented by orders that know which items were returned.
type ReturnMarked interface {
	GetRefundItems() ([]providers.OrderItem, error)
}

// Find lists every candidate for a refund of amount (positive) on refundDate.
func (f Finder) Find(amount float64, refundDate time.Time, orders []providers.Order) []Candidate {
	amount = math.Abs(amount)
	earliest := refundDate.AddDate(0, 0, -f.WindowDays)
	latest := refundDate.AddDate(0, 0, 1)

	var out []Candidate
	for _, order := range orders {
		date := order.GetDate()
		if date.Before(earliest) || date.After(latest) {
			continue
		}
		items := order.GetItems()
		if len(items) == 0 {
			continue
		}

		if marked := markedItems(order, items); len(marked) > 0 {
			subtotal := 0.0
			for _, item := range marked {
				subtotal += item.GetPrice()
			}
			if subtotal > 0 && amount >= subtotal-f.Tolerance && amount <= subtotal*(1+f.MarkedMaxTaxRate)+f.Tolerance {
				out = append(out, Candidate{Order: order, Items: marked, Marked: true})
			}
		}

		if near(order.GetTotal(), amount, f.Tolerance) {
			out = append(out, Candidate{Order: order, Items: items, WholeOrder: true, Expected: round2(order.GetTotal())})
			if len(items) == 1 {
				continue // the single item is the whole order
			}
		}

		rate := 0.0
		if order.GetSubtotal() > 0 {
			rate = order.GetTax() / order.GetSubtotal()
		}
		for _, item := range items {
			if expected, ok := f.itemMatches(item, rate, amount); ok {
				out = append(out, Candidate{Order: order, Items: []providers.OrderItem{item}, Expected: expected})
			}
		}
	}
	return out
}

// itemMatches checks the item's line price and, for multiples, one unit, each
// with and without the order's tax rate.
func (f Finder) itemMatches(item providers.OrderItem, rate, amount float64) (float64, bool) {
	prices := []float64{item.GetPrice()}
	if item.GetQuantity() > 1 && item.GetUnitPrice() > 0 {
		prices = append(prices, item.GetUnitPrice())
	}
	for _, price := range prices {
		if price <= 0 {
			continue
		}
		for _, expected := range []float64{price * (1 + rate), price} {
			if near(expected, amount, f.Tolerance) {
				return round2(expected), true
			}
		}
	}
	return 0, false
}

// Pick chooses the candidate to use, or explains why there is none. Items the
// provider marked as returned win over price-only matches; otherwise exactly
// one candidate must remain.
func Pick(candidates []Candidate) (*Candidate, string) {
	var marked []Candidate
	for _, c := range candidates {
		if c.Marked {
			marked = append(marked, c)
		}
	}
	switch {
	case len(marked) == 1:
		return &marked[0], ""
	case len(marked) > 1:
		return nil, fmt.Sprintf("ambiguous: %d orders have returned items matching this refund", len(marked))
	case len(candidates) == 1:
		return &candidates[0], ""
	case len(candidates) == 0:
		return nil, "no purchase in the return window matches this refund"
	default:
		return nil, fmt.Sprintf("ambiguous: %d purchases match this refund", len(candidates))
	}
}

func markedItems(order providers.Order, items []providers.OrderItem) []providers.OrderItem {
	rm, ok := order.(ReturnMarked)
	if !ok {
		return nil
	}
	refunded, err := rm.GetRefundItems()
	if err != nil || len(refunded) == 0 {
		return nil
	}
	// Return metadata lives on a different item list than GetItems; pair
	// them by name and price.
	used := make([]bool, len(items))
	var out []providers.OrderItem
	for _, r := range refunded {
		for i, item := range items {
			if !used[i] && item.GetName() == r.GetName() && near(item.GetPrice(), r.GetPrice(), 0.005) {
				used[i] = true
				out = append(out, item)
				break
			}
		}
	}
	return out
}

func near(a, b, tolerance float64) bool {
	return math.Abs(a-b) <= tolerance
}

func round2(x float64) float64 {
	return math.Round(x*100) / 100
}
