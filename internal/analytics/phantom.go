package analytics

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"math"
	"time"
)

// PhantomData represents phantom (non-CLI) usage estimation.
type PhantomData struct {
	ExtraUsageTotal   float64 `json:"extraUsageTotal"`
	LocalSessionTotal float64 `json:"localSessionTotal"`
	PhantomCost       float64 `json:"phantomCost"`
	Source            string  `json:"source"`
	Confidence        string  `json:"confidence"`
}

// CalcPhantomUsage detects usage from non-CLI tools by comparing rate limit growth
// during CLI-active vs CLI-inactive periods over the last 7 days.
func CalcPhantomUsage(db *sql.DB) *PhantomData {
	sevenDaysCutoff := time.Now().Add(-7 * 24 * time.Hour).Format("2006-01-02T15:04:05Z")

	localTotal, err := sumRecentSessionCost(db, sevenDaysCutoff)
	if err != nil {
		slog.Warn("phantom: sessions query failed", "err", err)
		return &PhantomData{Source: "none", Confidence: "none"}
	}

	if localTotal < 0.001 {
		return &PhantomData{Source: "none", Confidence: "none"}
	}

	sevenDaysAgo := time.Now().Add(-7 * 24 * time.Hour).Format(time.RFC3339)

	snapshots, err := loadLimitSnapshots(db, sevenDaysAgo)
	if err != nil {
		slog.Warn("phantom: limit_history query failed", "err", err)
		return &PhantomData{LocalSessionTotal: math.Round(localTotal*100) / 100, Source: "none", Confidence: "none"}
	}

	if len(snapshots) < 2 {
		return &PhantomData{LocalSessionTotal: math.Round(localTotal*100) / 100, Source: "none", Confidence: "none"}
	}

	activeMinutes, err := loadActiveMinutes(db, sevenDaysAgo)
	if err != nil {
		// Without the activity index every rate-limit delta would look like
		// phantom usage, so report the local total and stop rather than invent
		// a number from a truncated read.
		slog.Warn("phantom: history query failed", "err", err)
		return &PhantomData{LocalSessionTotal: math.Round(localTotal*100) / 100, Source: "none", Confidence: "none"}
	}

	// Walk consecutive snapshots. If pctWeekly grew and no CLI activity
	// occurred between them (with ±5min tolerance), attribute that growth to phantom.
	// The tolerance accounts for usage-history entries only being written on hook triggers,
	// not continuously — so there are natural gaps even during active CLI use.
	const activityTolerance = 5 // minutes before/after snapshot interval to check
	var phantomPct, cliPct float64
	for i := 1; i < len(snapshots); i++ {
		prev := snapshots[i-1]
		cur := snapshots[i]
		delta := cur.pctWeekly - prev.pctWeekly
		if delta <= 0 {
			continue
		}

		hasActivity := false
		searchStart := prev.ts.UTC().Add(-time.Duration(activityTolerance) * time.Minute).Truncate(time.Minute)
		searchEnd := cur.ts.UTC().Add(time.Duration(activityTolerance) * time.Minute).Truncate(time.Minute)
		t := searchStart
		for !t.After(searchEnd) {
			if activeMinutes[t.Format(time.RFC3339)] {
				hasActivity = true
				break
			}
			t = t.Add(time.Minute)
		}

		if hasActivity {
			cliPct += delta
		} else {
			phantomPct += delta
		}
	}

	totalPct := phantomPct + cliPct
	if totalPct < 0.5 || phantomPct < 0.5 || cliPct <= 0 {
		return &PhantomData{
			LocalSessionTotal: math.Round(localTotal*100) / 100,
			PhantomCost:       0,
			Source:            "rate_delta",
			Confidence:        "estimated",
		}
	}

	phantomCost := localTotal * (phantomPct / cliPct)

	return &PhantomData{
		ExtraUsageTotal:   math.Round(phantomPct*10) / 10,
		LocalSessionTotal: math.Round(localTotal*100) / 100,
		PhantomCost:       math.Round(phantomCost*100) / 100,
		Source:            "rate_delta",
		Confidence:        "estimated",
	}
}

// sumRecentSessionCost totals cumulative cost across sessions touched inside the
// window.
//
// Each helper drains and closes its cursor before returning, so only one read
// snapshot is ever open — CalcPhantomUsage used to keep three, on top of the
// three its caller held. rows.Err() is checked in every loop: a truncated read
// previously produced a silently understated cost that was then reported with
// "estimated" confidence.
//
// The updated_at separator is normalised on read because a row that fell back
// to the CURRENT_TIMESTAMP default renders with a space, and ' ' sorts before
// 'T'.
func sumRecentSessionCost(db *sql.DB, cutoff string) (float64, error) {
	rows, err := db.Query(
		"SELECT data FROM sessions WHERE replace(updated_at, ' ', 'T') >= ?", cutoff)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var total float64
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return 0, err
		}
		var sc struct {
			Cumulative *struct {
				Cost float64 `json:"cost"`
			} `json:"cumulative"`
		}
		if json.Unmarshal([]byte(raw), &sc) != nil || sc.Cumulative == nil {
			continue
		}
		total += sc.Cumulative.Cost
	}
	return total, rows.Err()
}

// stampLayouts is every separator the ts columns have carried, mirroring
// store/history.go's parseHistoryTS. It has to be duplicated rather than
// imported: store already depends on this package for CalcPhantomUsage.
//
// Both queries below already normalise the separator in SQL —
// `replace(ts, ' ', 'T')` — so a space-separated row is inside the window by
// deliberate choice. Parsing only the 'T' forms afterwards selected such a row
// and then threw it away without a word, which for loadActiveMinutes inverts
// the answer: a minute the CLI demonstrably ran in does not count as CLI
// activity, so the rate-limit growth across it is attributed to phantom
// (non-CLI) usage and reported as spend on a client the user never opened.
//
// history.ts and limit_history.ts are declared TEXT, so they keep whatever
// separator was written. Today's writers emit only "2006-01-02T15:04:05Z", so
// this bites imported, hand-repaired or pre-stampLayout rows rather than
// anything a current install produces — but the SQL and the parser disagreeing
// about which rows exist is not a difference of opinion worth keeping.
var stampLayouts = []string{
	"2006-01-02T15:04:05Z",
	time.RFC3339,
	time.RFC3339Nano,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04:05Z07:00",
}

// parseStamp reads a ts column value in any layout the table has ever held.
func parseStamp(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range stampLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

type limitSnapshot struct {
	ts        time.Time
	pctWeekly float64
}

// loadLimitSnapshots reads the rate-limit series inside the window.
func loadLimitSnapshots(db *sql.DB, cutoff string) ([]limitSnapshot, error) {
	rows, err := db.Query(
		"SELECT ts, data FROM limit_history WHERE replace(ts, ' ', 'T') >= ? ORDER BY replace(ts, ' ', 'T') ASC", cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []limitSnapshot
	for rows.Next() {
		var tsStr, dataStr string
		if err := rows.Scan(&tsStr, &dataStr); err != nil {
			return nil, err
		}
		t, ok := parseStamp(tsStr)
		if !ok {
			continue
		}
		var d map[string]any
		if json.Unmarshal([]byte(dataStr), &d) != nil {
			continue
		}
		pctW, ok := d["pctWeekly"].(float64)
		if !ok || pctW < 0 {
			continue
		}
		out = append(out, limitSnapshot{ts: t, pctWeekly: pctW})
	}
	return out, rows.Err()
}

// loadActiveMinutes indexes the minutes in which the CLI wrote a history point.
func loadActiveMinutes(db *sql.DB, cutoff string) (map[string]bool, error) {
	rows, err := db.Query(
		"SELECT ts FROM history WHERE replace(ts, ' ', 'T') >= ?", cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	active := map[string]bool{}
	for rows.Next() {
		var ts string
		if err := rows.Scan(&ts); err != nil {
			return nil, err
		}
		if t, ok := parseStamp(ts); ok {
			active[t.UTC().Truncate(time.Minute).Format(time.RFC3339)] = true
		}
	}
	return active, rows.Err()
}
