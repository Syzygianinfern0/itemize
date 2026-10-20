# Handoff: Walmart hardening + review UI

Branch: `walmart-hardening` on the fork `Syzygianinfern0/itemize` (upstream: `eshaffer321/itemize`).

This file hands the project to the next agent. It captures what an evaluation found, how to work safely, and a prioritized task list. Read it fully before changing code.

## Goal

The owner wants Monarch Money "Retail Purchase Sync"-style itemization for Walmart: match Walmart orders to Monarch transactions, split each transaction by category, and write item details to notes. itemize already does most of this. The goal is to make it **safe and trustworthy** for real runs, then add a **local web UI to review and approve every change** before it is written to Monarch.

## Context: how we got here

- Monarch's official extension (Amazon + Target only) is a thin collector: it uploads raw retailer pages/JSON to Monarch's servers, which do parsing, matching, categorization and splitting. Third parties can't add a retailer to it. A Walmart version has to do matching itself and write through Monarch's unofficial GraphQL API. That is what itemize does.
- Walmart data access works. itemize's Walmart client calls Walmart's internal `orchestra` GraphQL endpoints with a captured browser session: `PurchaseHistoryV2` (order list), `getOrder` (items, tax, totals) and `getOrderLedger` (the actual card charges). The charge ledger is what makes matching possible. Walmart often bills one online order as several charges (per shipment, tip separately), and in-store purchases are a single charge.
- A one-year **dry run** on the owner's real data (shared Walmart account, so many Walmart orders legitimately have no Monarch counterpart) gave these results:
  - 95 Walmart orders processed, 50 matched. Every in-store Walmart purchase present in Monarch matched exactly (same amount, 0–1 day apart). No false matches were seen.
  - 1 multi-charge online order matched, and exposed the worst bug (P0-2 below).
  - Unmatched Monarch Walmart transactions were in-store **refunds** (not handled), **PayPal**-paid walmart.com charges, and one LLM **timeout**.
  - Categories were mostly sensible; some splits attached categories to the wrong items (P0-1 below).
- Specific reproduction cases (order IDs, amounts, dates) are in `../notes/repro-cases.md`. That file is outside the repo on purpose: it holds the owner's personal financial data. **Never copy its contents into the repo.**

## Workspace layout

Everything lives in `/Users/ss99569/Developer/Fun-v2/itemize-walmart/`:

| Path | What |
|---|---|
| `itemize/` | This repo (fork), branch `walmart-hardening` |
| `monarch-go-readonly/` | Local copy of `monarch-go` v2.1.0 with a guard in `internal/transport/graphql.go` that **refuses every GraphQL mutation** (override: `MONARCH_ALLOW_MUTATIONS=1`). Test: `readonly_guard_test.go` |
| `go.work` | Uses `./itemize` and `./probe`; **replaces** `monarch-go` with `./monarch-go-readonly`. Every build in this workspace is therefore write-proof against Monarch. |
| `dry-run.sh` | Runs `bin/itemize walmart -dry-run …` with telemetry off, from `run/`. Always adds `-dry-run`. |
| `probe/` | Small read-only Go tool: lists Monarch transactions whose merchant contains "walmart" (`bin/probe 365`) |
| `bin/` | Built binaries: `itemize`, `walmart` (walmart-client-go CLI), `probe` |
| `run/` | itemize's local SQLite DB (`monarch_sync.db`), logs of the eval runs (`dry-run-365d.log` is the key one) |
| `secrets/` | `.env` with `MONARCH_COOKIE` and `OPENAI_API_KEY` (mode 700/600). **Never print, log, or commit.** |
| `notes/repro-cases.md` | Private repro data from the eval |

The Walmart session cookies live in `~/.walmart-api/cookies.json` (walmart-client-go default).

Build: `cd itemize && go build -o ../bin/itemize ./cmd/itemize`. Run: `../dry-run.sh -days 60 -verbose`. A full year takes about 50 minutes, because Walmart's ledger endpoint is throttled to about 1 call per 20–30 s.

## Safety rules (non-negotiable)

1. **No writes to Monarch** unless the owner explicitly approves that specific run in chat. That covers category updates, splits, notes, amount changes and deletes. Default to `-dry-run`, and keep the `go.work` replace (the mutation guard) in place for all development and testing.
2. Never send anything to Walmart other than the read-only queries itemize already makes. Keep the existing rate limits. On a bot challenge (HTTP 418/429/456, "Access Denied"), stop; don't retry in a loop.
3. Secrets: never echo `secrets/.env`, cookie files, or API keys, including into logs, commits, PR bodies, or chat.
4. The fork is **public**. Keep personal data (order IDs, amounts, merchant/account names, item lists from the owner's orders) out of commits, test fixtures, and PRs. Build fixtures from synthetic data.
5. Ask the owner before: pushing branches, opening PRs (to the fork or upstream), or deleting anything.
6. Owner preferences: no `Co-Authored-By`/AI attribution lines in commits or PR bodies; don't hard-wrap PR/issue bodies.

## Already done on this branch

- **Walmart order-history pagination** (`internal/adapters/providers/walmart/provider.go`): upstream reads only the first page (10 orders) regardless of `-days`; the branch follows `PageInfo.NextPageCursor` (capped at 200 pages).
- **Monarch transaction pagination** (`internal/application/sync/fetch.go`): upstream stops at 500 transactions; the branch pages with `Offset` until a short page.
- Both were verified on real data (107 Walmart orders over a year; 1,468 Monarch transactions). **Neither has unit tests yet**; add them (see P1-1).

## Task list (in priority order)

Each task: fix, add tests with synthetic fixtures, run `go test ./...`, then a dry run to confirm behavior. One commit per task, with a clear message.

### P0: must fix before any real run

**P0-1. Categories attached to the wrong items in splits.**
- Root cause: `Categorizer.CategorizeItems` (`internal/domain/categorizer/categorizer.go`) returns cached items first, then LLM results. `Splitter.createMultiCategorySplits` (`internal/domain/splitter/splitter.go`, the loop over `categorizationResult.Categorizations`) pairs results with `order.GetItems()` **by index**.
- Effect: as the cache fills during a run, any order mixing cached and new items gets shifted categories. The LLM can also reorder or drop items.
- Seen in the eval: shorts → Groceries, water → Clothing; body wash ↔ dates swapped.
- Fix:
  - Make results positionally stable: return categorizations in input order, with an explicit item index or ID in the prompt and response.
  - Make the splitter join on that key, not position.
  - Handle missing or duplicate items (e.g., same item twice).
- Tests: a cached + uncached mix; LLM output in a different order; the LLM omitting an item.

**P0-2. Multi-charge consolidation rewrites amounts and deletes bank transactions.**
- Code: `processMultiDeliveryOrder` (`internal/application/sync/handlers/walmart.go`) and `Consolidator` (`internal/application/sync/consolidator.go`).
- What happens: for an order billed as N charges, it updates one Monarch transaction's **amount** to the order total and **deletes** the other N−1.
- In the eval, the "order total" it used was net of a later refund (order total < sum of charges). A real run would have:
  - understated spending by the refund amount,
  - double-counted the refund, which is a separate Monarch transaction,
  - deleted two bank-imported transactions.
- Fix direction (owner agreed):
  - Never change transaction amounts. Never delete transactions.
  - Split **each charge's** Monarch transaction on its own, allocating items and tax across the charges proportionally to the charge amounts. Optionally also tag them as one order.
  - Use the ledger's charges, never a refund-netted order total.
  - Gate any remaining destructive path behind an explicit config flag that defaults to off.
- Tests: 3 charges, one with a later refund; charges summing to the gross total; a tip as its own charge.

**P0-3. Global guardrails in itemize itself** (independent of the `go.work` guard):
- A write-policy layer that, by default, forbids `Transactions.Delete` and any `Update` that changes `Amount`.
- Before writing, check the Monarch transaction still matches what was seen (amount, date, no existing splits, `updatedAt` unchanged) to avoid racing the user's own edits.

**P0-4. Propose → review → apply workflow.** The foundation for the UI.
- Today a run either applies changes immediately or does nothing (dry run). Add a "propose" mode that stores the full intended change set per order:
  - the matched Monarch transaction(s), with a before-snapshot;
  - the proposed category or splits, with item lists and notes;
  - the reason and confidence.
- Add an "apply" step that writes only approved proposals, re-validating each target first (P0-3).
- The `processing_records` table already stores dry-run proposals (`status='dry-run'`, `splits_json`, `items_json`). Extend it or add a `proposals` table rather than reinventing.
- Note: in dry-run, the single-category decision's `category_id`/`monarch_notes` is **not** persisted; it only appears in the log. Persist it.

### P1: correctness and robustness

1. **Tests for the two pagination fixes** (mock Walmart client returning multiple cursors; mock Monarch returning more than 500).
2. **SQLite `order_date` round-trip bug.** A second run fails with `sql: Scan error on column index 4, name "order_date": unsupported Scan, storing driver.Value type string into type *time.Time`. Dates are stored as Go's `time.String()` (e.g. `2026-10-12 00:24:11 -0400 -0400`). Store RFC3339 and parse robustly; add a migration for existing rows. Code: `internal/infrastructure/storage/sqlite.go`, `models.go`.
3. **Dry-run failures recorded as real.** Skipped and failed orders during `-dry-run` are saved with `dry_run=0` (`internal/application/sync/recording.go`, `recordError`/`recordPending`). Thread the dry-run flag through.
4. **`-order-id` doesn't work for Walmart.** `GetOrderDetails` returns "not supported for Walmart without fulfillment type" (`internal/adapters/providers/walmart/provider.go`). Use `GetOrderAutoDetect` or look the order up in purchase history first. This makes single-order testing possible.
5. **LLM timeout.** The OpenAI client has a 30 s timeout and one order failed with `context deadline exceeded` (`internal/adapters/clients/openai/client.go`). Make the timeout configurable, confirm retries actually apply to that error path, and record the failure as retryable.
6. **In-store returns.** Positive Monarch Walmart transactions (returns) are never processed. Match them to Walmart return receipts or ledger refunds, and categorize them to mirror the original items. Investigate what Walmart exposes for in-store returns first.
7. **PayPal-paid walmart.com orders.** Monarch shows them as `PAYPAL *WALMART COM`; the ledger should show payment type PayPal. Confirm the amounts align and include them in matching.
8. **Speed.** About 28 s per order is dominated by the ledger throttle. For in-store orders the ledger is usually empty and the order total is used anyway. Skip the ledger call when no Monarch candidate (same amount within the date window) exists. Make sure this doesn't skip refunds.
9. `gofmt -l internal/` lists about 13 files that were already unformatted upstream. Leave them unless a task touches them.

### P2: review UI

The owner wants a web page that shows sync status and lets them **review and approve every change**, similar to the Monarch extension's Orders view (order → items → matched transactions → sync status, with reasons for unmatched orders).

- **Backend:**
  - Use the existing chi API in `internal/api/` (`itemize serve`). It already has handlers for orders, runs, ledgers, items, transactions, stats and sync jobs, plus CORS config.
  - Add endpoints for proposals: list/filter, get detail, approve/reject (bulk too), edit an item's category, and apply approved.
  - Bind to `127.0.0.1` only.
- **Frontend recommendation:** a small static SPA embedded in the Go binary via `embed.FS`, served by `itemize serve`, so it stays one binary with no separate server. Confirm the stack with the owner before building.
- **Screens:**
  1. **Runs:** history with counts (processed, matched, skipped, errors), a "start dry run / propose" button, and live progress. Runs are long; show per-order progress.
  2. **Orders:** table filterable by status (matched / unmatched / error / applied), date and type (in-store, online, multi-charge). Unmatched rows show the reason ("no Monarch transaction for $X", "3 charges, 0 found").
  3. **Order detail:**
     - Walmart items with prices; ledger charges.
     - The matched Monarch transaction(s), before → after: category or splits, amounts, notes.
     - Editable category per item, with the split preview recomputed live.
     - Approve / reject.
  4. **Review queue:** all pending proposals with bulk approve. "Apply approved" shows a final confirmation listing exactly what will be written.
  5. **Applied log:** what was written and when, built from the existing `api_calls` audit table, with a way to see the before-state.
- The UI must never trigger a write without an explicit approval click, and the apply step must re-validate (P0-3).

## Verification checklist for each change

- `go test ./...` passes (also run `go vet ./...`).
- Dry run on real data: `../dry-run.sh -days 365 -verbose > ../run/<name>.log 2>&1`. Compare against the eval baseline in `../run/dry-run-365d.log` and `../notes/repro-cases.md`: in-store matches should stay at 50, and the multi-charge case must produce no delete and no amount change.
- `grep -c "READ-ONLY GUARD" ../run/<name>.log` must be 0 for dry runs. A non-zero count means a code path tried to write despite `-dry-run`: treat it as a P0 bug.
