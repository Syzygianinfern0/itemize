package storage

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func proposalRepos(t *testing.T) map[string]ProposalRepository {
	s, err := NewStorage(filepath.Join(t.TempDir(), "proposals.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return map[string]ProposalRepository{"sqlite": s, "mock": NewMockRepository()}
}

func sampleProposal(orderID, txnID string) *Proposal {
	return &Proposal{
		OrderID: orderID, Provider: "walmart", TransactionID: txnID, Kind: ProposalKindSplits,
		Amount: -44, Before: json.RawMessage(`{"id":"` + txnID + `","amount":-44}`),
		Splits:    []SplitDetail{{CategoryID: "groceries", Amount: -11, Notes: "Groceries:\n- Water $10.00"}, {CategoryID: "travel", Amount: -33}},
		Items:     []ProposalItem{{Name: "Water", Price: 10, Quantity: 1, CategoryID: "groceries"}},
		OrderDate: "2025-01-02", OrderTotal: 44, Reason: "synthetic", Confidence: 0.9,
	}
}

func TestProposals_ReplaceKeepsReviewedAndReplacesPending(t *testing.T) {
	for name, repo := range proposalRepos(t) {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, repo.ReplaceOrderProposals("o1", []*Proposal{sampleProposal("o1", "t1"), sampleProposal("o1", "t2")}))
			list, err := repo.ListProposals(ProposalFilters{OrderID: "o1"})
			require.NoError(t, err)
			require.Equal(t, 2, list.TotalCount)

			first := list.Proposals[0]
			assert.Equal(t, ProposalPending, first.Status)
			assert.Len(t, first.Splits, 2)
			assert.Equal(t, "Water", first.Items[0].Name)
			assert.JSONEq(t, string(sampleProposal("o1", first.TransactionID).Before), string(first.Before))

			n, err := repo.SetProposalStatus([]int64{first.ID}, ProposalApproved, []string{ProposalPending})
			require.NoError(t, err)
			assert.Equal(t, 1, n)
			reviewed, err := repo.HasReviewedProposals("o1")
			require.NoError(t, err)
			assert.True(t, reviewed)

			// Re-proposing replaces only the unreviewed proposal.
			require.NoError(t, repo.ReplaceOrderProposals("o1", []*Proposal{sampleProposal("o1", "t3")}))
			all, err := repo.GetOrderProposals("o1")
			require.NoError(t, err)
			require.Len(t, all, 2)
			statuses := map[string]string{}
			for _, p := range all {
				statuses[p.TransactionID] = p.Status
			}
			assert.Equal(t, ProposalApproved, statuses[first.TransactionID])
			assert.Equal(t, ProposalPending, statuses["t3"])
		})
	}
}

func TestProposals_StatusTransitionsAreGuarded(t *testing.T) {
	for name, repo := range proposalRepos(t) {
		t.Run(name, func(t *testing.T) {
			p := sampleProposal("o2", "t1")
			require.NoError(t, repo.ReplaceOrderProposals("o2", []*Proposal{p}))

			n, err := repo.SetProposalStatus([]int64{p.ID}, ProposalApproved, []string{ProposalRejected})
			require.NoError(t, err)
			assert.Equal(t, 0, n, "only allowed source statuses move")

			require.NoError(t, repo.RecordProposalResult(p.ID, ProposalApplied, ""))
			got, err := repo.GetProposal(p.ID)
			require.NoError(t, err)
			assert.Equal(t, ProposalApplied, got.Status)
			assert.NotEmpty(t, got.AppliedAt)

			got.Kind = ProposalKindCategory
			assert.Error(t, repo.UpdateProposalContent(got), "applied proposals cannot be edited")

			missing, err := repo.GetProposal(999)
			require.NoError(t, err)
			assert.Nil(t, missing)
		})
	}
}

func TestProposals_UpdateContent(t *testing.T) {
	for name, repo := range proposalRepos(t) {
		t.Run(name, func(t *testing.T) {
			p := sampleProposal("o3", "t1")
			require.NoError(t, repo.ReplaceOrderProposals("o3", []*Proposal{p}))
			p.Kind, p.CategoryID, p.CategoryName, p.Notes, p.Splits = ProposalKindCategory, "groceries", "Groceries", "Groceries:\n- Water", nil
			require.NoError(t, repo.UpdateProposalContent(p))
			got, err := repo.GetProposal(p.ID)
			require.NoError(t, err)
			assert.Equal(t, ProposalKindCategory, got.Kind)
			assert.Equal(t, "groceries", got.CategoryID)
			assert.Empty(t, got.Splits)
		})
	}
}
