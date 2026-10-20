package migrations

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// TimeLayout is how timestamps are stored: ISO 8601 with a numeric offset,
// which the SQLite driver parses back into time.Time (its "sqlite" format).
const TimeLayout = "2006-01-02 15:04:05.999999999-07:00"

// legacyTimeColumns lists timestamp columns that older builds filled with
// Go's time.String() output.
var legacyTimeColumns = map[string][]string{
	"processing_records":  {"order_date", "processed_at"},
	"processing_attempts": {"order_date", "processed_at"},
	"ledger_charges":      {"charged_at", "matched_at"},
	"order_ledgers":       {"fetched_at"},
}

// legacyTimePattern matches time.String() output: a space before the offset.
var legacyTimePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(\.\d+)? [+-]\d{4}( |$)`)

// ParseLegacyTime parses a time.String() value such as
// "2026-10-12 00:24:11 -0400 -0400" or "... -0500 CDT m=+216.3".
func ParseLegacyTime(s string) (time.Time, bool) {
	if i := strings.Index(s, " m="); i >= 0 {
		s = s[:i]
	}
	fields := strings.Fields(s)
	if len(fields) < 3 {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02 15:04:05.999999999 -0700", strings.Join(fields[:3], " "))
	return t, err == nil
}

type execQuerier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
}

// RewriteLegacyTimes rewrites time.String() values in the known timestamp
// columns to TimeLayout, keeping each value's offset. Other values are left
// alone; tables or columns that don't exist are skipped.
func RewriteLegacyTimes(db execQuerier) error {
	for table, columns := range legacyTimeColumns {
		for _, column := range columns {
			if err := rewriteColumn(db, table, column); err != nil {
				return fmt.Errorf("rewrite %s.%s: %w", table, column, err)
			}
		}
	}
	return nil
}

func rewriteColumn(db execQuerier, table, column string) error {
	rows, err := db.Query(fmt.Sprintf(`SELECT rowid, CAST(%s AS TEXT) FROM %s WHERE %s IS NOT NULL`, column, table, column))
	if err != nil {
		if strings.Contains(err.Error(), "no such") {
			return nil
		}
		return err
	}
	type update struct {
		rowid int64
		value string
	}
	var updates []update
	for rows.Next() {
		var rowid int64
		var value string
		if err := rows.Scan(&rowid, &value); err != nil {
			_ = rows.Close()
			return err
		}
		if !legacyTimePattern.MatchString(value) {
			continue
		}
		if t, ok := ParseLegacyTime(value); ok {
			updates = append(updates, update{rowid, t.Format(TimeLayout)})
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, u := range updates {
		if _, err := db.Exec(fmt.Sprintf(`UPDATE %s SET %s = ? WHERE rowid = ?`, table, column), u.value, u.rowid); err != nil {
			return err
		}
	}
	return nil
}
