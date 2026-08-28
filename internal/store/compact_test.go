package store

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mkEntry builds a limit_history entry the plan function can reason about.
func mkEntry(id int64, ts time.Time, pct5, pctWk float64) limitEntry {
	data, _ := json.Marshal(map[string]any{
		"ts":        ts.UTC().Format(time.RFC3339),
		"pct5hr":    pct5,
		"pctWeekly": pctWk,
	})
	return limitEntry{id: id, ts: ts.UTC(), data: string(data), pct5hr: pct5, pctWeekly: pctWk}
}

func idSet(ids []int64) map[int64]bool {
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// Anything inside the full-resolution window is untouchable — the live 6h and
// 24h ranges plot these samples directly.
func TestPlanLimitCompaction_KeepsRecentFullResolution(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	var all []limitEntry
	for i := 0; i < 600; i++ {
		ts := now.Add(-time.Duration(600-i) * time.Minute) // 10h ago .. now
		all = append(all, mkEntry(int64(i+1), ts, float64(i%100), float64(i%50)))
	}
	del := planLimitCompaction(all, now)
	if len(del) != 0 {
		t.Fatalf("deleted %d rows inside the %v full-resolution window, want 0", len(del), limitFullResolutionAge)
	}
}

// A rate-limit spike buried in the hourly tier must survive the rollup. A
// mean-per-bucket rollup would erase it; that is the whole point of this test.
func TestPlanLimitCompaction_PreservesPeaks(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	base := now.Add(-5 * 24 * time.Hour) // hourly tier

	var all []limitEntry
	var spikeID int64
	id := int64(1)
	for i := 0; i < 120; i++ { // 2 hours at 1-minute cadence
		pct := 10.0
		if i == 37 { // one lone spike, mid-bucket
			pct = 99.0
			spikeID = id
		}
		all = append(all, mkEntry(id, base.Add(time.Duration(i)*time.Minute), pct, 20))
		id++
	}

	deleted := idSet(planLimitCompaction(all, now))
	if deleted[spikeID] {
		t.Fatalf("the 99%% spike (id=%d) was compacted away — peaks must survive rollup", spikeID)
	}
	if len(deleted) < 100 {
		t.Fatalf("deleted only %d of 120 rows; the hourly tier is not thinning", len(deleted))
	}
}

// The trough after a 5h-window reset is the other half of the sawtooth. Losing
// it turns a reset edge into a plateau.
func TestPlanLimitCompaction_PreservesTroughs(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	base := now.Add(-5 * 24 * time.Hour).Truncate(time.Hour)

	var all []limitEntry
	var troughID int64
	id := int64(1)
	for i := 0; i < 60; i++ {
		// Rises to 90, resets to 3 at minute 40, climbs again.
		var pct float64
		switch {
		case i < 40:
			pct = 50 + float64(i)
		case i == 40:
			pct = 3
			troughID = id
		default:
			pct = 3 + float64(i-40)
		}
		all = append(all, mkEntry(id, base.Add(time.Duration(i)*time.Minute), pct, 20))
		id++
	}

	deleted := idSet(planLimitCompaction(all, now))
	if deleted[troughID] {
		t.Fatalf("the post-reset trough (id=%d) was compacted away", troughID)
	}
}

// pctWeekly moves on its own schedule; its peak must be kept even when it does
// not coincide with the pct5hr peak.
func TestPlanLimitCompaction_PreservesWeeklyPeak(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	base := now.Add(-10 * 24 * time.Hour).Truncate(time.Hour)

	var all []limitEntry
	var wkPeakID int64
	id := int64(1)
	for i := 0; i < 60; i++ {
		pct5 := float64(i) // rises monotonically, max at the end
		wk := 10.0         // flat...
		if i == 12 {       // ...except one weekly spike early in the bucket
			wk = 88
			wkPeakID = id
		}
		all = append(all, mkEntry(id, base.Add(time.Duration(i)*time.Minute), pct5, wk))
		id++
	}

	deleted := idSet(planLimitCompaction(all, now))
	if deleted[wkPeakID] {
		t.Fatalf("the pctWeekly peak (id=%d) was compacted away", wkPeakID)
	}
}

// Beyond 30 days the buckets go daily.
func TestPlanLimitCompaction_DailyBeyondThirtyDays(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	base := now.Add(-60 * 24 * time.Hour).Truncate(24 * time.Hour)

	var all []limitEntry
	id := int64(1)
	for i := 0; i < 24*60; i++ { // one full day at 1-minute cadence
		all = append(all, mkEntry(id, base.Add(time.Duration(i)*time.Minute), float64(i%97), 30))
		id++
	}
	total := len(all)
	kept := total - len(planLimitCompaction(all, now))
	if kept > 8 {
		t.Fatalf("kept %d rows for a single day beyond 30d, want a handful", kept)
	}
	if kept == 0 {
		t.Fatal("daily tier kept nothing")
	}
}

// 365d retention, matching HistoryRetention. Nothing inside the window is
// deleted outright; everything older goes.
func TestPlanLimitCompaction_RetentionMatchesHistory(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	inside := mkEntry(1, now.Add(-HistoryRetention).Add(time.Hour), 50, 50)
	outside := mkEntry(2, now.Add(-HistoryRetention).Add(-time.Hour), 50, 50)
	// Pad so the guard that skips tiny tables does not fire.
	all := []limitEntry{outside, inside}
	for i := 0; i < 200; i++ {
		all = append(all, mkEntry(int64(100+i), now.Add(-time.Duration(i)*time.Minute), 1, 1))
	}

	deleted := idSet(planLimitCompaction(all, now))
	if !deleted[2] {
		t.Fatalf("row older than the %v retention window was kept", HistoryRetention)
	}
	if deleted[1] {
		t.Fatal("row inside the retention window was deleted outright")
	}
}

// Compaction must converge: a second pass over already-compacted data must be
// a no-op, otherwise every hourly run keeps eroding the series.
func TestPlanLimitCompaction_IsIdempotent(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	all := syntheticLimitSeries(now, 40*24*time.Hour)

	first := idSet(planLimitCompaction(all, now))
	var survivors []limitEntry
	for _, e := range all {
		if !first[e.id] {
			survivors = append(survivors, e)
		}
	}
	second := planLimitCompaction(survivors, now)
	if len(second) != 0 {
		t.Fatalf("second pass at the same instant deleted %d more rows; compaction does not converge", len(second))
	}
}

// --- End-to-end over the DB, with the before/after numbers ---

// oldPlan is the policy this change replaces: a pure time-gap thinner that
// keeps whichever sample happens to come first in each window and therefore
// flattens spikes. Kept here only to produce an honest "before" number.
func oldPlan(all []limitEntry, now time.Time) []int64 {
	var del []int64
	var lastKept time.Time
	for _, e := range all {
		age := now.Sub(e.ts)
		var minGap time.Duration
		switch {
		case age < 24*time.Hour:
			minGap = 0
		case age < 7*24*time.Hour:
			minGap = 5 * time.Minute
		case age < 30*24*time.Hour:
			minGap = 60 * time.Minute
		default:
			minGap = 4 * time.Hour
		}
		if minGap > 0 && !lastKept.IsZero() && e.ts.Sub(lastKept) < minGap {
			del = append(del, e.id)
		} else {
			lastKept = e.ts
		}
	}
	return del
}

// syntheticLimitSeries reproduces the shape of the live table: a snapshot every
// 60s, pct5hr sawtoothing over a 5h window, pctWeekly over 7d, with the
// occasional hard spike to 100.
func syntheticLimitSeries(now time.Time, span time.Duration) []limitEntry {
	var all []limitEntry
	start := now.Add(-span)
	id := int64(1)
	for t := start; t.Before(now); t = t.Add(time.Minute) {
		el5 := t.Sub(start) % (5 * time.Hour)
		elWk := t.Sub(start) % (7 * 24 * time.Hour)
		pct5 := 100 * el5.Hours() / 5
		wk := 100 * elWk.Hours() / (7 * 24)
		if id%3600 == 0 {
			pct5 = 100 // hard spike
		}
		all = append(all, mkEntry(id, t, math.Round(pct5), math.Round(wk)))
		id++
	}
	return all
}

// The headline number: how much smaller the table gets, and proof the spikes
// are still there afterwards.
func TestCompactLimitHistory_ReductionAndPeakSurvival(t *testing.T) {
	now := time.Now().UTC()
	// 191 days, matching the live table's 2026-02-18 -> now span.
	all := syntheticLimitSeries(now, 191*24*time.Hour)

	oldDel := idSet(oldPlan(all, now))
	newDel := idSet(planLimitCompaction(all, now))

	oldKept := len(all) - len(oldDel)
	newKept := len(all) - len(newDel)
	t.Logf("raw=%d rows  oldPolicy=%d rows  newPolicy=%d rows  (%.1f%% fewer than old)",
		len(all), oldKept, newKept, 100*float64(oldKept-newKept)/float64(oldKept))

	if newKept >= oldKept {
		t.Fatalf("new policy keeps %d rows, old kept %d — no reduction", newKept, oldKept)
	}

	// The property that matters: for every rollup bucket, the highest (and
	// lowest) pct5hr among the surviving rows must equal the highest (lowest)
	// among all the rows that bucket started with. That is what "a spike stays
	// visible" means on a line chart. The old gap thinner does not have this
	// property, and the test says by how much.
	oldMissedPeaks := bucketsLosingExtremes(all, oldDel, now)
	newMissedPeaks := bucketsLosingExtremes(all, newDel, now)
	t.Logf("buckets whose peak or trough was lost: oldPolicy=%d newPolicy=%d", oldMissedPeaks, newMissedPeaks)
	if newMissedPeaks != 0 {
		t.Fatalf("new policy flattened %d buckets", newMissedPeaks)
	}
	if oldMissedPeaks == 0 {
		t.Fatal("the old policy was supposed to be the lossy one; the fixture is not exercising it")
	}
}

// bucketsLosingExtremes counts rollup buckets where the surviving rows no
// longer reach the bucket's true min or max pct5hr.
func bucketsLosingExtremes(all []limitEntry, deleted map[int64]bool, now time.Time) int {
	type span struct{ allMin, allMax, keptMin, keptMax float64 }
	buckets := map[limitBucketKey]*span{}
	for _, e := range all {
		age := now.Sub(e.ts)
		if age <= limitFullResolutionAge || age > HistoryRetention {
			continue
		}
		var k limitBucketKey
		if age <= limitHourlyAge {
			k = limitBucketKey{tier: 1, start: e.ts.UTC().Truncate(time.Hour)}
		} else {
			k = limitBucketKey{tier: 2, start: e.ts.UTC().Truncate(limitDailyBucket)}
		}
		b := buckets[k]
		if b == nil {
			b = &span{allMin: math.Inf(1), allMax: math.Inf(-1), keptMin: math.Inf(1), keptMax: math.Inf(-1)}
			buckets[k] = b
		}
		b.allMin = math.Min(b.allMin, e.pct5hr)
		b.allMax = math.Max(b.allMax, e.pct5hr)
		if !deleted[e.id] {
			b.keptMin = math.Min(b.keptMin, e.pct5hr)
			b.keptMax = math.Max(b.keptMax, e.pct5hr)
		}
	}
	lost := 0
	for _, b := range buckets {
		if b.keptMax != b.allMax || b.keptMin != b.allMin {
			lost++
		}
	}
	return lost
}

// The real entry point, over a real database, with the JSONL rewritten.
func TestCompactLimitHistory_EndToEnd(t *testing.T) {
	db := openTestDB(t)
	dataDir := t.TempDir()

	now := time.Now().UTC()
	base := now.Add(-45 * 24 * time.Hour)
	spikeTS := base.Add(90 * time.Minute)
	inserted := 0
	for i := 0; i < 60*24*3; i++ { // 3 days at 1-minute cadence, all in the daily/hourly tiers
		ts := base.Add(time.Duration(i) * time.Minute)
		pct := float64(i % 90)
		if ts.Equal(spikeTS) {
			pct = 100
		}
		data := fmt.Sprintf(`{"ts":%q,"pct5hr":%v,"pctWeekly":17}`, ts.Format(time.RFC3339), pct)
		mustExec(t, db, "INSERT INTO limit_history(ts, data) VALUES(?, ?)", ts.Format(time.RFC3339), data)
		inserted++
	}

	before := countRows(t, db, "limit_history")
	if err := CompactLimitHistory(db, dataDir); err != nil {
		t.Fatalf("CompactLimitHistory: %v", err)
	}
	after := countRows(t, db, "limit_history")
	t.Logf("end-to-end: %d rows -> %d rows (%.1f%% reduction)", before, after,
		100*float64(before-after)/float64(before))

	if after >= before/4 {
		t.Fatalf("rows = %d, want a large reduction from %d", after, before)
	}
	if after == 0 {
		t.Fatal("compaction removed everything")
	}

	var spikes int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM limit_history WHERE ts = ?`, spikeTS.Format(time.RFC3339)).Scan(&spikes); err != nil {
		t.Fatalf("spike query: %v", err)
	}
	if spikes != 1 {
		t.Fatalf("the 100%% spike row is gone after compaction")
	}

	raw, err := os.ReadFile(filepath.Join(dataDir, "limit-history.jsonl"))
	if err != nil {
		t.Fatalf("JSONL not rewritten: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("JSONL is empty")
	}
}

// Rows that survive must be untouched originals — the dashboard reads reset5hr
// and resetWeekly off them, so a synthesised average row would break the widget.
func TestCompactLimitHistory_KeepsOriginalRowsVerbatim(t *testing.T) {
	db := openTestDB(t)
	dataDir := t.TempDir()

	now := time.Now().UTC()
	base := now.Add(-10 * 24 * time.Hour)
	want := map[string]string{}
	for i := 0; i < 600; i++ {
		ts := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		data := fmt.Sprintf(`{"ts":%q,"pct5hr":%d,"pctWeekly":7,"reset5hr":"2026-08-20T05:00:00Z","resetWeekly":"2026-08-24T00:00:00Z"}`, ts, i%80)
		mustExec(t, db, "INSERT INTO limit_history(ts, data) VALUES(?, ?)", ts, data)
		want[ts] = data
	}

	if err := CompactLimitHistory(db, dataDir); err != nil {
		t.Fatalf("CompactLimitHistory: %v", err)
	}

	rows, err := db.Query("SELECT ts, data FROM limit_history")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var ts, data string
		if err := rows.Scan(&ts, &data); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if want[ts] != data {
			t.Fatalf("surviving row for %s was rewritten:\n got  %s\n want %s", ts, data, want[ts])
		}
		n++
	}
	if n == 0 {
		t.Fatal("nothing survived")
	}
}
