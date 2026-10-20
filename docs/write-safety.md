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

## Propose → review → apply

A propose run (`itemize walmart -propose -days 30`) processes orders exactly like a live sync, but the Monarch adapter captures each write instead of sending it. After each order, the captured writes become rows in the `proposals` table, one per target transaction:

- `kind`: `category` (category + notes) or `splits`
- `before_json`: the transaction snapshot the change was computed against
- the proposed category/notes or splits, the categorized items (for editing), a reason and an average categorization confidence

The order's `processing_records` row gets status `proposed` (with `dry_run=1`), so it is not treated as processed. Re-proposing replaces an order's unreviewed proposals (`pending`, `stale`, `failed`); orders with `approved`, `rejected` or `applied` proposals are skipped unless `-force`. Multi-charge consolidation never runs in propose mode. Amazon refunds are previewed but not proposed yet.

Review with `itemize proposals` (`-approve`, `-reject`, `-reset` take comma-separated IDs) or the web UI (`itemize serve`). `itemize apply` lists exactly what will be written and asks you to type `apply`. For each approved proposal it:

1. re-reads the transaction and runs the pre-write check against `before_json`; on a mismatch the proposal becomes `stale` and nothing is written;
2. writes the splits or the category/notes (never the amount), logging intent and completion in `api_calls`;
3. marks the proposal `applied` (or `failed` with the error).

When every proposal of an order is `applied` or `rejected` (with at least one applied), the order's record becomes `success`, so later syncs skip it.
