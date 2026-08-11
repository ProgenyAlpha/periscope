package forecast

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeHistory(t *testing.T, dir string, lines ...string) {
	t.Helper()
	var buf string
	for _, l := range lines {
		buf += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "usage-history.jsonl"), []byte(buf), 0644); err != nil {
		t.Fatal(err)
	}
}

func entry(ago time.Duration, sid string, cost float64) string {
	ts := time.Now().UTC().Add(-ago).Format("2006-01-02T15:04:05Z")
	return fmt.Sprintf(`{"ts":"%s","sid":"%s","cost":%v}`, ts, sid, cost)
}

// A session whose history begins inside the window because the Stop hook was
// not running earlier must not have its entire lifetime cost booked as burn.
func TestLocalBurnResumedSessionNotCountedInFull(t *testing.T) {
	dir := t.TempDir()
	writeHistory(t, dir,
		entry(50*time.Minute, "resumed", 642.39),
		entry(5*time.Minute, "resumed", 642.39),
	)
	got, ok := LocalBurn(dir, time.Hour)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got != 0 {
		t.Errorf("LocalBurn = %v, want 0 — session spent nothing in-window", got)
	}
}

func TestLocalBurnUsesInWindowDelta(t *testing.T) {
	dir := t.TempDir()
	writeHistory(t, dir,
		entry(50*time.Minute, "a", 10),
		entry(5*time.Minute, "a", 25),
		entry(40*time.Minute, "b", 100),
		entry(2*time.Minute, "b", 110),
	)
	got, ok := LocalBurn(dir, time.Hour)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got != 25 {
		t.Errorf("LocalBurn = %v, want 25 (15 + 10)", got)
	}
}

// A pre-window snapshot is the correct baseline when one exists.
func TestLocalBurnPrefersPreWindowBaseline(t *testing.T) {
	dir := t.TempDir()
	writeHistory(t, dir,
		entry(3*time.Hour, "a", 100),
		entry(30*time.Minute, "a", 140),
	)
	got, ok := LocalBurn(dir, time.Hour)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got != 40 {
		t.Errorf("LocalBurn = %v, want 40", got)
	}
}

func TestLocalBurnRateDividesByWindow(t *testing.T) {
	dir := t.TempDir()
	writeHistory(t, dir,
		entry(3*time.Hour, "a", 0),
		entry(30*time.Minute, "a", 20),
	)
	got, ok := LocalBurnRate(dir, 2*time.Hour)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got != 10 {
		t.Errorf("LocalBurnRate = %v, want 10", got)
	}
}
