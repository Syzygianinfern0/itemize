-- +goose Up
-- Proposed Monarch changes awaiting review. A propose run stores one row per
-- target transaction instead of writing; only approved rows are applied.

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS proposals (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER,
    order_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    transaction_id TEXT NOT NULL,
    kind TEXT NOT NULL,                -- 'category' or 'splits'
    status TEXT NOT NULL DEFAULT 'pending', -- pending, approved, rejected, applied, failed, stale
    amount REAL NOT NULL,
    before_json TEXT NOT NULL,         -- transaction snapshot the change was computed against
    category_id TEXT,
    category_name TEXT,
    notes TEXT,
    splits_json TEXT,
    items_json TEXT,                   -- categorized items, for review and editing
    note_prefix TEXT,                  -- e.g. 'Charge 1 of 3 (...)' for multi-charge orders
    mark_reviewed INTEGER NOT NULL DEFAULT 0,
    order_date TEXT,
    order_total REAL,
    reason TEXT,
    confidence REAL,
    error TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    decided_at TIMESTAMP,
    applied_at TIMESTAMP
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_proposals_order_id ON proposals(order_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_proposals_status ON proposals(status);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_proposals_transaction_id ON proposals(transaction_id);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sync_runs ADD COLUMN mode TEXT; -- live, dry-run, propose, apply
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS proposals;
-- +goose StatementEnd
