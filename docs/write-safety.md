# Write safety

itemize writes to Monarch through Monarch's unofficial GraphQL API. These guardrails keep those writes narrow and safe.

## Write policy (always on)

`clients.ApplyWritePolicy` wraps the Monarch client's transaction service when itemize builds it (`clients.NewClients`). Unless `safety.allow_destructive_writes: true` is set in `config.yaml` (or `ITEMIZE_ALLOW_DESTRUCTIVE_WRITES=1`), it refuses, before any request is sent:

- `Transactions.Delete`
- `Transactions.Create`
- any `Transactions.Update` that sets `Amount`

What remains possible is setting a transaction's category, notes and review flag, and replacing its splits. The refusal error wraps `clients.ErrDestructiveWriteForbidden`.

The same flag turns on the legacy multi-charge consolidation (rewrite one transaction's amount, delete the others). Without it, each charge's transaction is split on its own; see `docs/walmart-multi-delivery.md`.

## Pre-write check (always on for syncs)

When a sync fetches Monarch transactions it snapshots each one (`TransactionSnapshot`: amount, date, pending, `updatedAt`, category, notes). Before every category/notes/splits write, the Monarch adapter re-reads the transaction and refuses the write (`ErrTransactionChanged`) if:

- it no longer exists,
- its amount, date or pending state changed,
- its `updatedAt` changed (the owner edited it), or
- it already has splits (except when reconciliation re-applies itemize's own cached splits).

A transaction itemize already wrote during the same run is not re-checked, since that write changed its `updatedAt`. A write to a transaction that was never snapshotted is refused.

## Development guard

In the local evaluation workspace, `go.work` replaces `monarch-go` with a copy that refuses every GraphQL mutation unless `MONARCH_ALLOW_MUTATIONS=1`. That guard is independent of the ones above.
