package forecast

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// LocalBurn computes the dollar cost spent in the last `window` from the
// usage-history.jsonl ledger written by the Stop hook. Each entry is a
// cumulative-per-session snapshot, so per-session deltas across the window
// give an unbiased burn estimate even when sessions cross window boundaries.
//
// Returns (costInWindow, ok). ok is false when there's not enough data to
// make a meaningful estimate (e.g. fewer than 2 entries for any session).
func LocalBurn(stateDir string, window time.Duration) (float64, bool) {
	histPath := filepath.Join(stateDir, "usage-history.jsonl")
	f, err := os.Open(histPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	cutoff := time.Now().UTC().Add(-window)

	type sessionSpan struct {
		costAtCutoff float64
		costLatest   float64
		seenInside   bool
		seenOutside  bool
	}
	spans := map[string]*sessionSpan{}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<16), 1<<20)
	for scanner.Scan() {
		var e struct {
			TS   string  `json:"ts"`
			SID  string  `json:"sid"`
			Cost float64 `json:"cost"`
		}
		if json.Unmarshal(scanner.Bytes(), &e) != nil {
			continue
		}
		if e.SID == "" || e.TS == "" {
			continue
		}
		t, err := time.Parse("2006-01-02T15:04:05Z", e.TS)
		if err != nil {
			continue
		}
		s := spans[e.SID]
		if s == nil {
			s = &sessionSpan{}
			spans[e.SID] = s
		}
		if t.Before(cutoff) {
			// Track the most recent pre-window cost for delta calculation.
			s.costAtCutoff = e.Cost
			s.seenOutside = true
		} else {
			s.costLatest = e.Cost
			s.seenInside = true
		}
	}

	var total float64
	any := false
	for _, s := range spans {
		if !s.seenInside {
			continue
		}
		any = true
		// If we never saw the session before the window, the session began
		// inside the window — its earliest in-window cost is its starting
		// cumulative cost at session-start, which we approximate as 0.
		base := s.costAtCutoff
		if !s.seenOutside {
			base = 0
		}
		delta := s.costLatest - base
		if delta > 0 {
			total += delta
		}
	}
	return total, any
}

// LocalBurnRate returns dollars-per-hour over the given window.
func LocalBurnRate(stateDir string, window time.Duration) (float64, bool) {
	cost, ok := LocalBurn(stateDir, window)
	if !ok || window <= 0 {
		return 0, false
	}
	return cost / window.Hours(), true
}
