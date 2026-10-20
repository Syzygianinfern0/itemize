package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	gosync "sync"
	"time"

	"github.com/eshaffer321/itemize/internal/infrastructure/storage"
	"github.com/eshaffer321/monarch-go/v2/pkg/monarch"
)

// ApplyOutcome is what happened to one proposal during apply.
type ApplyOutcome struct {
	ProposalID    int64  `json:"proposal_id"`
	OrderID       string `json:"order_id"`
	TransactionID string `json:"transaction_id"`
	Status        string `json:"status"` // applied, stale, failed, skipped
	Error         string `json:"error,omitempty"`
}

// ApplyResult summarizes an apply run.
type ApplyResult struct {
	RunID    int64          `json:"run_id"`
	Applied  int            `json:"applied"`
	Stale    int            `json:"stale"`
	Failed   int            `json:"failed"`
	Skipped  int            `json:"skipped"`
	Outcomes []ApplyOutcome `json:"outcomes"`
}

// Applier writes approved proposals to Monarch. Every write first re-reads
// the target transaction and is refused if it no longer matches the
// proposal's before-snapshot (the proposal is then marked stale).
type Applier struct {
	client *monarch.Client
	store  storage.Repository
	logger *slog.Logger
	mu     gosync.Mutex
}

// NewApplier creates an Applier.
func NewApplier(client *monarch.Client, store storage.Repository, logger *slog.Logger) *Applier {
	if logger == nil {
		logger = slog.Default()
	}
	return &Applier{client: client, store: store, logger: logger}
}

// Apply writes the given approved proposals (all approved proposals when ids
// is empty). Proposals that are not approved are skipped. Only one Apply runs
// at a time.
func (a *Applier) Apply(ctx context.Context, ids []int64) (*ApplyResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.client == nil || a.store == nil {
		return nil, errors.New("apply needs a Monarch client and storage")
	}

	proposals, err := a.selectProposals(ids)
	if err != nil {
		return nil, err
	}

	result := &ApplyResult{Outcomes: []ApplyOutcome{}}
	if len(proposals) == 0 {
		return result, nil
	}

	runID, err := a.store.StartSyncRun("apply", 0, false)
	if err != nil {
		return nil, fmt.Errorf("start apply run: %w", err)
	}
	result.RunID = runID
	if err := a.store.SetSyncRunMode(runID, "apply"); err != nil {
		a.logger.Warn("Failed to record apply run mode", "error", err)
	}

	touchedOrders := map[string]bool{}
	for _, p := range proposals {
		outcome := a.applyOne(ctx, runID, p)
		result.Outcomes = append(result.Outcomes, outcome)
		switch outcome.Status {
		case storage.ProposalApplied:
			result.Applied++
			touchedOrders[p.OrderID] = true
		case storage.ProposalStale:
			result.Stale++
		case storage.ProposalFailed:
			result.Failed++
		default:
			result.Skipped++
		}
	}

	orders := make([]string, 0, len(touchedOrders))
	for orderID := range touchedOrders {
		orders = append(orders, orderID)
	}
	sort.Strings(orders)
	for _, orderID := range orders {
		if err := a.finalizeOrder(runID, orderID); err != nil {
			a.logger.Error("Failed to record applied order", "order_id", orderID, "error", err)
		}
	}

	if err := a.store.CompleteSyncRun(runID, len(proposals), result.Applied, result.Stale+result.Skipped, result.Failed); err != nil {
		a.logger.Warn("Failed to complete apply run", "error", err)
	}
	return result, nil
}

func (a *Applier) selectProposals(ids []int64) ([]*storage.Proposal, error) {
	if len(ids) == 0 {
		list, err := a.store.ListProposals(storage.ProposalFilters{Status: storage.ProposalApproved, Limit: 10000})
		if err != nil {
			return nil, err
		}
		return list.Proposals, nil
	}
	var out []*storage.Proposal
	for _, id := range ids {
		p, err := a.store.GetProposal(id)
		if err != nil {
			return nil, err
		}
		if p == nil {
			return nil, fmt.Errorf("proposal %d not found", id)
		}
		out = append(out, p)
	}
	return out, nil
}

func (a *Applier) applyOne(ctx context.Context, runID int64, p *storage.Proposal) ApplyOutcome {
	outcome := ApplyOutcome{ProposalID: p.ID, OrderID: p.OrderID, TransactionID: p.TransactionID}
	if p.Status != storage.ProposalApproved {
		outcome.Status = "skipped"
		outcome.Error = fmt.Sprintf("proposal is %s, not approved", p.Status)
		return outcome
	}

	var before TransactionSnapshot
	if err := json.Unmarshal(p.Before, &before); err != nil || before.ID != p.TransactionID {
		return a.finish(p, outcome, storage.ProposalFailed, fmt.Errorf("unusable before-snapshot: %v", err))
	}

	adapter := &monarchAdapter{
		client:       a.client,
		storage:      a.store,
		logger:       a.logger,
		runID:        runID,
		verifyWrites: true,
		snapshots:    map[string]TransactionSnapshot{p.TransactionID: before},
	}
	ctx = withAuditContext(ctx, p.OrderID, false)

	var err error
	switch p.Kind {
	case storage.ProposalKindSplits:
		err = a.writeSplits(ctx, adapter, p)
	case storage.ProposalKindCategory:
		err = a.writeCategory(ctx, adapter, p)
	default:
		err = fmt.Errorf("unknown proposal kind %q", p.Kind)
	}

	switch {
	case err == nil:
		a.logger.Info("Applied proposal", "proposal_id", p.ID, "order_id", p.OrderID, "transaction_id", p.TransactionID, "kind", p.Kind)
		return a.finish(p, outcome, storage.ProposalApplied, nil)
	case errors.Is(err, ErrTransactionChanged):
		a.logger.Warn("Proposal is stale; not applied", "proposal_id", p.ID, "transaction_id", p.TransactionID, "error", err)
		return a.finish(p, outcome, storage.ProposalStale, err)
	default:
		a.logger.Error("Failed to apply proposal", "proposal_id", p.ID, "transaction_id", p.TransactionID, "error", err)
		return a.finish(p, outcome, storage.ProposalFailed, err)
	}
}

func (a *Applier) writeSplits(ctx context.Context, adapter *monarchAdapter, p *storage.Proposal) error {
	if len(p.Splits) < 2 {
		return fmt.Errorf("splits proposal has %d split(s)", len(p.Splits))
	}
	total := 0.0
	splits := make([]*monarch.TransactionSplit, len(p.Splits))
	for i, s := range p.Splits {
		total += s.Amount
		splits[i] = &monarch.TransactionSplit{Amount: s.Amount, CategoryID: s.CategoryID, Notes: s.Notes}
	}
	if diff := total - p.Amount; diff > 0.005 || diff < -0.005 {
		return fmt.Errorf("splits total %.2f does not equal transaction amount %.2f", total, p.Amount)
	}
	if err := adapter.UpdateSplits(ctx, p.TransactionID, splits); err != nil {
		return err
	}
	if p.MarkReviewed {
		reviewed := false
		if err := adapter.UpdateTransaction(ctx, p.TransactionID, &monarch.UpdateTransactionParams{NeedsReview: &reviewed}); err != nil {
			a.logger.Warn("Applied splits but could not mark transaction reviewed", "transaction_id", p.TransactionID, "error", err)
		}
	}
	return nil
}

func (a *Applier) writeCategory(ctx context.Context, adapter *monarchAdapter, p *storage.Proposal) error {
	if p.CategoryID == "" {
		return errors.New("category proposal has no category")
	}
	categoryID, notes := p.CategoryID, p.Notes
	params := &monarch.UpdateTransactionParams{CategoryID: &categoryID, Notes: &notes}
	if p.MarkReviewed {
		reviewed := false
		params.NeedsReview = &reviewed
	}
	return adapter.UpdateTransaction(ctx, p.TransactionID, params)
}

func (a *Applier) finish(p *storage.Proposal, outcome ApplyOutcome, status string, err error) ApplyOutcome {
	outcome.Status = status
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
		outcome.Error = errMsg
	}
	if recErr := a.store.RecordProposalResult(p.ID, status, errMsg); recErr != nil {
		a.logger.Error("Failed to record proposal result", "proposal_id", p.ID, "error", recErr)
	}
	return outcome
}

// finalizeOrder marks an order processed once every proposal for it is either
// applied or rejected, so later syncs skip it (deduplication).
func (a *Applier) finalizeOrder(runID int64, orderID string) error {
	proposals, err := a.store.GetOrderProposals(orderID)
	if err != nil {
		return err
	}
	var applied []*storage.Proposal
	for _, p := range proposals {
		switch p.Status {
		case storage.ProposalApplied:
			applied = append(applied, p)
		case storage.ProposalRejected:
		default:
			return nil // still has open proposals
		}
	}
	if len(applied) == 0 {
		return nil
	}

	record, err := a.store.GetRecord(orderID)
	if err != nil {
		return err
	}
	if record == nil {
		record = &storage.ProcessingRecord{OrderID: orderID, Provider: applied[0].Provider}
	}
	first := applied[0]
	final := *record
	final.RunID = runID
	final.Status = "success"
	final.DryRun = false
	final.ErrorMessage = ""
	final.ProcessedAt = time.Now()
	final.TransactionID = first.TransactionID
	final.TransactionAmount = first.Amount
	final.Splits = first.Splits
	final.SplitCount = len(first.Splits)
	final.CategoryID = first.CategoryID
	final.CategoryName = first.CategoryName
	final.MonarchNotes = first.Notes
	if err := a.store.SaveRecord(&final); err != nil {
		return err
	}
	for _, p := range applied {
		if err := a.store.SaveOrderTransaction(&storage.OrderTransaction{
			RunID:         runID,
			OrderID:       orderID,
			TransactionID: p.TransactionID,
			Role:          "applied",
			Amount:        p.Amount,
			CategoryID:    p.CategoryID,
			CategoryName:  p.CategoryName,
			Notes:         p.Notes,
		}); err != nil {
			return err
		}
	}
	return nil
}
