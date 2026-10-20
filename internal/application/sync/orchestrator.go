package sync

import (
	"context"
	"fmt"
	"time"

	"github.com/eshaffer321/itemize/internal/adapters/providers"
	"github.com/eshaffer321/itemize/internal/application/sync/handlers"
	"github.com/eshaffer321/itemize/internal/domain/categorizer"
	"github.com/eshaffer321/itemize/internal/domain/splitter"
	"github.com/eshaffer321/monarch-go/v2/pkg/monarch"
)

// handleResult processes the result from a provider handler and records success/error
// Returns (processed, skipped, error) matching processOrder signature
func (o *Orchestrator) handleResult(order providers.Order, result *handlers.ProcessResult, err error, opts Options) (bool, bool, error) {
	noWrites := opts.DryRun || opts.Propose
	if o.proposing {
		captured := o.monarchAdapter.takeCaptured()
		if err == nil && result != nil && result.Processed {
			if proposeErr := o.saveProposals(order, captured); proposeErr != nil {
				err = proposeErr
			}
		}
	}
	if err != nil {
		o.logger.Error("Handler error", "order_id", order.GetID(), "error", err)
		o.recordError(order, err.Error(), nil, noWrites)
		return false, false, err
	}
	if result.Skipped {
		// Don't treat "payment pending" as an error - it's expected for new orders
		if result.SkipReason == "payment pending" {
			o.logger.Info("Order pending (awaiting shipment/charge)", "order_id", order.GetID())
			o.recordPending(order, result.SkipReason, noWrites)
			return false, true, nil
		}
		// Don't treat "already has splits" as an error - just skip silently
		if result.SkipReason == "transaction already has splits" {
			o.logger.Debug("Order skipped (already has splits)", "order_id", order.GetID())
			return false, true, nil
		}
		o.logger.Warn("Order skipped", "order_id", order.GetID(), "reason", result.SkipReason)
		o.recordError(order, result.SkipReason, result, noWrites)
		return false, false, fmt.Errorf("skipped: %s", result.SkipReason)
	}
	if result.Processed {
		// Pass the full result to capture audit trail data (category, notes, transaction, etc.)
		o.recordSuccessWithResult(order, result.Transaction, result.Splits, 0, noWrites, result, nil)
	}
	return result.Processed, result.Skipped, nil
}

// processOrder processes a single order, matching it to a transaction and creating splits
// Returns (processed, skipped, error)
func (o *Orchestrator) processOrder(
	ctx context.Context,
	order providers.Order,
	providerTransactions []*monarch.Transaction,
	usedTransactionIDs map[string]bool,
	catCategories []categorizer.Category,
	monarchCategories []*monarch.TransactionCategory,
	opts Options,
) (bool, bool, error) {
	noWrites := opts.DryRun || opts.Propose
	ctx = withAuditContext(ctx, order.GetID(), noWrites)
	o.currentOrderID = order.GetID()
	o.monarchAdapter.takeCaptured()

	o.logger.Debug("Processing order",
		"order_id", order.GetID(),
		"order_date", order.GetDate().Format("2006-01-02"),
		"order_total", order.GetTotal(),
		"item_count", len(order.GetItems()),
	)

	// A pending transaction may be replaced by a different posted transaction
	// ID. Resolve and verify the stored Monarch state before applying the
	// order-level deduplication guard.
	if !opts.Force && o.storage != nil {
		record, err := o.storage.GetRecord(order.GetID())
		if err != nil {
			return false, false, fmt.Errorf("load processing record: %w", err)
		}
		if record != nil {
			handled, processed, skipped, reconcileErr := o.reconcileStoredOrder(
				ctx,
				order,
				record,
				providerTransactions,
				usedTransactionIDs,
				noWrites,
			)
			if handled {
				return processed, skipped, reconcileErr
			}
		}
		if o.storage.IsProcessed(order.GetID()) {
			o.logger.Debug("Skipping already processed order", "order_id", order.GetID())
			return false, true, nil
		}
		if o.proposing {
			reviewed, err := o.storage.HasReviewedProposals(order.GetID())
			if err != nil {
				return false, false, fmt.Errorf("load proposals: %w", err)
			}
			if reviewed {
				o.logger.Debug("Skipping order with reviewed proposals (use -force to re-propose)", "order_id", order.GetID())
				return false, true, nil
			}
		}
	}

	// Use Amazon handler for Amazon orders (uses pro-rata allocation)
	if amazonOrder, ok := handlers.AsAmazonOrder(order); ok && o.amazonHandler != nil {
		o.logger.Debug("Using Amazon handler for order", "order_id", order.GetID())
		result, err := o.amazonHandler.ProcessOrder(ctx, amazonOrder, providerTransactions, usedTransactionIDs, catCategories, monarchCategories, opts.DryRun)
		return o.handleResult(order, result, err, opts)
	}

	// Use Walmart handler for Walmart orders (handles multi-delivery and gift cards)
	if walmartOrder, ok := handlers.AsWalmartOrder(order); ok && o.walmartHandler != nil {
		o.logger.Debug("Using Walmart handler for order", "order_id", order.GetID())
		result, err := o.walmartHandler.ProcessOrder(ctx, walmartOrder, providerTransactions, usedTransactionIDs, catCategories, monarchCategories, opts.DryRun)
		return o.handleResult(order, result, err, opts)
	}

	// Use Simple handler for all other providers (Costco, etc.)
	if o.simpleHandler != nil {
		o.logger.Debug("Using Simple handler for order", "order_id", order.GetID())
		result, err := o.simpleHandler.ProcessOrder(ctx, order, providerTransactions, usedTransactionIDs, catCategories, monarchCategories, opts.DryRun)
		return o.handleResult(order, result, err, opts)
	}

	// No handler available (testing mode without clients)
	o.logger.Warn("No handler available for order", "order_id", order.GetID())
	return false, true, nil
}

// Run executes the sync process for the configured provider
func (o *Orchestrator) Run(ctx context.Context, opts Options) (*Result, error) {
	result := &Result{
		Errors: make([]error, 0),
	}

	o.logger.Debug("Starting sync",
		"provider", o.provider.DisplayName(),
		"lookback_days", opts.LookbackDays,
		"max_orders", opts.MaxOrders,
		"dry_run", opts.DryRun,
		"force", opts.Force,
	)

	if err := o.configureMode(opts); err != nil {
		return nil, err
	}

	// 1. Start sync run tracking before external fetches so fetch logs are tied to a run.
	if o.storage != nil {
		var err error
		o.runID, err = o.storage.StartSyncRun(o.provider.DisplayName(), opts.LookbackDays, opts.DryRun || opts.Propose)
		if err != nil {
			o.logger.Warn("Failed to start sync run tracking", "error", err)
		}
		if o.runID > 0 {
			if err := o.storage.SetSyncRunMode(o.runID, runMode(opts)); err != nil {
				o.logger.Warn("Failed to record sync run mode", "error", err)
			}
		}
		if o.consolidator != nil {
			o.consolidator.SetRunID(o.runID)
		}
		if o.monarchAdapter != nil {
			o.monarchAdapter.runID = o.runID
		}
		// Set up ledger storage for Walmart handler
		if o.walmartHandler != nil {
			o.walmartHandler.SetLedgerStorage(&ledgerStorageAdapter{repo: o.storage}, o.runID)
		}
	}

	// 2. Fetch orders from provider
	orders, err := o.fetchOrders(ctx, opts)
	if err != nil {
		o.completeFailedRun(1)
		return nil, err
	}

	// 3. Fetch Monarch transactions (if clients configured)
	if o.clients == nil {
		return result, nil // Testing mode
	}

	providerTransactions, err := o.fetchMonarchTransactions(ctx, opts)
	if err != nil {
		o.completeFailedRun(1)
		return nil, err
	}

	// 4. Get Monarch categories
	catCategories, monarchCategories, err := o.fetchCategories(ctx)
	if err != nil {
		o.completeFailedRun(1)
		return nil, err
	}
	o.categoryNames = make(map[string]string, len(catCategories))
	for _, cat := range catCategories {
		o.categoryNames[cat.ID] = cat.Name
	}

	amazonReturns, returnsErr := o.fetchAmazonReturns(ctx)
	if returnsErr != nil {
		result.ErrorCount++
		result.Errors = append(result.Errors, returnsErr)
		o.logger.Error("Amazon return ledger unavailable; continuing with order sync", "error", returnsErr)
	}

	// 5. Process orders
	usedTransactionIDs := make(map[string]bool)

	for i, order := range orders {
		if opts.OrderID != "" && order.GetID() != opts.OrderID {
			o.logger.Debug("Skipping order (not matching -order-id filter)",
				"order_id", order.GetID(),
				"filter", opts.OrderID,
			)
			continue
		}

		o.logger.Debug("Processing order", "index", i+1, "total", len(orders))

		processed, skipped, err := o.processOrder(ctx, order, providerTransactions, usedTransactionIDs, catCategories, monarchCategories, opts)
		if err != nil {
			result.ErrorCount++
			result.Errors = append(result.Errors, fmt.Errorf("order %s (%s, $%.2f): %w",
				order.GetID(),
				order.GetDate().Format("2006-01-02"),
				order.GetTotal(),
				err))
			continue
		}

		if processed {
			result.ProcessedCount++
		}
		if skipped {
			result.SkippedCount++
		}
	}

	if returnsErr == nil && len(amazonReturns) > 0 {
		// Amazon refunds are not proposed yet; a propose run previews them.
		returnOpts := opts
		returnOpts.DryRun = opts.DryRun || opts.Propose
		o.processAmazonReturns(ctx, amazonReturns, providerTransactions, usedTransactionIDs, catCategories, monarchCategories, returnOpts, time.Now(), result)
	}

	// 6. Complete sync run
	if o.storage != nil && o.runID > 0 {
		if err := o.storage.CompleteSyncRun(o.runID, len(orders), result.ProcessedCount, result.SkippedCount, result.ErrorCount); err != nil {
			o.logger.Error("Failed to complete sync run", "run_id", o.runID, "error", err)
		}
	}

	return result, nil
}

func (o *Orchestrator) completeFailedRun(errorCount int) {
	if o.storage == nil || o.runID <= 0 {
		return
	}
	if err := o.storage.CompleteSyncRun(o.runID, 0, 0, 0, errorCount); err != nil {
		o.logger.Warn("Failed to complete failed sync run tracking", "error", err)
	}
}

// runMode names how a run was invoked, for the run history.
func runMode(opts Options) string {
	switch {
	case opts.Propose:
		return "propose"
	case opts.DryRun:
		return "dry-run"
	default:
		return "live"
	}
}

// configureMode sets up propose mode: the Monarch adapter captures writes,
// and destructive consolidation is off (it cannot be proposed).
func (o *Orchestrator) configureMode(opts Options) error {
	o.proposing = opts.Propose
	allowConsolidation := o.clients != nil && o.clients.AllowDestructiveWrites && !opts.Propose
	if o.walmartHandler != nil {
		o.walmartHandler.SetAllowConsolidation(allowConsolidation)
	}
	if o.amazonHandler != nil {
		o.amazonHandler.SetAllowConsolidation(allowConsolidation)
	}
	if !opts.Propose {
		if o.monarchAdapter != nil {
			o.monarchAdapter.proposing = false
		}
		return nil
	}
	if opts.DryRun {
		return fmt.Errorf("-propose and -dry-run cannot be combined: a propose run already makes no Monarch changes")
	}
	if o.clients != nil && o.monarchAdapter == nil {
		return fmt.Errorf("propose mode needs a Monarch client")
	}
	if o.monarchAdapter != nil {
		o.monarchAdapter.proposing = true
		o.monarchAdapter.currentItems = func() []splitter.CategorizedItem {
			if o.splitter == nil {
				return nil
			}
			return o.splitter.LastCategorized(o.currentOrderID)
		}
	}
	return nil
}

// saveProposals stores an order's captured writes as pending proposals,
// replacing any unreviewed ones from earlier propose runs.
func (o *Orchestrator) saveProposals(order providers.Order, captured []*capturedWrite) error {
	proposals, err := o.buildProposals(order, captured)
	if err != nil {
		return err
	}
	if o.storage == nil {
		return nil
	}
	if err := o.storage.ReplaceOrderProposals(order.GetID(), proposals); err != nil {
		return fmt.Errorf("save proposals: %w", err)
	}
	o.logger.Info("Proposed changes for review", "order_id", order.GetID(), "proposal_count", len(proposals))
	return nil
}
