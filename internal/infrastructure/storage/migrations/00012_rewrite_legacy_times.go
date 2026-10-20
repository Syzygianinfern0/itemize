package migrations

import (
	"context"
	"database/sql"

	"github.com/pressly/goose/v3"
)

func init() {
	goose.AddMigrationContext(upRewriteLegacyTimes, downRewriteLegacyTimes)
}

// upRewriteLegacyTimes converts timestamps stored as Go's time.String()
// (e.g. "2026-10-12 00:24:11 -0400 -0400"), which the SQLite driver cannot
// scan back, to ISO 8601 with a numeric offset.
func upRewriteLegacyTimes(_ context.Context, tx *sql.Tx) error {
	return RewriteLegacyTimes(tx)
}

func downRewriteLegacyTimes(context.Context, *sql.Tx) error {
	return nil // the new format is readable by older builds too
}
