package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eshaffer321/itemize/internal/api"
	"github.com/eshaffer321/itemize/internal/api/handlers"
	"github.com/eshaffer321/itemize/internal/infrastructure/storage"
	"github.com/eshaffer321/monarch-go/v2/pkg/monarch"
)

var reviewDay = time.Date(2025, 3, 4, 0, 0, 0, 0, time.UTC)

// fakeMonarch is a Monarch transaction service holding one transaction.
type fakeMonarch struct {
	monarch.TransactionService
	txn    *monarch.Transaction
	writes []string
}

func (f *fakeMonarch) Query() monarch.TransactionQueryBuilder { return &fakeQuery{f: f} }
func (f *fakeMonarch) GetSplits(context.Context, string) ([]*monarch.TransactionSplit, error) {
	return nil, nil
}
func (f *fakeMonarch) UpdateSplits(_ context.Context, id string, _ []*monarch.TransactionSplit) error {
	f.writes = append(f.writes, "splits "+id)
	return nil
}
func (f *fakeMonarch) Update(_ context.Context, id string, _ *monarch.UpdateTransactionParams) (*monarch.Transaction, error) {
	f.writes = append(f.writes, "update "+id)
	return f.txn, nil
}

func (f *fakeMonarch) Categories() monarch.TransactionCategoryService { return fakeCategories{} }

type fakeCategories struct{ monarch.TransactionCategoryService }

func (fakeCategories) List(context.Context) ([]*monarch.TransactionCategory, error) {
	return []*monarch.TransactionCategory{{ID: "groceries", Name: "Groceries"}, {ID: "travel", Name: "Travel"}}, nil
}

type fakeQuery struct {
	monarch.TransactionQueryBuilder
	f *fakeMonarch
}

func (q *fakeQuery) Between(_, _ time.Time) monarch.TransactionQueryBuilder { return q }
func (q *fakeQuery) Limit(int) monarch.TransactionQueryBuilder              { return q }
func (q *fakeQuery) Offset(int) monarch.TransactionQueryBuilder             { return q }
func (q *fakeQuery) Execute(context.Context) (*monarch.TransactionList, error) {
	copied := *q.f.txn
	return &monarch.TransactionList{Transactions: []*monarch.Transaction{&copied}}, nil
}

func reviewServer(t *testing.T, client *monarch.Client) (*api.Server, *storage.MockRepository) {
	t.Helper()
	repo := storage.NewMockRepository()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return api.NewServer(api.DefaultConfig(), repo, nil, client, logger), repo
}

func seedProposal(t *testing.T, repo *storage.MockRepository) *storage.Proposal {
	t.Helper()
	before, _ := json.Marshal(map[string]any{"id": "t1", "date": reviewDay.Format("2006-01-02"), "amount": -44.0, "pending": false})
	p := &storage.Proposal{
		OrderID: "SYN-UI-1", Provider: "walmart", TransactionID: "t1", Kind: storage.ProposalKindSplits, Amount: -44,
		Before: before, OrderDate: "2025-03-04", OrderTotal: 44,
		Splits: []storage.SplitDetail{
			{CategoryID: "groceries", CategoryName: "Groceries", Amount: -11, Notes: "Groceries:\n- Water $10.00"},
			{CategoryID: "travel", CategoryName: "Travel", Amount: -33, Notes: "Travel:\n- Bag $30.00"},
		},
		Items: []storage.ProposalItem{
			{Name: "Water", Price: 10, Quantity: 1, CategoryID: "groceries", CategoryName: "Groceries", Confidence: 0.9},
			{Name: "Bag", Price: 30, Quantity: 1, CategoryID: "travel", CategoryName: "Travel", Confidence: 0.8},
		},
	}
	require.NoError(t, repo.ReplaceOrderProposals(p.OrderID, []*storage.Proposal{p}))
	require.NoError(t, repo.SaveRecord(&storage.ProcessingRecord{
		OrderID: p.OrderID, Provider: "walmart", OrderDate: reviewDay, OrderTotal: 44, Status: "proposed", DryRun: true,
		RawOrderJSON: `{"type":"IN_STORE"}`,
	}))
	return p
}

func do(t *testing.T, s *api.Server, method, path string, body any, local bool) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Host = "127.0.0.1:8085"
	if local && method != http.MethodGet {
		req.Header.Set("X-Itemize-Request", "1")
		req.Header.Set("Origin", "http://127.0.0.1:8085")
	}
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func TestReviewAPI_ListDecideAndGuard(t *testing.T) {
	s, repo := reviewServer(t, nil)
	p := seedProposal(t, repo)

	rec := do(t, s, http.MethodGet, "/api/proposals?status=pending", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"transaction_id":"t1"`)

	// A state change without the request header (e.g. a form on another site) is refused.
	rec = do(t, s, http.MethodPost, "/api/proposals/decision", map[string]any{"ids": []int64{p.ID}, "action": "approve"}, false)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	rec = do(t, s, http.MethodPost, "/api/proposals/decision", map[string]any{"ids": []int64{p.ID}, "action": "approve"}, true)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, _ := repo.GetProposal(p.ID)
	assert.Equal(t, storage.ProposalApproved, got.Status)

	// Non-loopback Host (DNS rebinding) is refused even for reads.
	req := httptest.NewRequest(http.MethodGet, "/api/proposals", nil)
	req.Host = "attacker.example"
	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestReviewAPI_EditItemsRecomputesPlan(t *testing.T) {
	s, repo := reviewServer(t, &monarch.Client{Transactions: &fakeMonarch{}})
	p := seedProposal(t, repo)

	rec := do(t, s, http.MethodPut, "/api/proposals/"+itoa(p.ID)+"/items", map[string]any{"categories": []string{"travel", "travel"}}, true)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var updated storage.Proposal
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	assert.Equal(t, storage.ProposalKindCategory, updated.Kind)
	assert.Equal(t, "travel", updated.CategoryID)
	assert.Contains(t, updated.Notes, "- Water $10.00")
	assert.Equal(t, 1.0, updated.Items[0].Confidence, "an owner edit is certain")

	rec = do(t, s, http.MethodPut, "/api/proposals/"+itoa(p.ID)+"/items", map[string]any{"categories": []string{"groceries", "travel"}}, true)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	assert.Equal(t, storage.ProposalKindSplits, updated.Kind)
	require.Len(t, updated.Splits, 2)
	assert.Equal(t, -11.0, updated.Splits[0].Amount)

	rec = do(t, s, http.MethodPut, "/api/proposals/"+itoa(p.ID)+"/items", map[string]any{"categories": []string{"nope", "travel"}}, true)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = do(t, s, http.MethodPut, "/api/proposals/"+itoa(p.ID)+"/items", map[string]any{"categories": []string{"travel"}}, true)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "one category per item")
}

func TestReviewAPI_ApplyRequiresApprovalAndClient(t *testing.T) {
	s, repo := reviewServer(t, nil)
	p := seedProposal(t, repo)

	rec := do(t, s, http.MethodPost, "/api/apply/plan", map[string]any{"ids": []int64{p.ID}}, true)
	assert.Equal(t, http.StatusConflict, rec.Code, "pending proposals cannot be applied")
	rec = do(t, s, http.MethodPost, "/api/apply", map[string]any{"ids": []int64{}}, true)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestReviewAPI_ApplyWritesApprovedProposal(t *testing.T) {
	fake := &fakeMonarch{txn: &monarch.Transaction{ID: "t1", Amount: -44, Date: monarch.Date{Time: reviewDay}}}
	s, repo := reviewServer(t, &monarch.Client{Transactions: fake})
	p := seedProposal(t, repo)
	_, err := repo.SetProposalStatus([]int64{p.ID}, storage.ProposalApproved, []string{storage.ProposalPending})
	require.NoError(t, err)

	rec := do(t, s, http.MethodPost, "/api/apply/plan", map[string]any{"ids": []int64{p.ID}}, true)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"count":1`)

	rec = do(t, s, http.MethodPost, "/api/apply", map[string]any{"ids": []int64{p.ID}}, true)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var job handlers.ApplyJob
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &job))

	require.Eventually(t, func() bool {
		rec := do(t, s, http.MethodGet, "/api/apply/"+job.ID, nil, true)
		_ = json.Unmarshal(rec.Body.Bytes(), &job)
		return job.Status != "running"
	}, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, "completed", job.Status, job.Error)
	assert.Equal(t, 1, job.Result.Applied)
	assert.Equal(t, []string{"splits t1"}, fake.writes)

	rec = do(t, s, http.MethodGet, "/api/applied", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"method":"Transactions.UpdateSplits"`)
	assert.Contains(t, rec.Body.String(), `"before"`)
}

func TestReviewAPI_OrdersAndDetail(t *testing.T) {
	s, repo := reviewServer(t, nil)
	seedProposal(t, repo)

	rec := do(t, s, http.MethodGet, "/api/review/orders", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"order_type":"in-store"`)
	assert.Contains(t, rec.Body.String(), `"pending":1`)

	rec = do(t, s, http.MethodGet, "/api/review/orders/SYN-UI-1", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"proposals":[{`)

	rec = do(t, s, http.MethodGet, "/api/review/overview", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"apply_available":false`)
}

func TestUI_IsServedWithStrictCSP(t *testing.T) {
	s, _ := reviewServer(t, nil)
	rec := do(t, s, http.MethodGet, "/", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "/ui/app.js")
	assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "script-src 'self'")

	for _, asset := range []string{"/ui/app.js", "/ui/app.css"} {
		rec = do(t, s, http.MethodGet, asset, nil, true)
		assert.Equal(t, http.StatusOK, rec.Code, asset)
	}
	assert.False(t, strings.Contains(rec.Body.String(), "innerHTML"))
}

func TestStartSync_RefusesRunsThatWriteToMonarch(t *testing.T) {
	h := handlers.NewSyncHandler(nil)
	for _, body := range []string{`{"provider":"walmart"}`, `{"provider":"walmart","dry_run":true,"propose":true}`} {
		req := httptest.NewRequest(http.MethodPost, "/api/sync", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.StartSync(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
