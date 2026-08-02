package sqlite

import (
	"database/sql"
	"time"
)

// rfc3339 formats an instant for a TEXT column. UTC keeps ordering and
// comparisons deterministic; presentation tz is the recipient's concern.
func rfc3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// parseTime parses an RFC3339 TEXT value. Empty string yields the zero time
// and ok=false (used for NOT NULL columns that should always be present).
func parseTime(s string, ok bool) (time.Time, bool) {
	if !ok || s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// nullStr converts a nullable TEXT column to a Go string (NULL -> "").
func nullStr(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

// nullTime converts a nullable TEXT time column to a *time.Time (NULL -> nil).
func nullTime(v sql.NullString) *time.Time {
	if t, ok := parseTime(v.String, v.Valid); ok {
		return &t
	}
	return nil
}

// ptrTime returns a pointer to t, or nil if t is the zero value.
func ptrTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// nullTimePtr stores a *time.Time into a sql.NullString for an INSERT.
func nullTimePtr(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{Valid: true, String: rfc3339(*t)}
}
