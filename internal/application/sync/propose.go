package sync

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/eshaffer321/itemize/internal/adapters/providers"
	"github.com/eshaffer321/itemize/internal/domain/splitter"
	"github.com/eshaffer321/itemize/internal/infrastructure/storage"
	"github.com/eshaffer321/monarch-go/v2/pkg/monarch"
)

// Propose mode: handlers run exactly as in a live sync, but the Monarch
// adapter captures each write instead of sending it. After an order is
// processed its captured writes become proposals, one per transaction.

// capturedWrite is the merged set of writes a handler asked for on one
// transaction while processing one order.
type capturedWrite struct {
	transactionID string
	kind          string
	categoryID    string
	notes         string
	hasNotes      bool
	splits        []*monarch.TransactionSplit
	markReviewed  bool
	items         []splitter.CategorizedItem
}

// captureUpdate records an UpdateTransaction call in propose mode.
func (a *monarchAdapter) captureUpdate(id string, params *monarch.UpdateTransactionParams) error {
	if params == nil {
		return nil
	}
	if params.Amount != nil {
		return fmt.Errorf("propose mode: refusing to propose an amount change for %s", id)
	}
	w := a.capturedFor(id)
	if params.NeedsReview != nil && !*params.NeedsReview {
		w.markReviewed = true
	}
	if params.CategoryID != nil || params.Notes != nil {
		if w.kind == "" {
			w.kind = storage.ProposalKindCategory
		}
		if params.CategoryID != nil {
			w.categoryID = *params.CategoryID
		}
		if params.Notes != nil {
			w.notes, w.hasNotes = *params.Notes, true
		}
	}
	return nil
}

// captureSplits records an UpdateSplits call in propose mode.
func (a *monarchAdapter) captureSplits(id string, splits []*monarch.TransactionSplit) error {
	w := a.capturedFor(id)
	w.kind = storage.ProposalKindSplits
	w.splits = make([]*monarch.TransactionSplit, len(splits))
	for i, split := range splits {
		copied := *split
		w.splits[i] = &copied
	}
	return nil
}

func (a *monarchAdapter) capturedFor(id string) *capturedWrite {
	for _, w := range a.captured {
		if w.transactionID == id {
			if a.currentItems != nil {
				w.items = a.currentItems()
			}
			return w
		}
	}
	w := &capturedWrite{transactionID: id}
	if a.currentItems != nil {
		w.items = a.currentItems()
	}
	a.captured = append(a.captured, w)
	return w
}

// takeCaptured returns and clears the writes captured since the last call.
func (a *monarchAdapter) takeCaptured() []*capturedWrite {
	if a == nil {
		return nil
	}
	captured := a.captured
	a.captured = nil
	return captured
}

var chargePrefixPattern = regexp.MustCompile(`^(Charge \d+ of \d+ \([^)\n]*\)|Return of [^\n]*\(matched by price\))\n`)

// notePrefixOf extracts the context line handlers prepend to notes
// ("Charge i of N (...)" or "Return of ... (matched by price)").
func notePrefixOf(notes string) string {
	return chargePrefixPattern.FindString(notes)
}

// buildProposals turns an order's captured writes into proposals. Writes that
// set nothing (e.g. only a review flag on a transaction with no other change)
// are dropped.
func (o *Orchestrator) buildProposals(order providers.Order, captured []*capturedWrite) ([]*storage.Proposal, error) {
	var proposals []*storage.Proposal
	for _, w := range captured {
		if w.kind == "" {
			continue
		}
		snap, ok := o.monarchAdapter.snapshots[w.transactionID]
		if !ok {
			return nil, fmt.Errorf("propose: no snapshot of transaction %s", w.transactionID)
		}
		before, err := json.Marshal(snap)
		if err != nil {
			return nil, err
		}

		p := &storage.Proposal{
			RunID:         o.runID,
			OrderID:       order.GetID(),
			Provider:      order.GetProviderName(),
			TransactionID: w.transactionID,
			Kind:          w.kind,
			Status:        storage.ProposalPending,
			Amount:        snap.Amount,
			Before:        before,
			MarkReviewed:  w.markReviewed,
			OrderDate:     order.GetDate().Format("2006-01-02"),
			OrderTotal:    order.GetTotal(),
			Items:         proposalItems(w.items),
			Confidence:    averageConfidence(w.items),
		}

		switch w.kind {
		case storage.ProposalKindSplits:
			for _, split := range w.splits {
				p.Splits = append(p.Splits, storage.SplitDetail{
					CategoryID:   split.CategoryID,
					CategoryName: o.categoryName(split.CategoryID),
					Amount:       split.Amount,
					Notes:        split.Notes,
				})
			}
			if len(w.splits) > 0 {
				p.NotePrefix = notePrefixOf(w.splits[0].Notes)
			}
		default:
			p.CategoryID = w.categoryID
			p.CategoryName = o.categoryName(w.categoryID)
			p.Notes = w.notes
			p.NotePrefix = notePrefixOf(w.notes)
		}
		p.Reason = proposalReason(order, snap, p)
		proposals = append(proposals, p)
	}
	return proposals, nil
}

func (o *Orchestrator) categoryName(id string) string {
	if name, ok := o.categoryNames[id]; ok {
		return name
	}
	return id
}

func proposalItems(items []splitter.CategorizedItem) []storage.ProposalItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]storage.ProposalItem, len(items))
	for i, item := range items {
		out[i] = storage.ProposalItem{
			Name:         item.Name,
			Price:        item.Price,
			Quantity:     item.Quantity,
			CategoryID:   item.CategoryID,
			CategoryName: item.CategoryName,
			Confidence:   item.Confidence,
		}
	}
	return out
}

func averageConfidence(items []splitter.CategorizedItem) float64 {
	if len(items) == 0 {
		return 0
	}
	total := 0.0
	for _, item := range items {
		total += item.Confidence
	}
	return math.Round(total/float64(len(items))*100) / 100
}

// proposalReason explains, for the reviewer, why this transaction was chosen.
func proposalReason(order providers.Order, snap TransactionSnapshot, p *storage.Proposal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s order of %s ($%.2f) matched Monarch transaction of %s ($%.2f",
		strings.Title(order.GetProviderName()), order.GetDate().Format("2006-01-02"), order.GetTotal(), //nolint:staticcheck // ASCII provider names
		snap.Date, math.Abs(snap.Amount))
	if snap.Merchant != "" {
		fmt.Fprintf(&b, ", %s", snap.Merchant)
	}
	b.WriteString(")")
	if prefix := strings.TrimSpace(p.NotePrefix); prefix != "" {
		fmt.Fprintf(&b, "; %s", prefix)
	}
	if snap.Amount > 0 {
		b.WriteString("; refund")
	}
	return b.String()
}
