package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Proposal statuses.
const (
	ProposalPending  = "pending"
	ProposalApproved = "approved"
	ProposalRejected = "rejected"
	ProposalApplied  = "applied"
	ProposalFailed   = "failed"
	ProposalStale    = "stale" // the transaction changed after the proposal was made
)

// Proposal kinds.
const (
	ProposalKindCategory = "category"
	ProposalKindSplits   = "splits"
)

// Proposal is one proposed change to one Monarch transaction: either a single
// category with notes, or a set of splits.
type Proposal struct {
	ID            int64   `json:"id"`
	RunID         int64   `json:"run_id,omitempty"`
	OrderID       string  `json:"order_id"`
	Provider      string  `json:"provider"`
	TransactionID string  `json:"transaction_id"`
	Kind          string  `json:"kind"`
	Status        string  `json:"status"`
	Amount        float64 `json:"amount"`

	// Before is the transaction snapshot (JSON) the change was computed
	// against; apply re-validates the live transaction against it.
	Before json.RawMessage `json:"before"`

	CategoryID   string         `json:"category_id,omitempty"`
	CategoryName string         `json:"category_name,omitempty"`
	Notes        string         `json:"notes,omitempty"`
	Splits       []SplitDetail  `json:"splits,omitempty"`
	Items        []ProposalItem `json:"items,omitempty"`
	NotePrefix   string         `json:"note_prefix,omitempty"`
	MarkReviewed bool           `json:"mark_reviewed"`
	OrderDate    string         `json:"order_date,omitempty"`
	OrderTotal   float64        `json:"order_total"`
	Reason       string         `json:"reason,omitempty"`
	Confidence   float64        `json:"confidence"`
	Error        string         `json:"error,omitempty"`
	CreatedAt    string         `json:"created_at,omitempty"`
	UpdatedAt    string         `json:"updated_at,omitempty"`
	DecidedAt    string         `json:"decided_at,omitempty"`
	AppliedAt    string         `json:"applied_at,omitempty"`
}

// ProposalItem is an order item with the category it was assigned.
type ProposalItem struct {
	Name         string  `json:"name"`
	Price        float64 `json:"price"`
	Quantity     float64 `json:"quantity"`
	CategoryID   string  `json:"category_id"`
	CategoryName string  `json:"category_name"`
	Confidence   float64 `json:"confidence"`
}

const proposalColumns = `id, COALESCE(run_id, 0), order_id, provider, transaction_id, kind, status, amount,
	before_json, COALESCE(category_id, ''), COALESCE(category_name, ''), COALESCE(notes, ''),
	COALESCE(splits_json, ''), COALESCE(items_json, ''), COALESCE(note_prefix, ''), mark_reviewed,
	COALESCE(order_date, ''), COALESCE(order_total, 0), COALESCE(reason, ''), COALESCE(confidence, 0),
	COALESCE(error, ''), COALESCE(created_at, ''), COALESCE(updated_at, ''),
	COALESCE(decided_at, ''), COALESCE(applied_at, '')`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanProposal(row rowScanner) (*Proposal, error) {
	p := &Proposal{}
	var before, splitsJSON, itemsJSON string
	if err := row.Scan(&p.ID, &p.RunID, &p.OrderID, &p.Provider, &p.TransactionID, &p.Kind, &p.Status, &p.Amount,
		&before, &p.CategoryID, &p.CategoryName, &p.Notes,
		&splitsJSON, &itemsJSON, &p.NotePrefix, &p.MarkReviewed,
		&p.OrderDate, &p.OrderTotal, &p.Reason, &p.Confidence,
		&p.Error, &p.CreatedAt, &p.UpdatedAt, &p.DecidedAt, &p.AppliedAt); err != nil {
		return nil, err
	}
	p.Before = json.RawMessage(before)
	if splitsJSON != "" {
		if err := json.Unmarshal([]byte(splitsJSON), &p.Splits); err != nil {
			return nil, fmt.Errorf("proposal %d splits: %w", p.ID, err)
		}
	}
	if itemsJSON != "" {
		if err := json.Unmarshal([]byte(itemsJSON), &p.Items); err != nil {
			return nil, fmt.Errorf("proposal %d items: %w", p.ID, err)
		}
	}
	return p, nil
}

func marshalProposalContent(p *Proposal) (splitsJSON, itemsJSON any, err error) {
	if len(p.Splits) > 0 {
		b, err := json.Marshal(p.Splits)
		if err != nil {
			return nil, nil, err
		}
		splitsJSON = string(b)
	}
	if len(p.Items) > 0 {
		b, err := json.Marshal(p.Items)
		if err != nil {
			return nil, nil, err
		}
		itemsJSON = string(b)
	}
	return splitsJSON, itemsJSON, nil
}

// ReplaceOrderProposals implements ProposalRepository.
func (s *Storage) ReplaceOrderProposals(orderID string, proposals []*Proposal) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM proposals WHERE order_id = ? AND status IN ('pending', 'stale', 'failed')`, orderID); err != nil {
		return err
	}
	for _, p := range proposals {
		if p.OrderID != orderID {
			return fmt.Errorf("proposal for order %s passed with order %s", p.OrderID, orderID)
		}
		splitsJSON, itemsJSON, err := marshalProposalContent(p)
		if err != nil {
			return err
		}
		status := p.Status
		if status == "" {
			status = ProposalPending
		}
		res, err := tx.Exec(`
			INSERT INTO proposals (run_id, order_id, provider, transaction_id, kind, status, amount, before_json,
				category_id, category_name, notes, splits_json, items_json, note_prefix, mark_reviewed,
				order_date, order_total, reason, confidence)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			nullInt64(p.RunID), p.OrderID, p.Provider, p.TransactionID, p.Kind, status, p.Amount, string(p.Before),
			nullString(p.CategoryID), nullString(p.CategoryName), nullString(p.Notes), splitsJSON, itemsJSON,
			nullString(p.NotePrefix), p.MarkReviewed, nullString(p.OrderDate), p.OrderTotal, nullString(p.Reason), p.Confidence)
		if err != nil {
			return err
		}
		if p.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		p.Status = status
	}
	return tx.Commit()
}

// ListProposals implements ProposalRepository.
func (s *Storage) ListProposals(filters ProposalFilters) (*ProposalListResult, error) {
	if filters.Limit <= 0 {
		filters.Limit = 200
	}
	var where []string
	var args []any
	if filters.Status != "" {
		where = append(where, "status = ?")
		args = append(args, filters.Status)
	}
	if filters.OrderID != "" {
		where = append(where, "order_id = ?")
		args = append(args, filters.OrderID)
	}
	if filters.Provider != "" {
		where = append(where, "LOWER(provider) = LOWER(?)")
		args = append(args, filters.Provider)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	result := &ProposalListResult{Proposals: []*Proposal{}, Limit: filters.Limit, Offset: filters.Offset}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM proposals`+clause, args...).Scan(&result.TotalCount); err != nil {
		return nil, err
	}

	rows, err := s.db.Query(`SELECT `+proposalColumns+` FROM proposals`+clause+
		` ORDER BY order_date DESC, order_id, id LIMIT ? OFFSET ?`, append(args, filters.Limit, filters.Offset)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		result.Proposals = append(result.Proposals, p)
	}
	return result, rows.Err()
}

// GetProposal implements ProposalRepository.
func (s *Storage) GetProposal(id int64) (*Proposal, error) {
	p, err := scanProposal(s.db.QueryRow(`SELECT `+proposalColumns+` FROM proposals WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

// GetOrderProposals implements ProposalRepository.
func (s *Storage) GetOrderProposals(orderID string) ([]*Proposal, error) {
	rows, err := s.db.Query(`SELECT `+proposalColumns+` FROM proposals WHERE order_id = ? ORDER BY id`, orderID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetProposalStatus implements ProposalRepository.
func (s *Storage) SetProposalStatus(ids []int64, status string, from []string) (int, error) {
	if len(ids) == 0 || len(from) == 0 {
		return 0, nil
	}
	args := []any{status}
	idMarks := make([]string, len(ids))
	for i, id := range ids {
		idMarks[i] = "?"
		args = append(args, id)
	}
	fromMarks := make([]string, len(from))
	for i, f := range from {
		fromMarks[i] = "?"
		args = append(args, f)
	}
	res, err := s.db.Exec(`UPDATE proposals SET status = ?, decided_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id IN (`+strings.Join(idMarks, ",")+`) AND status IN (`+strings.Join(fromMarks, ",")+`)`, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// UpdateProposalContent implements ProposalRepository.
func (s *Storage) UpdateProposalContent(p *Proposal) error {
	splitsJSON, itemsJSON, err := marshalProposalContent(p)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE proposals SET kind = ?, category_id = ?, category_name = ?, notes = ?,
		splits_json = ?, items_json = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = 'pending'`,
		p.Kind, nullString(p.CategoryID), nullString(p.CategoryName), nullString(p.Notes), splitsJSON, itemsJSON, p.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("proposal %d is not pending", p.ID)
	}
	return nil
}

// RecordProposalResult implements ProposalRepository.
func (s *Storage) RecordProposalResult(id int64, status, errMsg string) error {
	appliedAt := "NULL"
	if status == ProposalApplied {
		appliedAt = "CURRENT_TIMESTAMP"
	}
	_, err := s.db.Exec(`UPDATE proposals SET status = ?, error = ?, updated_at = CURRENT_TIMESTAMP, applied_at = `+appliedAt+` WHERE id = ?`,
		status, nullString(errMsg), id)
	return err
}

// HasReviewedProposals implements ProposalRepository.
func (s *Storage) HasReviewedProposals(orderID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM proposals WHERE order_id = ? AND status IN ('approved', 'rejected', 'applied')`, orderID).Scan(&n)
	return n > 0, err
}
