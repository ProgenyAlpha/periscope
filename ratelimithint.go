package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const (
	rateLimitHintDir    = "ratelimit-hints"
	rateLimitHintMaxAge = 10 * time.Minute
)

// rateLimitHint is one session's view of the rate-limit windows, taken from the
// statusline payload Claude Code delivers on every render.
type rateLimitHint struct {
	Pct5hr      *int   `json:"pct5hr,omitempty"`
	Reset5hr    string `json:"reset5hr,omitempty"`
	PctWeekly   *int   `json:"pctWeekly,omitempty"`
	ResetWeekly string `json:"resetWeekly,omitempty"`
}

// writeRateLimitHint records what this session observed. It deliberately does
// not touch usage-api-cache.json: the polling loop is that file's only writer,
// so a per-session render can never clobber a fresher server value.
func writeRateLimitHint(dataDir, sessionID string, rl *rateLimitsField) {
	if rl == nil || sessionID == "" {
		return
	}
	var h rateLimitHint
	if w := rl.FiveHour; w != nil && w.UsedPercentage != nil && *w.UsedPercentage >= 0 {
		h.Pct5hr = w.UsedPercentage
		h.Reset5hr = time.Unix(w.ResetsAt, 0).UTC().Format(time.RFC3339)
	}
	if w := rl.SevenDay; w != nil && w.UsedPercentage != nil && *w.UsedPercentage >= 0 {
		h.PctWeekly = w.UsedPercentage
		h.ResetWeekly = time.Unix(w.ResetsAt, 0).UTC().Format(time.RFC3339)
	}
	if h.Pct5hr == nil && h.PctWeekly == nil {
		return
	}
	dir := filepath.Join(dataDir, rateLimitHintDir)
	if os.MkdirAll(dir, 0755) != nil {
		return
	}
	payload, err := json.Marshal(h)
	if err != nil {
		return
	}
	writeFileAtomic(filepath.Join(dir, sessionID+".json"), payload, 0644)
}

// applyRateLimitHints fills windows the API did not report, using the most
// recent session hint. A window the API did report is never overwritten.
func applyRateLimitHints(usage map[string]any, dataDir string) {
	needs5hr := numOrNeg(usage["pct5hr"]) < 0
	needsWeekly := numOrNeg(usage["pctWeekly"]) < 0
	if !needs5hr && !needsWeekly {
		return
	}
	dir := filepath.Join(dataDir, rateLimitHintDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var newest time.Time
	var hint *rateLimitHint
	cutoff := time.Now().Add(-rateLimitHintMaxAge)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(cutoff) || !info.ModTime().After(newest) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var h rateLimitHint
		if json.Unmarshal(stripBOM(raw), &h) != nil {
			continue
		}
		newest, hint = info.ModTime(), &h
	}
	if hint == nil {
		return
	}
	if needs5hr && hint.Pct5hr != nil {
		usage["pct5hr"] = *hint.Pct5hr
		if hint.Reset5hr != "" {
			usage["reset5hr"] = hint.Reset5hr
		}
	}
	if needsWeekly && hint.PctWeekly != nil {
		usage["pctWeekly"] = *hint.PctWeekly
		if hint.ResetWeekly != "" {
			usage["resetWeekly"] = hint.ResetWeekly
		}
	}
}

func numOrNeg(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return -1
}
