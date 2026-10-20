package sync

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/eshaffer321/monarch-go/v2/pkg/monarch"
)

// ErrTransactionChanged is returned when a Monarch transaction no longer looks
// the way it did when itemize decided how to change it. The write is refused
// so itemize never races the owner's own edits.
var ErrTransactionChanged = errors.New("monarch transaction changed since it was read")

// TransactionSnapshot is the state of a Monarch transaction that a proposed
// change was computed against.
type TransactionSnapshot struct {
	ID           string    `json:"id"`
	Date         string    `json:"date"`
	Amount       float64   `json:"amount"`
	Pending      bool      `json:"pending"`
	UpdatedAt    time.Time `json:"updated_at,omitempty"`
	CategoryID   string    `json:"category_id,omitempty"`
	CategoryName string    `json:"category_name,omitempty"`
	Notes        string    `json:"notes,omitempty"`
	Merchant     string    `json:"merchant,omitempty"`
	Account      string    `json:"account,omitempty"`
	PlaidName    string    `json:"plaid_name,omitempty"`
	SplitCount   int       `json:"split_count"`
}

// SnapshotOf captures txn's current state.
func SnapshotOf(txn *monarch.Transaction) TransactionSnapshot {
	snap := TransactionSnapshot{
		ID:        txn.ID,
		Date:      txn.Date.Format("2006-01-02"),
		Amount:    txn.Amount,
		Pending:   txn.Pending,
		UpdatedAt: txn.UpdatedAt.Time,
		Notes:     txn.Notes,
		PlaidName: txn.PlaidName,
	}
	if txn.Category != nil {
		snap.CategoryID = txn.Category.ID
		snap.CategoryName = txn.Category.Name
	}
	if txn.Merchant != nil {
		snap.Merchant = txn.Merchant.Name
	}
	if txn.Account != nil {
		snap.Account = txn.Account.DisplayName
	}
	return snap
}

// VerifyUnchanged reports (as ErrTransactionChanged) whether current differs
// from before in any way that would make the proposed change unsafe: amount,
// date, pending state or updatedAt (when both are known), or — unless
// allowExistingSplits — splits already present on the transaction.
func VerifyUnchanged(before TransactionSnapshot, current *monarch.Transaction, currentSplits []*monarch.TransactionSplit, allowExistingSplits bool) error {
	if current == nil {
		return fmt.Errorf("%w: transaction %s no longer exists", ErrTransactionChanged, before.ID)
	}
	if math.Abs(current.Amount-before.Amount) > 0.005 {
		return fmt.Errorf("%w: %s amount %.2f, expected %.2f", ErrTransactionChanged, before.ID, current.Amount, before.Amount)
	}
	if date := current.Date.Format("2006-01-02"); before.Date != "" && date != before.Date {
		return fmt.Errorf("%w: %s date %s, expected %s", ErrTransactionChanged, before.ID, date, before.Date)
	}
	if current.Pending != before.Pending {
		return fmt.Errorf("%w: %s pending=%v, expected %v", ErrTransactionChanged, before.ID, current.Pending, before.Pending)
	}
	if !before.UpdatedAt.IsZero() && !current.UpdatedAt.IsZero() && !current.UpdatedAt.Equal(before.UpdatedAt) {
		return fmt.Errorf("%w: %s was edited at %s (read at version %s)", ErrTransactionChanged, before.ID,
			current.UpdatedAt.Format(time.RFC3339), before.UpdatedAt.Format(time.RFC3339))
	}
	if !allowExistingSplits && len(currentSplits) > 0 {
		return fmt.Errorf("%w: %s already has %d split(s)", ErrTransactionChanged, before.ID, len(currentSplits))
	}
	return nil
}

// transactionReader is the read-only subset of Monarch's transaction service
// needed to re-read a transaction before writing.
type transactionReader interface {
	Query() monarch.TransactionQueryBuilder
	GetSplits(ctx context.Context, transactionID string) ([]*monarch.TransactionSplit, error)
}

// FetchCurrent re-reads the transaction a snapshot describes, with its splits.
// It uses the list query (which, unlike Get, returns updatedAt) over the days
// around the snapshot's date. A nil transaction means it was not found.
func FetchCurrent(ctx context.Context, reader transactionReader, before TransactionSnapshot) (*monarch.Transaction, []*monarch.TransactionSplit, error) {
	day, err := time.Parse("2006-01-02", before.Date)
	if err != nil {
		return nil, nil, fmt.Errorf("snapshot of %s has no usable date %q", before.ID, before.Date)
	}

	const pageSize = 500
	var current *monarch.Transaction
	for offset := 0; current == nil; offset += pageSize {
		page, err := reader.Query().
			Between(day.AddDate(0, 0, -1), day.AddDate(0, 0, 1)).
			Limit(pageSize).
			Offset(offset).
			Execute(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("re-read transaction %s: %w", before.ID, err)
		}
		for _, txn := range page.Transactions {
			if txn != nil && txn.ID == before.ID {
				current = txn
				break
			}
		}
		if len(page.Transactions) < pageSize {
			break
		}
	}
	if current == nil {
		return nil, nil, nil
	}

	splits, err := reader.GetSplits(ctx, before.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("re-read splits of %s: %w", before.ID, err)
	}
	return current, splits, nil
}
