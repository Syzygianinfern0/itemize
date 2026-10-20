package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/eshaffer321/itemize/internal/adapters/clients"
	"github.com/eshaffer321/itemize/internal/application/sync"
	"github.com/eshaffer321/itemize/internal/infrastructure/config"
	"github.com/eshaffer321/itemize/internal/infrastructure/logging"
	"github.com/eshaffer321/itemize/internal/infrastructure/storage"
)

// RunProposals lists proposals and approves or rejects them by ID.
//
//	itemize proposals [-status pending|approved|...|all] [-order-id ID]
//	itemize proposals -approve 3,4 -reject 5
func RunProposals(cfg *config.Config, args []string, _ io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("proposals", flag.ContinueOnError)
	status := fs.String("status", storage.ProposalPending, "Filter by status: pending, approved, rejected, applied, failed, stale, or all")
	orderID := fs.String("order-id", "", "Only this order")
	approve := fs.String("approve", "", "Comma-separated proposal IDs to approve")
	reject := fs.String("reject", "", "Comma-separated proposal IDs to reject")
	reset := fs.String("reset", "", "Comma-separated IDs to move back to pending (from approved, rejected, stale or failed)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	store, err := storage.NewStorage(cfg.Storage.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	changes := []struct {
		ids, to string
		from    []string
	}{
		{*approve, storage.ProposalApproved, []string{storage.ProposalPending}},
		{*reject, storage.ProposalRejected, []string{storage.ProposalPending, storage.ProposalApproved}},
		{*reset, storage.ProposalPending, []string{storage.ProposalApproved, storage.ProposalRejected, storage.ProposalStale, storage.ProposalFailed}},
	}
	changed := false
	for _, change := range changes {
		if change.ids == "" {
			continue
		}
		ids, err := parseIDs(change.ids)
		if err != nil {
			return err
		}
		n, err := store.SetProposalStatus(ids, change.to, change.from)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "%d of %d proposal(s) marked %s\n", n, len(ids), change.to)
		changed = true
	}
	if changed {
		return nil
	}

	filter := storage.ProposalFilters{Status: *status, OrderID: *orderID, Limit: 10000}
	if filter.Status == "all" {
		filter.Status = ""
	}
	list, err := store.ListProposals(filter)
	if err != nil {
		return err
	}
	PrintProposals(out, list.Proposals)
	return nil
}

// PrintProposals writes a compact table of proposals.
func PrintProposals(out io.Writer, proposals []*storage.Proposal) {
	if len(proposals) == 0 {
		_, _ = fmt.Fprintln(out, "No proposals.")
		return
	}
	_, _ = fmt.Fprintf(out, "%-5s %-9s %-10s %-22s %10s  %s\n", "ID", "STATUS", "DATE", "ORDER", "AMOUNT", "CHANGE")
	for _, p := range proposals {
		_, _ = fmt.Fprintf(out, "%-5d %-9s %-10s %-22s %10.2f  %s\n", p.ID, p.Status, p.OrderDate, p.OrderID, p.Amount, proposalSummary(p))
		if p.Error != "" {
			_, _ = fmt.Fprintf(out, "      ! %s\n", p.Error)
		}
	}
}

func proposalSummary(p *storage.Proposal) string {
	if p.Kind == storage.ProposalKindSplits {
		parts := make([]string, len(p.Splits))
		for i, s := range p.Splits {
			parts[i] = fmt.Sprintf("%s %.2f", nameOr(s.CategoryName, s.CategoryID), s.Amount)
		}
		return fmt.Sprintf("split: %s", strings.Join(parts, " | "))
	}
	return "category: " + nameOr(p.CategoryName, p.CategoryID)
}

func nameOr(name, id string) string {
	if name != "" {
		return name
	}
	return id
}

// RunApply writes approved proposals to Monarch after showing exactly what
// will be written and asking for confirmation.
//
//	itemize apply [-id 3,4] [-yes]
func RunApply(cfg *config.Config, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	idList := fs.String("id", "", "Comma-separated proposal IDs (default: all approved)")
	yes := fs.Bool("yes", false, "Skip the confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}

	store, err := storage.NewStorage(cfg.Storage.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	var ids []int64
	var targets []*storage.Proposal
	if *idList != "" {
		if ids, err = parseIDs(*idList); err != nil {
			return err
		}
		for _, id := range ids {
			p, err := store.GetProposal(id)
			if err != nil {
				return err
			}
			if p == nil {
				return fmt.Errorf("proposal %d not found", id)
			}
			if p.Status != storage.ProposalApproved {
				return fmt.Errorf("proposal %d is %s; approve it first (itemize proposals -approve %d)", id, p.Status, id)
			}
			targets = append(targets, p)
		}
	} else {
		list, err := store.ListProposals(storage.ProposalFilters{Status: storage.ProposalApproved, Limit: 10000})
		if err != nil {
			return err
		}
		targets = list.Proposals
		for _, p := range targets {
			ids = append(ids, p.ID)
		}
	}
	if len(targets) == 0 {
		_, _ = fmt.Fprintln(out, "No approved proposals to apply.")
		return nil
	}

	_, _ = fmt.Fprintf(out, "These %d change(s) will be written to Monarch:\n\n", len(targets))
	for _, p := range targets {
		PrintWritePlan(out, p)
	}
	if !*yes {
		_, _ = fmt.Fprintf(out, "Type 'apply' to write %d change(s): ", len(targets))
		answer, _ := bufio.NewReader(in).ReadString('\n')
		if strings.TrimSpace(answer) != "apply" {
			_, _ = fmt.Fprintln(out, "Cancelled; nothing was written.")
			return nil
		}
	}

	client, err := clients.NewMonarchClient(cfg)
	if err != nil {
		return err
	}
	logger := logging.NewLoggerWithSystem(cfg.Observability.Logging, "apply")
	result, err := sync.NewApplier(client, store, logger).Apply(context.Background(), ids)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "\nApplied=%d Stale=%d Failed=%d Skipped=%d\n", result.Applied, result.Stale, result.Failed, result.Skipped)
	for _, o := range result.Outcomes {
		if o.Status != storage.ProposalApplied {
			_, _ = fmt.Fprintf(out, "  #%d %s: %s\n", o.ProposalID, o.Status, o.Error)
		}
	}
	return nil
}

// PrintWritePlan describes exactly what applying p writes.
func PrintWritePlan(out io.Writer, p *storage.Proposal) {
	_, _ = fmt.Fprintf(out, "#%d  order %s (%s)  transaction %s  amount %.2f (unchanged)\n", p.ID, p.OrderID, p.OrderDate, p.TransactionID, p.Amount)
	if p.Kind == storage.ProposalKindSplits {
		for _, s := range p.Splits {
			_, _ = fmt.Fprintf(out, "      split %10.2f  %s\n", s.Amount, nameOr(s.CategoryName, s.CategoryID))
		}
	} else {
		_, _ = fmt.Fprintf(out, "      category -> %s\n", nameOr(p.CategoryName, p.CategoryID))
		_, _ = fmt.Fprintf(out, "      notes    -> %s\n", strings.ReplaceAll(p.Notes, "\n", " / "))
	}
	if p.MarkReviewed {
		_, _ = fmt.Fprintln(out, "      mark reviewed")
	}
	_, _ = fmt.Fprintln(out)
}

func parseIDs(list string) ([]int64, error) {
	var ids []int64
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid proposal ID %q", part)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no proposal IDs given")
	}
	return ids, nil
}
