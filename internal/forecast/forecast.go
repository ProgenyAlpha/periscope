package forecast

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BuildForecast calculates projected usage at reset time.
// 5hr window: rate-based (blended current 30min rate + average rate).
// Weekly window: duty-cycle adjusted burn rate (stable across sleep/wake cycles).
func BuildForecast(stateDir string, usage map[string]any) string {
	histPath := filepath.Join(stateDir, "limit-history.jsonl")

	data, err := os.ReadFile(histPath)
	if err != nil {
		return ""
	}

	// Parse last 60 entries
	allLines := strings.Split(strings.TrimSpace(string(data)), "\n")
	start := len(allLines) - 60
	if start < 0 {
		start = 0
	}

	type limitPoint struct {
		ts     time.Time
		pct5hr float64
		pctWk  float64
	}

	var pts []limitPoint
	for _, line := range allLines[start:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		tsStr, ok := entry["ts"].(string)
		if !ok {
			continue
		}
		t, err := time.Parse(time.RFC3339, tsStr)
		if err != nil {
			continue
		}
		pts = append(pts, limitPoint{
			ts:     t,
			pct5hr: FloatOrDefault(entry["pct5hr"], -1),
			pctWk:  FloatOrDefault(entry["pctWeekly"], -1),
		})
	}

	if len(pts) < 3 {
		return ""
	}

	now := time.Now().UTC()
	pct5hr := IntOrDefault(usage["pct5hr"], -1)
	pctWk := IntOrDefault(usage["pctWeekly"], -1)
	reset5hr, _ := usage["reset5hr"].(string)
	resetWk, _ := usage["resetWeekly"].(string)

	// Load duty cycle data (written by dashboard for weekly projection stability)
	dutyHrs := 24.0
	dutyCachePath := filepath.Join(stateDir, "duty-cache.json")
	if dcData, err := os.ReadFile(dutyCachePath); err == nil {
		var dc map[string]any
		if json.Unmarshal(dcData, &dc) == nil {
			if updAt, ok := dc["updated_at"].(float64); ok {
				if time.Since(time.Unix(int64(updAt), 0)) < time.Hour {
					if dh, ok := dc["activeHrsPerDay"].(float64); ok && dh > 0 {
						dutyHrs = dh
					}
				}
			}
		}
	}

	type windowSpec struct {
		label, resetStr string
		current         int
		use5hr          bool // true = 5hr rate-based, false = weekly duty-adjusted
	}
	windows := []windowSpec{
		{"5h", reset5hr, pct5hr, true},
		{"wk", resetWk, pctWk, false},
	}

	var parts []string
	for _, w := range windows {
		if w.current < 0 || w.resetStr == "" {
			continue
		}
		resetTime, err := time.Parse(time.RFC3339, w.resetStr)
		if err != nil {
			continue
		}
		hrsLeft := resetTime.Sub(now).Hours()
		if hrsLeft <= 0 {
			continue
		}

		// Build series with reset detection (big drop = window rolled over)
		var series []limitPoint
		prev := -1.0
		for _, pt := range pts {
			v := pt.pct5hr
			if !w.use5hr {
				v = pt.pctWk
			}
			if v < 0 {
				continue
			}
			if prev >= 0 && v < prev-15 {
				series = nil // Reset detected
			}
			series = append(series, pt)
			prev = v
		}
		if len(series) < 2 {
			continue
		}

		var proj int
		var tl, rateStr string

		if !w.use5hr {
			// Weekly: duty-adjusted active-hours projection
			elapsedHrs := 168 - hrsLeft
			activeElapsed := math.Max(0.5, (elapsedHrs/24)*dutyHrs)
			activeRemaining := (hrsLeft / 24) * dutyHrs
			burnRate := float64(w.current) / activeElapsed
			proj = int(math.Round(float64(w.current) + burnRate*activeRemaining))
			tl = fmt.Sprintf("%.1fd", hrsLeft/24)
			rateStr = fmt.Sprintf("%.1f%%/ah", burnRate)
		} else {
			// 5hr: current rate (last 30min) blended with average rate
			cutoff := now.Add(-30 * time.Minute)
			var recent []limitPoint
			for _, s := range series {
				if !s.ts.Before(cutoff) {
					recent = append(recent, s)
				}
			}

			curRate := 0.0
			if len(recent) >= 2 {
				first, last := recent[0], recent[len(recent)-1]
				dh := last.ts.Sub(first.ts).Hours()
				if dh > 0.01 {
					curRate = (last.pct5hr - first.pct5hr) / dh
				}
			}

			firstAll, lastAll := series[0], series[len(series)-1]
			dAll := lastAll.ts.Sub(firstAll.ts).Hours()
			avgRate := 0.0
			if dAll > 0.05 {
				avgRate = (lastAll.pct5hr - firstAll.pct5hr) / dAll
			}

			// Weighted blend: 60% current, 40% average
			rate := 0.0
			if curRate > 0 && avgRate > 0 {
				rate = 0.6*curRate + 0.4*avgRate
			} else if curRate > 0 {
				rate = curRate
			} else if avgRate > 0 {
				rate = avgRate
			}

			proj = int(math.Round(float64(w.current) + rate*hrsLeft))
			tl = fmt.Sprintf("%.1fh", hrsLeft)
			rateStr = fmt.Sprintf("%.1f%%/h", rate)
		}

		verdict := "OK"
		if !w.use5hr && proj <= w.current {
			verdict = "idle"
		} else if proj > 100 {
			verdict = "OVER LIMIT"
		} else if proj > 90 {
			verdict = "SLOW DOWN"
		} else if proj > 70 {
			verdict = "monitor"
		}

		parts = append(parts, fmt.Sprintf("%s:%d%%->~%d%%(%s left, %s) %s",
			w.label, w.current, proj, tl, rateStr, verdict))
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " | ")
}

// IntOrDefault extracts an int from an any value, returning def if not numeric.
func IntOrDefault(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return def
}

func FloatOrDefault(v any, def float64) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return def
}
