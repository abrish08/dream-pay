// Package csvx writes CSV reports safely for spreadsheets.
package csvx

import (
	"encoding/csv"
	"net/http"
	"strings"
	"time"

	"paygate/internal/platform/apperror"
)

// Safe neutralises formula injection: a cell starting with = + - @ (or a tab
// / carriage return) would run as a formula in Excel or Google Sheets, so it
// is prefixed with a quote. Numbers we format ourselves are written as-is.
func Safe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

// Writer streams a CSV file as the HTTP response.
type Writer struct{ *csv.Writer }

func New(w http.ResponseWriter, filename string, header []string) *Writer {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	cw := &Writer{csv.NewWriter(w)}
	_ = cw.Write(header)
	return cw
}

// Row writes text cells through Safe.
func (w *Writer) Row(cells ...string) error {
	for i, c := range cells {
		cells[i] = Safe(c)
	}
	return w.Write(cells)
}

// Period parses ?from=&to= (RFC 3339 or YYYY-MM-DD; default the last 30
// days), at most 93 days.
func Period(r *http.Request, now time.Time) (time.Time, time.Time, error) {
	parse := func(v string, def time.Time) (time.Time, error) {
		if v == "" {
			return def, nil
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.UTC(), nil
		}
		return time.Parse("2006-01-02", v)
	}
	to, err := parse(r.URL.Query().Get("to"), now)
	if err != nil {
		return time.Time{}, time.Time{}, apperror.Invalid("invalid_period", "to must be RFC 3339 or YYYY-MM-DD")
	}
	from, err := parse(r.URL.Query().Get("from"), to.AddDate(0, 0, -30))
	if err != nil {
		return time.Time{}, time.Time{}, apperror.Invalid("invalid_period", "from must be RFC 3339 or YYYY-MM-DD")
	}
	if !from.Before(to) || to.Sub(from) > 93*24*time.Hour {
		return time.Time{}, time.Time{}, apperror.Invalid("invalid_period", "from must be before to, at most 93 days")
	}
	return from, to, nil
}
