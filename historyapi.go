package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ProgenyAlpha/periscope/internal/store"
)

// --- History windowing for /api/data and /api/history ---
//
// COMPATIBILITY STRATEGY: option (a). The default response shape is unchanged.
// GET /api/data with no query string returns exactly the array it has always
// returned, so every widget written before this change — and any user-edited
// copy of one that pluginsync has preserved — keeps working untouched. Callers
// opt IN to a smaller payload with ?history=rollup, and opt in to a windowed
// one with ?from=/?to=. Nothing is taken away from a client that does not ask.

// maxHistoryRows caps a single /api/history response. The range validator
// already refuses spans that could realistically blow past this, so hitting it
// means something unexpected; the response says so rather than lying by
// omission.
const maxHistoryRows = 100000

// parseTimeParam accepts what a browser can cheaply produce: an RFC3339 string
// (Date#toISOString, fractional seconds and all) or an integer epoch. The
// integer is read as milliseconds above 1e11 and seconds below it — 1e11
// seconds is the year 5138 and 1e11 milliseconds is 1973, so no real timestamp
// is ambiguous.
func parseTimeParam(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n > 1e11 || n < -1e11 {
			return time.UnixMilli(n).UTC(), nil
		}
		return time.Unix(n, 0).UTC(), nil
	}
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("not an RFC3339 timestamp or epoch: %q", raw)
}

// historyQueryFromRequest reads the history controls off any request.
//
// An empty query string yields the zero HistoryQuery, which BuildDashboardData
// treats as "everything, untouched".
func historyQueryFromRequest(q url.Values) (store.HistoryQuery, error) {
	var hq store.HistoryQuery

	switch mode := q.Get("history"); mode {
	case "", "full":
		hq.Mode = store.HistoryFull
	case "rollup":
		hq.Mode = store.HistoryRollup
	case "none":
		hq.Mode = store.HistoryNone
	default:
		return hq, fmt.Errorf("history must be one of full, rollup, none (got %q)", mode)
	}

	if raw := q.Get("from"); raw != "" {
		t, err := parseTimeParam(raw)
		if err != nil {
			return hq, fmt.Errorf("from: %w", err)
		}
		hq.From = t
	}
	if raw := q.Get("to"); raw != "" {
		t, err := parseTimeParam(raw)
		if err != nil {
			return hq, fmt.Errorf("to: %w", err)
		}
		hq.To = t
	}
	if raw := q.Get("bucket"); raw != "" {
		d, err := parseBucketParam(raw)
		if err != nil {
			return hq, err
		}
		hq.Bucket = d
		if d > 0 {
			hq.Mode = store.HistoryRollup
		}
	}

	if !hq.From.IsZero() && !hq.To.IsZero() {
		if !hq.To.After(hq.From) {
			return hq, fmt.Errorf("to must be after from")
		}
		// A bounded window is checked against the same limits /api/history
		// enforces, so ?from=&to= on /api/data cannot be used to ask for the
		// whole table at full resolution either.
		effBucket := hq.Bucket
		if hq.Mode == store.HistoryRollup && effBucket == 0 {
			// The tiers downsample by themselves; treat that as bucketed.
			effBucket = store.MinHistoryBucket
		}
		if err := store.ValidateHistoryRange(hq.From, hq.To, effBucket); err != nil {
			return hq, err
		}
	}
	return hq, nil
}

// parseBucketParam accepts a Go duration ("1h", "24h", "15m") or "0" for full
// resolution.
func parseBucketParam(raw string) (time.Duration, error) {
	if raw == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("bucket: not a duration: %q", raw)
	}
	if d < 0 {
		return 0, fmt.Errorf("bucket: must not be negative")
	}
	if d > 0 && d < store.MinHistoryBucket {
		return 0, fmt.Errorf("bucket: must be at least %v", store.MinHistoryBucket)
	}
	return d, nil
}

// handleHistory serves a windowed slice of the history table on its own, so a
// widget can zoom into a range without re-fetching the entire dashboard.
//
// The window is mandatory and validated: this endpoint refuses to be a second
// way of asking for the whole table.
func handleHistory(app *App, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()

	fromRaw := q.Get("from")
	if fromRaw == "" {
		writeError(w, http.StatusBadRequest, "from is required (RFC3339 or epoch)")
		return
	}
	from, err := parseTimeParam(fromRaw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "from: "+err.Error())
		return
	}

	to := time.Now().UTC()
	if raw := q.Get("to"); raw != "" {
		to, err = parseTimeParam(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "to: "+err.Error())
			return
		}
	}

	bucket := time.Duration(0)
	if raw := q.Get("bucket"); raw != "" {
		bucket, err = parseBucketParam(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	if err := store.ValidateHistoryRange(from, to, bucket); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	hq := store.HistoryQuery{From: from, To: to, Bucket: bucket}
	if bucket > 0 {
		hq.Mode = store.HistoryRollup
	}
	rows, err := store.QueryHistory(app.DB, hq)
	if err != nil {
		slog.Error("history query failed", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	truncated := false
	if len(rows) > maxHistoryRows {
		rows = rows[:maxHistoryRows]
		truncated = true
		slog.Warn("history response truncated", "cap", maxHistoryRows, "from", from, "to", to)
	}

	resp := map[string]any{
		"from":      from.Format(time.RFC3339),
		"to":        to.Format(time.RFC3339),
		"bucket":    bucket.String(),
		"rows":      len(rows),
		"truncated": truncated,
		"history":   rows,
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(resp); err != nil {
		slog.Error("history encode error", "err", err)
		writeError(w, http.StatusInternalServerError, "could not encode history payload")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	writeMaybeGzip(w, r, buf.Bytes())
}
