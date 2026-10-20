package storage

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// mockProposals holds MockRepository's in-memory proposals.
type mockProposals struct {
	mu     sync.Mutex
	rows   []*Proposal
	nextID int64
}

func (m *MockRepository) proposals() *mockProposals {
	m.proposalsOnce.Do(func() { m.proposalRows = &mockProposals{nextID: 1} })
	return m.proposalRows
}

func copyProposal(p *Proposal) *Proposal {
	c := *p
	c.Splits = append([]SplitDetail(nil), p.Splits...)
	c.Items = append([]ProposalItem(nil), p.Items...)
	return &c
}

func mockNow() string { return time.Now().UTC().Format("2006-01-02 15:04:05") }

// ReplaceOrderProposals implements ProposalRepository.
func (m *MockRepository) ReplaceOrderProposals(orderID string, proposals []*Proposal) error {
	store := m.proposals()
	store.mu.Lock()
	defer store.mu.Unlock()
	kept := store.rows[:0]
	for _, p := range store.rows {
		if p.OrderID == orderID && (p.Status == ProposalPending || p.Status == ProposalStale || p.Status == ProposalFailed) {
			continue
		}
		kept = append(kept, p)
	}
	store.rows = kept
	for _, p := range proposals {
		if p.OrderID != orderID {
			return fmt.Errorf("proposal for order %s passed with order %s", p.OrderID, orderID)
		}
		if p.Status == "" {
			p.Status = ProposalPending
		}
		p.ID = store.nextID
		store.nextID++
		p.CreatedAt, p.UpdatedAt = mockNow(), mockNow()
		store.rows = append(store.rows, copyProposal(p))
	}
	return nil
}

// ListProposals implements ProposalRepository.
func (m *MockRepository) ListProposals(filters ProposalFilters) (*ProposalListResult, error) {
	store := m.proposals()
	store.mu.Lock()
	defer store.mu.Unlock()
	if filters.Limit <= 0 {
		filters.Limit = 200
	}
	var matched []*Proposal
	for _, p := range store.rows {
		if (filters.Status == "" || p.Status == filters.Status) &&
			(filters.OrderID == "" || p.OrderID == filters.OrderID) &&
			(filters.Provider == "" || strings.EqualFold(p.Provider, filters.Provider)) {
			matched = append(matched, copyProposal(p))
		}
	}
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].OrderDate > matched[j].OrderDate })
	result := &ProposalListResult{Proposals: []*Proposal{}, TotalCount: len(matched), Limit: filters.Limit, Offset: filters.Offset}
	for i := filters.Offset; i < len(matched) && i < filters.Offset+filters.Limit; i++ {
		result.Proposals = append(result.Proposals, matched[i])
	}
	return result, nil
}

// GetProposal implements ProposalRepository.
func (m *MockRepository) GetProposal(id int64) (*Proposal, error) {
	store := m.proposals()
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, p := range store.rows {
		if p.ID == id {
			return copyProposal(p), nil
		}
	}
	return nil, nil
}

// GetOrderProposals implements ProposalRepository.
func (m *MockRepository) GetOrderProposals(orderID string) ([]*Proposal, error) {
	store := m.proposals()
	store.mu.Lock()
	defer store.mu.Unlock()
	var out []*Proposal
	for _, p := range store.rows {
		if p.OrderID == orderID {
			out = append(out, copyProposal(p))
		}
	}
	return out, nil
}

// SetProposalStatus implements ProposalRepository.
func (m *MockRepository) SetProposalStatus(ids []int64, status string, from []string) (int, error) {
	store := m.proposals()
	store.mu.Lock()
	defer store.mu.Unlock()
	changed := 0
	for _, p := range store.rows {
		if containsID(ids, p.ID) && containsString(from, p.Status) {
			p.Status = status
			p.DecidedAt, p.UpdatedAt = mockNow(), mockNow()
			changed++
		}
	}
	return changed, nil
}

// UpdateProposalContent implements ProposalRepository.
func (m *MockRepository) UpdateProposalContent(updated *Proposal) error {
	store := m.proposals()
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, p := range store.rows {
		if p.ID == updated.ID {
			if p.Status != ProposalPending {
				return fmt.Errorf("proposal %d is not pending", p.ID)
			}
			p.Kind, p.CategoryID, p.CategoryName, p.Notes = updated.Kind, updated.CategoryID, updated.CategoryName, updated.Notes
			p.Splits = append([]SplitDetail(nil), updated.Splits...)
			p.Items = append([]ProposalItem(nil), updated.Items...)
			p.UpdatedAt = mockNow()
			return nil
		}
	}
	return fmt.Errorf("proposal %d is not pending", updated.ID)
}

// RecordProposalResult implements ProposalRepository.
func (m *MockRepository) RecordProposalResult(id int64, status, errMsg string) error {
	store := m.proposals()
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, p := range store.rows {
		if p.ID == id {
			p.Status, p.Error, p.UpdatedAt = status, errMsg, mockNow()
			if status == ProposalApplied {
				p.AppliedAt = mockNow()
			}
		}
	}
	return nil
}

// HasReviewedProposals implements ProposalRepository.
func (m *MockRepository) HasReviewedProposals(orderID string) (bool, error) {
	store := m.proposals()
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, p := range store.rows {
		if p.OrderID == orderID && (p.Status == ProposalApproved || p.Status == ProposalRejected || p.Status == ProposalApplied) {
			return true, nil
		}
	}
	return false, nil
}

func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func containsString(values []string, s string) bool {
	for _, v := range values {
		if v == s {
			return true
		}
	}
	return false
}
