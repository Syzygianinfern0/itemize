package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eshaffer321/itemize/internal/infrastructure/config"
	"github.com/eshaffer321/itemize/internal/infrastructure/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func proposalsConfig(t *testing.T) *config.Config {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "cli.db")
	store, err := storage.NewStorage(dbPath)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceOrderProposals("SYN-CLI", []*storage.Proposal{{
		OrderID: "SYN-CLI", Provider: "walmart", TransactionID: "t1", Kind: storage.ProposalKindCategory,
		Amount: -5, Before: json.RawMessage(`{"id":"t1"}`), CategoryID: "groceries", CategoryName: "Groceries",
		Notes: "Groceries:\n- Bread $5.00", OrderDate: "2025-01-01",
	}}))
	require.NoError(t, store.Close())
	cfg := &config.Config{}
	cfg.Storage.DatabasePath = dbPath
	return cfg
}

func TestRunProposals_ListApproveAndApplyPrompt(t *testing.T) {
	cfg := proposalsConfig(t)

	var out bytes.Buffer
	require.NoError(t, RunProposals(cfg, nil, nil, &out))
	assert.Contains(t, out.String(), "category: Groceries")

	out.Reset()
	require.NoError(t, RunProposals(cfg, []string{"-approve", "1"}, nil, &out))
	assert.Contains(t, out.String(), "1 of 1 proposal(s) marked approved")

	// Declining the prompt writes nothing (and needs no Monarch client).
	out.Reset()
	require.NoError(t, RunApply(cfg, nil, strings.NewReader("no\n"), &out))
	assert.Contains(t, out.String(), "category -> Groceries")
	assert.Contains(t, out.String(), "amount -5.00 (unchanged)")
	assert.Contains(t, out.String(), "Cancelled; nothing was written.")
}

func TestRunApply_RefusesUnapprovedID(t *testing.T) {
	cfg := proposalsConfig(t)
	err := RunApply(cfg, []string{"-id", "1"}, strings.NewReader("apply\n"), &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "approve it first")
}
