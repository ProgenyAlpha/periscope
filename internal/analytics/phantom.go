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
	var localTotal float64
	rows, err := db.Query("SELECT data FROM sessions WHERE updated_at >= ?", sevenDaysCutoff)
	if err != nil {
		slog.Warn("phantom: sessions query failed", "err", err)
		return &PhantomData{Source: "none", Confidence: "none"}
	}
	defer rows.Close()

	for rows.Next() {
		var raw string
		if rows.Scan(&raw) != nil {
			continue
		}
		var sc struct {
			Cumulative *struct {
				Cost float64 `json:"cost"`
			} `json:"cumulative"`
		}
		if json.Unmarshal([]byte(raw), &sc) != nil || sc.Cumulative == nil {
			continue
		}
		localTotal += sc.Cumulative.Cost
	}

	if localTotal < 0.001 {
		return &PhantomData{Source: "none", Confidence: "none"}
	}

	sevenDaysAgo := time.Now().Add(-7 * 24 * time.Hour).Format(time.RFC3339)

	type snapshot struct {
		ts        time.Time
		pctWeekly float64
	}
	var snapshots []snapshot
	lhRows, err := db.Query("SELECT ts, data FROM limit_history WHERE ts >= ? ORDER BY ts ASC", sevenDaysAgo)
	if err != nil {
		return &PhantomData{LocalSessionTotal: math.Round(localTotal*100) / 100, Source: "none", Confidence: "none"}
	}
	defer lhRows.Close()

	for lhRows.Next() {
		var tsStr, dataStr string
		if lhRows.Scan(&tsStr, &dataStr) != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, tsStr)
		if err != nil {
			t, err = time.Parse(time.RFC3339, tsStr)
			if err != nil {
				continue
			}
		}
		var d map[string]any
		if json.Unmarshal([]byte(dataStr), &d) != nil {
			continue
		}
		pctW, ok := d["pctWeekly"].(float64)
		if !ok || pctW < 0 {
			continue
		}
		snapshots = append(snapshots, snapshot{ts: t, pctWeekly: pctW})
	}

	if len(snapshots) < 2 {
		return &PhantomData{LocalSessionTotal: math.Round(localTotal*100) / 100, Source: "none", Confidence: "none"}
	}

	// Build "active minutes" from history snapshots (last 7 days).
	activeMinutes := map[string]bool{}
	hRows, err := db.Query("SELECT ts FROM history WHERE ts >= ?", sevenDaysAgo)
	if err == nil {
		defer hRows.Close()
		for hRows.Next() {
			var ts string
			if hRows.Scan(&ts) != nil {
				continue
			}
			if t, err := time.Parse("2006-01-02T15:04:05Z", ts); err == nil {
				activeMinutes[t.Truncate(time.Minute).Format(time.RFC3339)] = true
			} else if t, err := time.Parse(time.RFC3339, ts); err == nil {
				activeMinutes[t.UTC().Truncate(time.Minute).Format(time.RFC3339)] = true
			}
		}
	}

	// Walk consecutive snapshots. If pctWeekly grew and no CLI activity
	// occurred between them, attribute that growth to phantom.
	var phantomPct, cliPct float64
	for i := 1; i < len(snapshots); i++ {
		prev := snapshots[i-1]
		cur := snapshots[i]
		delta := cur.pctWeekly - prev.pctWeekly
		if delta <= 0 {
			continue
		}

		hasActivity := false
		t := prev.ts.UTC().Truncate(time.Minute)
		end := cur.ts.UTC().Truncate(time.Minute)
		for !t.After(end) {
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
	if totalPct < 0.5 || phantomPct < 0.5 {
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
