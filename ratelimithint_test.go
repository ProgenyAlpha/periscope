package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func intp(v int) *int { return &v }

func TestWriteRateLimitHintSkipsAbsentWindows(t *testing.T) {
	dir := t.TempDir()
	rl := &rateLimitsField{FiveHour: &rateWindow{UsedPercentage: nil}}
	writeRateLimitHint(dir, "sid", rl)
	if _, err := os.Stat(filepath.Join(dir, rateLimitHintDir, "sid.json")); err == nil {
		t.Error("wrote a hint for an absent used_percentage")
	}
}

func TestApplyRateLimitHintsFillsOnlyMissingWindows(t *testing.T) {
	dir := t.TempDir()
	rl := &rateLimitsField{
		FiveHour: &rateWindow{UsedPercentage: intp(7), ResetsAt: 1786447800},
		SevenDay: &rateWindow{UsedPercentage: intp(26), ResetsAt: 1786784400},
	}
	writeRateLimitHint(dir, "sid", rl)

	usage := map[string]any{"pct5hr": 42, "pctWeekly": -1}
	applyRateLimitHints(usage, dir)

	if got := usage["pct5hr"]; got != 42 {
		t.Errorf("pct5hr = %v, want 42 — a live API value must not be overwritten by a hint", got)
	}
	if got := usage["pctWeekly"]; got != 26 {
		t.Errorf("pctWeekly = %v, want 26 from the hint", got)
	}
}

func TestApplyRateLimitHintsIgnoresStaleHints(t *testing.T) {
	dir := t.TempDir()
	rl := &rateLimitsField{SevenDay: &rateWindow{UsedPercentage: intp(26), ResetsAt: 1786784400}}
	writeRateLimitHint(dir, "sid", rl)

	p := filepath.Join(dir, rateLimitHintDir, "sid.json")
	old := time.Now().Add(-2 * rateLimitHintMaxAge)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}

	usage := map[string]any{"pctWeekly": -1}
	applyRateLimitHints(usage, dir)
	if got := usage["pctWeekly"]; got != -1 {
		t.Errorf("pctWeekly = %v, want -1 — a hint older than %v must be ignored", got, rateLimitHintMaxAge)
	}
}

func TestApplyRateLimitHintsPrefersNewestSession(t *testing.T) {
	dir := t.TempDir()
	writeRateLimitHint(dir, "old", &rateLimitsField{SevenDay: &rateWindow{UsedPercentage: intp(10)}})
	p := filepath.Join(dir, rateLimitHintDir, "old.json")
	past := time.Now().Add(-time.Minute)
	os.Chtimes(p, past, past)
	writeRateLimitHint(dir, "new", &rateLimitsField{SevenDay: &rateWindow{UsedPercentage: intp(31)}})

	usage := map[string]any{"pctWeekly": -1}
	applyRateLimitHints(usage, dir)
	if got := usage["pctWeekly"]; got != 31 {
		t.Errorf("pctWeekly = %v, want 31 from the newest hint", got)
	}
}
