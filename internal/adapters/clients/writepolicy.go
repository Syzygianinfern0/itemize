package clients

import (
	"context"
	"errors"
	"fmt"

	"github.com/eshaffer321/monarch-go/v2/pkg/monarch"
)

// ErrDestructiveWriteForbidden is returned for a Monarch write the write
// policy refuses: deleting or creating a transaction, or changing its amount.
var ErrDestructiveWriteForbidden = errors.New("write policy: destructive Monarch write refused (set safety.allow_destructive_writes to permit)")

// policyTransactions wraps Monarch's transaction service so that, unless
// destructive writes are allowed, itemize can only change a transaction's
// category, notes, review flag and splits. Deletes, creates and any Update
// that sets Amount are refused before a request is sent.
//
// This is installed on every Monarch client itemize builds, so it covers all
// write paths (handlers, consolidator, reconciliation, API, apply).
type policyTransactions struct {
	monarch.TransactionService
	allowDestructive bool
}

// ApplyWritePolicy installs the write policy on client's transaction service.
func ApplyWritePolicy(client *monarch.Client, allowDestructive bool) {
	if client == nil || client.Transactions == nil {
		return
	}
	if existing, ok := client.Transactions.(*policyTransactions); ok {
		existing.allowDestructive = allowDestructive
		return
	}
	client.Transactions = &policyTransactions{
		TransactionService: client.Transactions,
		allowDestructive:   allowDestructive,
	}
}

func (p *policyTransactions) Update(ctx context.Context, transactionID string, params *monarch.UpdateTransactionParams) (*monarch.Transaction, error) {
	if params != nil && params.Amount != nil && !p.allowDestructive {
		return nil, fmt.Errorf("%w: Transactions.Update would change the amount of %s", ErrDestructiveWriteForbidden, transactionID)
	}
	return p.TransactionService.Update(ctx, transactionID, params)
}

func (p *policyTransactions) Delete(ctx context.Context, transactionID string) error {
	if !p.allowDestructive {
		return fmt.Errorf("%w: Transactions.Delete %s", ErrDestructiveWriteForbidden, transactionID)
	}
	return p.TransactionService.Delete(ctx, transactionID)
}

func (p *policyTransactions) Create(ctx context.Context, params *monarch.CreateTransactionParams) (*monarch.Transaction, error) {
	if !p.allowDestructive {
		return nil, fmt.Errorf("%w: Transactions.Create", ErrDestructiveWriteForbidden)
	}
	return p.TransactionService.Create(ctx, params)
}
