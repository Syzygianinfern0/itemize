package sync

import (
	"context"
	"fmt"

	"github.com/eshaffer321/itemize/internal/adapters/providers"
	"github.com/eshaffer321/itemize/internal/application/sync/handlers"
	"github.com/eshaffer321/itemize/internal/domain/categorizer"
	"github.com/eshaffer321/itemize/internal/domain/returns"
	"github.com/eshaffer321/monarch-go/v2/pkg/monarch"
)

// processInferredReturns handles Walmart refunds that no order's ledger
// accounted for (typically in-store returns): each unused positive Walmart
// transaction is matched by price to a purchase in the return window and
// categorized like the returned items. Ambiguous or unmatched refunds are
// left untouched.
func (o *Orchestrator) processInferredReturns(
	ctx context.Context,
	orders []providers.Order,
	transactions []*monarch.Transaction,
	usedTxnIDs map[string]bool,
	catCategories []categorizer.Category,
	monarchCategories []*monarch.TransactionCategory,
	opts Options,
	result *Result,
) {
	if o.walmartHandler == nil || opts.OrderID != "" {
		return
	}
	var purchases []providers.Order
	for _, order := range orders {
		if _, ok := order.(handlers.WalmartOrder); ok && order.GetTotal() > 0 {
			purchases = append(purchases, order)
		}
	}
	if len(purchases) == 0 {
		return
	}

	noWrites := opts.DryRun || opts.Propose
	for _, refund := range transactions {
		if refund == nil || refund.Amount <= 0 || refund.Pending || usedTxnIDs[refund.ID] {
			continue
		}
		returnID := handlers.InferredReturnID(refund)
		if skip, err := o.alreadyHandled(returnID, opts); err != nil || skip {
			if err != nil {
				o.logger.Warn("Could not check refund history", "refund_transaction_id", refund.ID, "error", err)
			}
			continue
		}

		candidate, reason := returns.Pick(returns.DefaultFinder.Find(refund.Amount, refund.Date.Time, purchases))
		if candidate == nil {
			result.RefundSkippedCount++
			o.logger.Info("Walmart refund left untouched",
				"refund_transaction_id", refund.ID,
				"date", refund.Date.Format("2006-01-02"),
				"amount", refund.Amount,
				"reason", reason)
			continue
		}

		purchase := candidate.Order.(handlers.WalmartOrder)
		returnOrder := handlers.NewInferredReturnOrder(purchase, candidate.Items, refund)
		usedTxnIDs[refund.ID] = true

		o.currentOrderID = returnID
		o.monarchAdapter.takeCaptured()
		returnCtx := withAuditContext(ctx, returnID, noWrites)
		processResult, err := o.walmartHandler.ProcessInferredReturn(returnCtx, returnOrder, refund, purchase.GetDate(), catCategories, monarchCategories, opts.DryRun)
		processed, _, err := o.handleResult(returnOrder, processResult, err, opts)
		if err != nil {
			result.ErrorCount++
			result.Errors = append(result.Errors, fmt.Errorf("walmart refund %s (%s, $%.2f): %w",
				refund.ID, refund.Date.Format("2006-01-02"), refund.Amount, err))
			continue
		}
		if processed {
			result.RefundProcessedCount++
		}
	}
}

// alreadyHandled reports whether a record ID was processed (or, in propose
// mode, already reviewed), unless -force.
func (o *Orchestrator) alreadyHandled(recordID string, opts Options) (bool, error) {
	if opts.Force || o.storage == nil {
		return false, nil
	}
	if o.storage.IsProcessed(recordID) {
		return true, nil
	}
	if o.proposing {
		return o.storage.HasReviewedProposals(recordID)
	}
	return false, nil
}
