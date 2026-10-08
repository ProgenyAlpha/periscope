package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func addHistoryRow(t *testing.T, db *sql.DB, sid string, ts time.Time, input, cr, cw, out int) {
	t.Helper()
	stamp := ts.UTC().Format(time.RFC3339)
	row := fmt.Sprintf(`{"sid":%q,"ts":%q,"input":%d,"cr":%d,"cw":%d,"out":%d,"cost":0,"turns":1}`,
		sid, stamp, input, cr, cw, out)
	if _, err := db.Exec("INSERT INTO history(ts, data) VALUES(?, ?)", stamp, row); err != nil {
		t.Fatalf("insert history: %v", err)
	}
}

// History rows carry cumulative counters, so a session already running when
// the window opened must contribute only what it burned inside the window.
// Counting its running total instead would inflate the inferred quota.
func TestAnnotateCapacitySubtractsPreWindowBaseline(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	reset := now.Add(2 * time.Hour) // a 5hr window that opened 3 hours ago

	// Started before the window and kept going: 100 weighted tokens inside.
	addHistoryRow(t, db, "old", now.Add(-4*time.Hour), 500, 0, 0, 0)
	addHistoryRow(t, db, "old", now.Add(-1*time.Hour), 600, 0, 0, 0)
	// Started inside the window: its single row counts in full.
	addHistoryRow(t, db, "new", now.Add(-30*time.Minute), 0, 0, 0, 8)

	snap := map[string]any{"reset5hr": reset.Format(time.RFC3339)}
	annotateCapacity(db, snap)

	// 100 from "old" (600-500), 40 from "new" (8 output tokens × 5).
	if got := snap["wt5hr"]; got != float64(140) {
		t.Errorf("wt5hr = %v, want 140", got)
	}
	if _, ok := snap["wtWeekly"]; ok {
		t.Error("annotated wtWeekly with no resetWeekly in the snapshot")
	}
}

// Cache reads are billed but do not consume rate limit, so they must not move
// the capacity figure at all.
func TestAnnotateCapacityIgnoresCacheReads(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()

	addHistoryRow(t, db, "s", now.Add(-10*time.Minute), 0, 9_000_000, 0, 0)

	snap := map[string]any{"reset5hr": now.Add(time.Hour).Format(time.RFC3339)}
	annotateCapacity(db, snap)

	if got, ok := snap["wt5hr"]; ok {
		t.Errorf("wt5hr = %v, want no annotation: cache reads weigh nothing", got)
	}
}

// A snapshot taken with no usage in the window gets no field rather than a
// zero, which is what keeps the trend filter honest and the payload small.
func TestAnnotateCapacitySkipsEmptyWindows(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()

	addHistoryRow(t, db, "s", now.Add(-9*24*time.Hour), 1000, 0, 0, 0)

	snap := map[string]any{
		"reset5hr":    now.Add(time.Hour).Format(time.RFC3339),
		"resetWeekly": now.Add(24 * time.Hour).Format(time.RFC3339),
	}
	annotateCapacity(db, snap)

	if len(snap) != 2 {
		t.Errorf("snapshot gained fields with no in-window usage: %v", snap)
	}
}

// Both windows are computed in one pass over history, and the weekly window
// takes in everything the 5hr one does.
func TestAnnotateCapacityAnnotatesBothWindows(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()

	addHistoryRow(t, db, "a", now.Add(-3*24*time.Hour), 0, 0, 200, 0) // weekly only
	addHistoryRow(t, db, "b", now.Add(-1*time.Hour), 0, 0, 50, 0)     // both

	snap := map[string]any{
		"reset5hr":    now.Add(2 * time.Hour).Format(time.RFC3339),
		"resetWeekly": now.Add(4 * 24 * time.Hour).Format(time.RFC3339),
	}
	annotateCapacity(db, snap)

	if got := snap["wt5hr"]; got != float64(50) {
		t.Errorf("wt5hr = %v, want 50", got)
	}
	if got := snap["wtWeekly"]; got != float64(250) {
		t.Errorf("wtWeekly = %v, want 250", got)
	}
}

// A malformed reset timestamp leaves that window unannotated instead of
// producing a window start from the zero time, which would sweep in everything.
func TestAnnotateCapacityIgnoresUnparseableReset(t *testing.T) {
	db := openTestDB(t)
	addHistoryRow(t, db, "s", time.Now().UTC(), 1000, 0, 0, 0)

	snap := map[string]any{"reset5hr": "not a timestamp"}
	annotateCapacity(db, snap)

	if _, ok := snap["wt5hr"]; ok {
		t.Errorf("annotated from an unparseable reset: %v", snap["wt5hr"])
	}
}

// The Stop hook appends percentage-only snapshots straight to the JSONL, so
// most rows reach limit_history with no capacity fields at all. The backfill is
// what makes the capacity trend work on the snapshots that actually exist.
func TestBackfillCapacityFillsHookSnapshots(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()

	addHistoryRow(t, db, "s", now.Add(-2*time.Hour), 0, 0, 0, 10)
	addHistoryRow(t, db, "s", now.Add(-1*time.Hour), 0, 0, 0, 30)

	ts := now.Add(-30 * time.Minute).Format(time.RFC3339)
	hookRow := fmt.Sprintf(`{"ts":%q,"pct5hr":12,"pctWeekly":38,"reset5hr":%q}`,
		ts, now.Add(2*time.Hour).Format(time.RFC3339))
	if _, err := db.Exec("INSERT INTO limit_history(ts, data) VALUES(?, ?)", ts, hookRow); err != nil {
		t.Fatal(err)
	}

	if err := BackfillCapacity(db); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var raw string
	if err := db.QueryRow("SELECT data FROM limit_history WHERE ts = ?", ts).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	// Both rows sit inside the window, so the first is the baseline:
	// 30 output tokens weighted, less the 10 already counted, times five.
	if got["wt5hr"] != float64(100) {
		t.Errorf("wt5hr = %v, want 100", got["wt5hr"])
	}
	if got["pct5hr"] != float64(12) {
		t.Errorf("backfill lost the original fields: %v", got)
	}
}

// A second pass must find nothing to do: the LIKE filter is what keeps the
// import loop from rewriting every snapshot on every tick.
func TestBackfillCapacityIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	addHistoryRow(t, db, "s", now.Add(-1*time.Hour), 100, 0, 0, 0)

	ts := now.Add(-30 * time.Minute).Format(time.RFC3339)
	row := fmt.Sprintf(`{"ts":%q,"reset5hr":%q}`, ts, now.Add(2*time.Hour).Format(time.RFC3339))
	if _, err := db.Exec("INSERT INTO limit_history(ts, data) VALUES(?, ?)", ts, row); err != nil {
		t.Fatal(err)
	}

	if err := BackfillCapacity(db); err != nil {
		t.Fatalf("first backfill: %v", err)
	}
	var first string
	db.QueryRow("SELECT data FROM limit_history WHERE ts = ?", ts).Scan(&first)

	if err := BackfillCapacity(db); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	var second string
	db.QueryRow("SELECT data FROM limit_history WHERE ts = ?", ts).Scan(&second)

	if first != second {
		t.Errorf("second pass rewrote the row:\n%s\n%s", first, second)
	}
}

// A snapshot older than the usage history has no window left to reconstruct.
// Guessing at one would invent a quota figure out of unrelated sessions.
func TestBackfillCapacitySkipsRowsBeyondRetention(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	addHistoryRow(t, db, "s", now.Add(-1*time.Hour), 100, 0, 0, 0)

	ts := now.Add(-HistoryRetention - 24*time.Hour).Format(time.RFC3339)
	row := fmt.Sprintf(`{"ts":%q,"reset5hr":%q}`, ts, now.Format(time.RFC3339))
	if _, err := db.Exec("INSERT INTO limit_history(ts, data) VALUES(?, ?)", ts, row); err != nil {
		t.Fatal(err)
	}

	if err := BackfillCapacity(db); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	var raw string
	db.QueryRow("SELECT data FROM limit_history WHERE ts = ?", ts).Scan(&raw)
	if raw != row {
		t.Errorf("annotated a snapshot older than the history: %s", raw)
	}
}

// A backfilled snapshot must reflect what had been burned when it was taken,
// not what has been burned since. Without that every row the backfill touches
// comes out with the same present-day figure and the trend reads flat.
func TestBackfillCapacityRespectsSnapshotTime(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	reset := now.Add(2 * time.Hour)

	addHistoryRow(t, db, "s", now.Add(-2*time.Hour), 100, 0, 0, 0)
	addHistoryRow(t, db, "s", now.Add(-90*time.Minute), 400, 0, 0, 0)
	addHistoryRow(t, db, "s", now.Add(-5*time.Minute), 9000, 0, 0, 0) // after the snapshot

	ts := now.Add(-1 * time.Hour).Format(time.RFC3339)
	row := fmt.Sprintf(`{"ts":%q,"reset5hr":%q}`, ts, reset.Format(time.RFC3339))
	if _, err := db.Exec("INSERT INTO limit_history(ts, data) VALUES(?, ?)", ts, row); err != nil {
		t.Fatal(err)
	}

	if err := BackfillCapacity(db); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var raw string
	db.QueryRow("SELECT data FROM limit_history WHERE ts = ?", ts).Scan(&raw)
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got["wt5hr"] != float64(300) {
		t.Errorf("wt5hr = %v, want 300: the 9000-token row lands after the snapshot", got["wt5hr"])
	}
}
