package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	gosync "sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/eshaffer321/itemize/internal/api/dto"
	appsync "github.com/eshaffer321/itemize/internal/application/sync"
	"github.com/eshaffer321/itemize/internal/domain/splitter"
	"github.com/eshaffer321/itemize/internal/infrastructure/storage"
	"github.com/eshaffer321/monarch-go/v2/pkg/monarch"
)

// ReviewHandler serves the review UI: proposals, edits, approval and apply.
// Nothing here writes to Monarch except Apply, which only takes proposals the
// owner approved, listed by ID, and re-validates each before writing.
type ReviewHandler struct {
	*Base
	monarch *monarch.Client
	applier *appsync.Applier

	mu        gosync.Mutex
	applyJobs map[string]*ApplyJob
	jobSeq    int
}

// ApplyJob is a background apply of approved proposals.
type ApplyJob struct {
	ID          string               `json:"id"`
	Status      string               `json:"status"` // running, completed, failed
	ProposalIDs []int64              `json:"proposal_ids"`
	StartedAt   string               `json:"started_at"`
	CompletedAt string               `json:"completed_at,omitempty"`
	Result      *appsync.ApplyResult `json:"result,omitempty"`
	Error       string               `json:"error,omitempty"`
}

// NewReviewHandler creates a review handler. monarchClient may be nil, in
// which case categories come from stored proposals and apply is unavailable.
func NewReviewHandler(repo storage.Repository, monarchClient *monarch.Client, applier *appsync.Applier) *ReviewHandler {
	return &ReviewHandler{
		Base:      NewBase(repo),
		monarch:   monarchClient,
		applier:   applier,
		applyJobs: map[string]*ApplyJob{},
	}
}

// ListProposals handles GET /api/proposals.
func (h *ReviewHandler) ListProposals(w http.ResponseWriter, r *http.Request) {
	result, err := h.repo.ListProposals(storage.ProposalFilters{
		Status:   r.URL.Query().Get("status"),
		OrderID:  r.URL.Query().Get("order_id"),
		Provider: r.URL.Query().Get("provider"),
		Limit:    ParseIntParam(r, "limit", 500),
		Offset:   ParseIntParam(r, "offset", 0),
	})
	if err != nil {
		h.WriteError(w, http.StatusInternalServerError, dto.InternalError())
		return
	}
	h.WriteJSON(w, http.StatusOK, result)
}

// GetProposal handles GET /api/proposals/{id}.
func (h *ReviewHandler) GetProposal(w http.ResponseWriter, r *http.Request) {
	p, ok := h.loadProposal(w, r)
	if ok {
		h.WriteJSON(w, http.StatusOK, p)
	}
}

func (h *ReviewHandler) loadProposal(w http.ResponseWriter, r *http.Request) (*storage.Proposal, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.WriteError(w, http.StatusBadRequest, dto.BadRequestError("invalid proposal ID"))
		return nil, false
	}
	p, err := h.repo.GetProposal(id)
	if err != nil {
		h.WriteError(w, http.StatusInternalServerError, dto.InternalError())
		return nil, false
	}
	if p == nil {
		h.WriteError(w, http.StatusNotFound, dto.NotFoundError("proposal"))
		return nil, false
	}
	return p, true
}

// DecisionRequest approves, rejects or resets proposals.
type DecisionRequest struct {
	IDs    []int64 `json:"ids"`
	Action string  `json:"action"` // approve, reject, reset
}

// Decide handles POST /api/proposals/decision.
func (h *ReviewHandler) Decide(w http.ResponseWriter, r *http.Request) {
	var req DecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.IDs) == 0 {
		h.WriteError(w, http.StatusBadRequest, dto.BadRequestError("ids and action are required"))
		return
	}
	var to string
	var from []string
	switch req.Action {
	case "approve":
		to, from = storage.ProposalApproved, []string{storage.ProposalPending}
	case "reject":
		to, from = storage.ProposalRejected, []string{storage.ProposalPending, storage.ProposalApproved}
	case "reset":
		to, from = storage.ProposalPending, []string{storage.ProposalApproved, storage.ProposalRejected, storage.ProposalStale, storage.ProposalFailed}
	default:
		h.WriteError(w, http.StatusBadRequest, dto.BadRequestError("action must be approve, reject or reset"))
		return
	}
	n, err := h.repo.SetProposalStatus(req.IDs, to, from)
	if err != nil {
		h.WriteError(w, http.StatusInternalServerError, dto.InternalError())
		return
	}
	h.WriteJSON(w, http.StatusOK, map[string]any{"changed": n, "requested": len(req.IDs), "status": to})
}

// EditItemsRequest assigns a category to each of a proposal's items, by index.
type EditItemsRequest struct {
	Categories []string `json:"categories"`
}

// EditItems handles PUT /api/proposals/{id}/items: re-categorize items of a
// pending proposal and recompute its category or splits the way sync would.
func (h *ReviewHandler) EditItems(w http.ResponseWriter, r *http.Request) {
	p, ok := h.loadProposal(w, r)
	if !ok {
		return
	}
	if p.Status != storage.ProposalPending {
		h.WriteError(w, http.StatusConflict, dto.NewAPIError("not_pending", "only pending proposals can be edited"))
		return
	}
	if len(p.Items) == 0 {
		h.WriteError(w, http.StatusBadRequest, dto.BadRequestError("this proposal has no item list to edit"))
		return
	}
	var req EditItemsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Categories) != len(p.Items) {
		h.WriteError(w, http.StatusBadRequest, dto.BadRequestError(fmt.Sprintf("categories must list one category ID per item (%d)", len(p.Items))))
		return
	}

	names, err := h.categoryNames(r.Context())
	if err != nil {
		h.WriteError(w, http.StatusBadGateway, dto.NewAPIError("categories_unavailable", err.Error()))
		return
	}
	items := make([]splitter.CategorizedItem, len(p.Items))
	for i, item := range p.Items {
		categoryID := req.Categories[i]
		name, known := names[categoryID]
		if !known {
			h.WriteError(w, http.StatusBadRequest, dto.BadRequestError(fmt.Sprintf("unknown category %q", categoryID)))
			return
		}
		confidence := item.Confidence
		if categoryID != item.CategoryID {
			confidence = 1 // set by the owner
		}
		items[i] = splitter.CategorizedItem{
			Name: item.Name, Price: item.Price, Quantity: item.Quantity,
			CategoryID: categoryID, CategoryName: name, Confidence: confidence,
		}
	}

	plan, err := splitter.PlanTransaction(items, p.Amount, p.NotePrefix)
	if err != nil {
		h.WriteError(w, http.StatusBadRequest, dto.BadRequestError(err.Error()))
		return
	}
	applyPlan(p, plan, names)
	p.Items = make([]storage.ProposalItem, len(items))
	for i, item := range items {
		p.Items[i] = storage.ProposalItem{
			Name: item.Name, Price: item.Price, Quantity: item.Quantity,
			CategoryID: item.CategoryID, CategoryName: item.CategoryName, Confidence: item.Confidence,
		}
	}
	if err := h.repo.UpdateProposalContent(p); err != nil {
		h.WriteError(w, http.StatusConflict, dto.NewAPIError("update_failed", err.Error()))
		return
	}
	updated, err := h.repo.GetProposal(p.ID)
	if err != nil || updated == nil {
		h.WriteError(w, http.StatusInternalServerError, dto.InternalError())
		return
	}
	h.WriteJSON(w, http.StatusOK, updated)
}

// applyPlan copies a recomputed plan into a proposal.
func applyPlan(p *storage.Proposal, plan *splitter.Plan, names map[string]string) {
	p.Splits = nil
	p.CategoryID, p.CategoryName, p.Notes = "", "", ""
	if plan.Kind == splitter.PlanSplits {
		p.Kind = storage.ProposalKindSplits
		for _, split := range plan.Splits {
			p.Splits = append(p.Splits, storage.SplitDetail{
				CategoryID: split.CategoryID, CategoryName: names[split.CategoryID], Amount: split.Amount, Notes: split.Notes,
			})
		}
		return
	}
	p.Kind = storage.ProposalKindCategory
	p.CategoryID, p.CategoryName, p.Notes = plan.CategoryID, plan.CategoryName, plan.Notes
}

// CategoryResponse is one Monarch category.
type CategoryResponse struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Group string `json:"group,omitempty"`
}

// Categories handles GET /api/categories.
func (h *ReviewHandler) Categories(w http.ResponseWriter, r *http.Request) {
	list, err := h.categories(r.Context())
	if err != nil {
		h.WriteError(w, http.StatusBadGateway, dto.NewAPIError("categories_unavailable", err.Error()))
		return
	}
	h.WriteJSON(w, http.StatusOK, map[string]any{"categories": list})
}

func (h *ReviewHandler) categories(ctx context.Context) ([]CategoryResponse, error) {
	var out []CategoryResponse
	if h.monarch != nil && h.monarch.Transactions != nil {
		cats, err := h.monarch.Transactions.Categories().List(ctx)
		if err != nil {
			return nil, fmt.Errorf("load Monarch categories: %w", err)
		}
		for _, c := range cats {
			if c == nil || c.IsDisabled {
				continue
			}
			resp := CategoryResponse{ID: c.ID, Name: c.Name}
			if c.Group != nil {
				resp.Group = c.Group.Name
			}
			out = append(out, resp)
		}
	} else {
		// Offline: offer the categories already used by proposals.
		seen := map[string]bool{}
		list, err := h.repo.ListProposals(storage.ProposalFilters{Limit: 100000})
		if err != nil {
			return nil, err
		}
		for _, p := range list.Proposals {
			for _, item := range p.Items {
				if item.CategoryID != "" && !seen[item.CategoryID] {
					seen[item.CategoryID] = true
					out = append(out, CategoryResponse{ID: item.CategoryID, Name: item.CategoryName})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (h *ReviewHandler) categoryNames(ctx context.Context) (map[string]string, error) {
	list, err := h.categories(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(list))
	for _, c := range list {
		names[c.ID] = c.Name
	}
	return names, nil
}

// IDsRequest lists proposal IDs.
type IDsRequest struct {
	IDs []int64 `json:"ids"`
}

// WritePlanEntry describes exactly what applying one proposal writes.
type WritePlanEntry struct {
	ProposalID    int64                 `json:"proposal_id"`
	OrderID       string                `json:"order_id"`
	TransactionID string                `json:"transaction_id"`
	Amount        float64               `json:"amount"`
	Kind          string                `json:"kind"`
	CategoryID    string                `json:"category_id,omitempty"`
	CategoryName  string                `json:"category_name,omitempty"`
	Notes         string                `json:"notes,omitempty"`
	Splits        []storage.SplitDetail `json:"splits,omitempty"`
	MarkReviewed  bool                  `json:"mark_reviewed"`
	Before        json.RawMessage       `json:"before"`
}

// ApplyPlan handles POST /api/apply/plan: the exact writes applying the given
// approved proposals would make. Unapproved IDs are rejected.
func (h *ReviewHandler) ApplyPlan(w http.ResponseWriter, r *http.Request) {
	proposals, ok := h.approvedProposals(w, r)
	if !ok {
		return
	}
	plan := make([]WritePlanEntry, len(proposals))
	for i, p := range proposals {
		plan[i] = WritePlanEntry{
			ProposalID: p.ID, OrderID: p.OrderID, TransactionID: p.TransactionID, Amount: p.Amount, Kind: p.Kind,
			CategoryID: p.CategoryID, CategoryName: p.CategoryName, Notes: p.Notes, Splits: p.Splits,
			MarkReviewed: p.MarkReviewed, Before: p.Before,
		}
	}
	h.WriteJSON(w, http.StatusOK, map[string]any{"writes": plan, "count": len(plan)})
}

func (h *ReviewHandler) approvedProposals(w http.ResponseWriter, r *http.Request) ([]*storage.Proposal, bool) {
	var req IDsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.IDs) == 0 {
		h.WriteError(w, http.StatusBadRequest, dto.BadRequestError("ids is required: list the approved proposals to apply"))
		return nil, false
	}
	var out []*storage.Proposal
	for _, id := range req.IDs {
		p, err := h.repo.GetProposal(id)
		if err != nil {
			h.WriteError(w, http.StatusInternalServerError, dto.InternalError())
			return nil, false
		}
		if p == nil {
			h.WriteError(w, http.StatusNotFound, dto.NotFoundError(fmt.Sprintf("proposal %d", id)))
			return nil, false
		}
		if p.Status != storage.ProposalApproved {
			h.WriteError(w, http.StatusConflict, dto.NewAPIError("not_approved", fmt.Sprintf("proposal %d is %s, not approved", id, p.Status)))
			return nil, false
		}
		out = append(out, p)
	}
	return out, true
}

// Apply handles POST /api/apply: starts writing the listed approved
// proposals to Monarch in the background. Poll GET /api/apply/{id}.
func (h *ReviewHandler) Apply(w http.ResponseWriter, r *http.Request) {
	if h.applier == nil {
		h.WriteError(w, http.StatusServiceUnavailable, dto.NewAPIError("apply_unavailable", "no Monarch client is configured"))
		return
	}
	proposals, ok := h.approvedProposals(w, r)
	if !ok {
		return
	}
	ids := make([]int64, len(proposals))
	for i, p := range proposals {
		ids[i] = p.ID
	}

	h.mu.Lock()
	for _, job := range h.applyJobs {
		if job.Status == "running" {
			h.mu.Unlock()
			h.WriteError(w, http.StatusConflict, dto.NewAPIError("apply_running", "an apply is already running"))
			return
		}
	}
	h.jobSeq++
	job := &ApplyJob{
		ID: fmt.Sprintf("apply-%d-%d", time.Now().Unix(), h.jobSeq), Status: "running",
		ProposalIDs: ids, StartedAt: time.Now().Format(time.RFC3339),
	}
	h.applyJobs[job.ID] = job
	h.mu.Unlock()

	go func() {
		result, err := h.applier.Apply(context.Background(), ids)
		h.mu.Lock()
		defer h.mu.Unlock()
		job.CompletedAt = time.Now().Format(time.RFC3339)
		if err != nil {
			job.Status, job.Error = "failed", err.Error()
			return
		}
		job.Status, job.Result = "completed", result
	}()

	h.WriteJSON(w, http.StatusAccepted, job)
}

// GetApplyJob handles GET /api/apply/{jobId}.
func (h *ReviewHandler) GetApplyJob(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	job, ok := h.applyJobs[chi.URLParam(r, "jobId")]
	if !ok {
		h.WriteError(w, http.StatusNotFound, dto.NotFoundError("apply job"))
		return
	}
	copied := *job
	h.WriteJSON(w, http.StatusOK, copied)
}

// ReviewOrderRow is one order in the review UI's order list.
type ReviewOrderRow struct {
	OrderID       string         `json:"order_id"`
	Provider      string         `json:"provider"`
	OrderDate     string         `json:"order_date"`
	OrderTotal    float64        `json:"order_total"`
	Status        string         `json:"status"`
	Reason        string         `json:"reason,omitempty"`
	DryRun        bool           `json:"dry_run"`
	OrderType     string         `json:"order_type"` // in-store, online, return
	ChargeCount   int            `json:"charge_count"`
	ItemCount     int            `json:"item_count"`
	TransactionID string         `json:"transaction_id,omitempty"`
	ProcessedAt   string         `json:"processed_at"`
	Proposals     map[string]int `json:"proposals"` // count by status
}

// ListOrders handles GET /api/review/orders: every recorded order with its
// type, charge count and proposal counts, for filtering in the UI.
func (h *ReviewHandler) ListOrders(w http.ResponseWriter, r *http.Request) {
	var records []*storage.ProcessingRecord
	for offset := 0; ; offset += 500 {
		page, err := h.repo.ListOrders(storage.OrderFilters{
			Provider: r.URL.Query().Get("provider"), Limit: 500, Offset: offset, OrderBy: "date", OrderDesc: true,
		})
		if err != nil {
			h.WriteError(w, http.StatusInternalServerError, dto.InternalError())
			return
		}
		records = append(records, page.Orders...)
		if len(page.Orders) < 500 {
			break
		}
	}

	proposalCounts := map[string]map[string]int{}
	if all, err := h.repo.ListProposals(storage.ProposalFilters{Limit: 100000}); err == nil {
		for _, p := range all.Proposals {
			if proposalCounts[p.OrderID] == nil {
				proposalCounts[p.OrderID] = map[string]int{}
			}
			proposalCounts[p.OrderID][p.Status]++
		}
	}

	rows := make([]ReviewOrderRow, 0, len(records))
	for _, rec := range records {
		row := ReviewOrderRow{
			OrderID: rec.OrderID, Provider: rec.Provider, OrderDate: formatDate(rec.OrderDate), OrderTotal: rec.OrderTotal,
			Status: rec.Status, Reason: rec.ErrorMessage, DryRun: rec.DryRun, ItemCount: rec.ItemCount,
			TransactionID: rec.TransactionID, ProcessedAt: rec.ProcessedAt.Format(time.RFC3339),
			OrderType: orderType(rec), Proposals: proposalCounts[rec.OrderID],
		}
		if ledger, err := h.repo.GetLatestLedger(rec.OrderID); err == nil && ledger != nil {
			row.ChargeCount = ledger.ChargeCount
		}
		rows = append(rows, row)
	}
	h.WriteJSON(w, http.StatusOK, map[string]any{"orders": rows, "count": len(rows)})
}

func formatDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02")
}

func orderType(rec *storage.ProcessingRecord) string {
	if strings.HasPrefix(rec.OrderID, "return:") {
		return "return"
	}
	var raw struct {
		Type string `json:"type"`
	}
	if rec.RawOrderJSON != "" && json.Unmarshal([]byte(rec.RawOrderJSON), &raw) == nil {
		switch raw.Type {
		case "IN_STORE":
			return "in-store"
		case "":
		default:
			return "online"
		}
	}
	return "unknown"
}

// OrderReview handles GET /api/review/orders/{id}: everything the order
// detail screen shows.
func (h *ReviewHandler) OrderReview(w http.ResponseWriter, r *http.Request) {
	orderID := chi.URLParam(r, "id")
	record, err := h.repo.GetRecord(orderID)
	if err != nil {
		h.WriteError(w, http.StatusInternalServerError, dto.InternalError())
		return
	}
	if record == nil {
		h.WriteError(w, http.StatusNotFound, dto.NotFoundError("order"))
		return
	}
	proposals, _ := h.repo.GetOrderProposals(orderID)
	ledger, _ := h.repo.GetLatestLedger(orderID)
	transactions, _ := h.repo.GetOrderTransactions(orderID)
	calls, _ := h.repo.GetAPICallsByOrderID(orderID)

	writes := make([]map[string]any, 0)
	for _, c := range calls {
		if c.Method == "Transactions.Get" || c.Method == "Transactions.GetSplits" {
			continue
		}
		writes = append(writes, map[string]any{
			"timestamp": c.Timestamp, "method": c.Method, "phase": c.Phase, "transaction_id": c.TransactionID,
			"dry_run": c.DryRun, "error": c.Error, "request": rawJSON(c.RequestJSON),
		})
	}

	h.WriteJSON(w, http.StatusOK, map[string]any{
		"order": map[string]any{
			"order_id": record.OrderID, "provider": record.Provider, "order_date": formatDate(record.OrderDate),
			"order_total": record.OrderTotal, "order_subtotal": record.OrderSubtotal, "order_tax": record.OrderTax,
			"order_tip": record.OrderTip, "status": record.Status, "reason": record.ErrorMessage, "dry_run": record.DryRun,
			"order_type": orderType(record), "items": record.Items, "transaction_id": record.TransactionID,
			"category_name": record.CategoryName, "monarch_notes": record.MonarchNotes, "splits": record.Splits,
		},
		"proposals":    nonNilProposals(proposals),
		"ledger":       ledger,
		"transactions": transactions,
		"api_calls":    writes,
	})
}

func nonNilProposals(p []*storage.Proposal) []*storage.Proposal {
	if p == nil {
		return []*storage.Proposal{}
	}
	return p
}

func rawJSON(s string) json.RawMessage {
	if s == "" || !json.Valid([]byte(s)) {
		return json.RawMessage("null")
	}
	return json.RawMessage(s)
}

// AppliedEntry is one write itemize made to Monarch.
type AppliedEntry struct {
	Timestamp     string            `json:"timestamp"`
	RunID         int64             `json:"run_id"`
	OrderID       string            `json:"order_id"`
	TransactionID string            `json:"transaction_id"`
	Method        string            `json:"method"`
	Phase         string            `json:"phase"`
	Error         string            `json:"error,omitempty"`
	Request       json.RawMessage   `json:"request"`
	Proposal      *storage.Proposal `json:"proposal,omitempty"` // with its before-state, if the write came from one
}

// Applied handles GET /api/applied: completed writes to Monarch, newest
// first, each with the proposal (and before-state) it came from.
func (h *ReviewHandler) Applied(w http.ResponseWriter, r *http.Request) {
	calls, err := h.repo.ListWriteCalls(ParseIntParam(r, "limit", 300))
	if err != nil {
		h.WriteError(w, http.StatusInternalServerError, dto.InternalError())
		return
	}
	proposalsByOrder := map[string][]*storage.Proposal{}
	entries := make([]AppliedEntry, 0, len(calls))
	for _, c := range calls {
		if c.Phase == "intent" {
			continue
		}
		entry := AppliedEntry{
			Timestamp: c.Timestamp, RunID: c.RunID, OrderID: c.OrderID, TransactionID: c.TransactionID,
			Method: c.Method, Phase: c.Phase, Error: c.Error, Request: rawJSON(c.RequestJSON),
		}
		if _, loaded := proposalsByOrder[c.OrderID]; !loaded {
			proposalsByOrder[c.OrderID], _ = h.repo.GetOrderProposals(c.OrderID)
		}
		for _, p := range proposalsByOrder[c.OrderID] {
			if p.TransactionID == c.TransactionID && p.Status == storage.ProposalApplied {
				entry.Proposal = p
			}
		}
		entries = append(entries, entry)
	}
	h.WriteJSON(w, http.StatusOK, map[string]any{"writes": entries, "count": len(entries)})
}

// Overview handles GET /api/review/overview: proposal counts by status.
func (h *ReviewHandler) Overview(w http.ResponseWriter, r *http.Request) {
	counts := map[string]int{}
	for _, status := range []string{storage.ProposalPending, storage.ProposalApproved, storage.ProposalRejected,
		storage.ProposalApplied, storage.ProposalStale, storage.ProposalFailed} {
		list, err := h.repo.ListProposals(storage.ProposalFilters{Status: status, Limit: 1})
		if err != nil {
			h.WriteError(w, http.StatusInternalServerError, dto.InternalError())
			return
		}
		counts[status] = list.TotalCount
	}
	h.WriteJSON(w, http.StatusOK, map[string]any{
		"proposals":       counts,
		"apply_available": h.applier != nil,
	})
}
