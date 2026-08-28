package store

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// mkHist builds one history row exactly as SnapshotSidecarsToHistory writes it.
func mkHist(sid string, ts time.Time, cost float64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"cost":%g,"cr":10,"cw":20,"input":30,"out":40,"sid":%q,"ts":%q,"turns":1}`,
		cost, sid, ts.UTC().Format(time.RFC3339)))
}

func histField(t *testing.T, raw json.RawMessage, key string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("row is not JSON: %v", err)
	}
	return m[key]
}

// The rollup must never invent a row. Every survivor is one of the originals,
// byte for byte, exactly like planLimitCompaction.
func TestRollupHistory_EmitsOriginalRowsOnly(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	var in []json.RawMessage
	for i := 0; i < 200; i++ {
		in = append(in, mkHist("sess0001", now.Add(-90*24*time.Hour+time.Duration(i)*7*time.Minute), float64(i)))
	}
	originals := map[string]bool{}
	for _, r := range in {
		originals[string(r)] = true
	}
	out := RollupHistory(in, now)
	if len(out) == 0 || len(out) >= len(in) {
		t.Fatalf("rollup produced %d rows from %d; want a strict reduction", len(out), len(in))
	}
	for _, r := range out {
		if !originals[string(r)] {
			t.Fatalf("rollup synthesised a row not present in the input: %s", r)
		}
	}
}

// The whole point: a single expensive day must still be findable after the
// rollup. A mean-per-bucket would smear this away.
func TestRollupHistory_PreservesTheSpikeDay(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	spikeDay := now.Add(-100 * 24 * time.Hour).Truncate(24 * time.Hour)

	var in []json.RawMessage
	// 100 quiet days, one session each, tiny cost.
	for d := 200; d > 100; d-- {
		day := now.Add(-time.Duration(d) * 24 * time.Hour)
		in = append(in, mkHist("quiet001", day, 0.10))
		in = append(in, mkHist("quiet001", day.Add(time.Hour), 0.20))
	}
	// The spike day: cost climbs from 0 to 500 across 300 snapshots.
	for i := 0; i < 300; i++ {
		in = append(in, mkHist("spike001", spikeDay.Add(time.Duration(i)*4*time.Minute), float64(i)*500.0/299.0))
	}
	out := RollupHistory(in, now)

	var maxCost float64
	for _, r := range out {
		if c, ok := histField(t, r, "cost").(float64); ok && c > maxCost {
			maxCost = c
		}
	}
	if maxCost < 500 {
		t.Fatalf("peak cost after rollup = %v, want the original 500 to survive", maxCost)
	}
}

// history rows are per-session CUMULATIVE counters and every widget computes
// max-min per session. Keeping the first and last original row of each
// (session, bucket) makes those deltas exact, not approximate.
func TestRollupHistory_SessionDeltaIsExact(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	start := now.Add(-120 * 24 * time.Hour)

	var in []json.RawMessage
	for i := 0; i < 500; i++ {
		in = append(in, mkHist("sessAAAA", start.Add(time.Duration(i)*20*time.Minute), float64(i)*0.5))
	}
	wantDelta := 499 * 0.5

	out := RollupHistory(in, now)
	var lo, hi float64
	lo = -1
	for _, r := range out {
		c := histField(t, r, "cost").(float64)
		if lo < 0 || c < lo {
			lo = c
		}
		if c > hi {
			hi = c
		}
	}
	if got := hi - lo; got != wantDelta {
		t.Fatalf("session delta after rollup = %v, want exactly %v", got, wantDelta)
	}
}

// Anything inside the full-resolution tier is untouched.
func TestRollupHistory_RecentWindowIsVerbatim(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	var in []json.RawMessage
	for i := 0; i < 120; i++ {
		in = append(in, mkHist("recent01", now.Add(-time.Duration(i)*time.Hour), float64(i)))
	}
	out := RollupHistory(in, now)
	if len(out) != len(in) {
		t.Fatalf("rows inside %v = %d, want all %d kept verbatim",
			HistoryFullResolutionAge, len(out), len(in))
	}
}

// Two sessions active in the same hour must not compete for the same slot.
func TestRollupHistory_SessionsDoNotShareBuckets(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-100 * 24 * time.Hour)
	var in []json.RawMessage
	for i := 0; i < 40; i++ {
		in = append(in, mkHist("aaaaaaaa", old.Add(time.Duration(i)*time.Minute), float64(i)))
		in = append(in, mkHist("bbbbbbbb", old.Add(time.Duration(i)*time.Minute), float64(i)*2))
	}
	out := RollupHistory(in, now)
	seen := map[string]bool{}
	for _, r := range out {
		seen[histField(t, r, "sid").(string)] = true
	}
	if !seen["aaaaaaaa"] || !seen["bbbbbbbb"] {
		t.Fatalf("rollup dropped a whole session: %v", seen)
	}
}

// A row whose ts cannot be parsed is data we do not understand; keep it rather
// than silently deleting it from the transport.
func TestRollupHistory_UnparseableRowsSurvive(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	in := []json.RawMessage{
		json.RawMessage(`{"sid":"weird001","ts":"not-a-time","cost":1}`),
		json.RawMessage(`{"sid":"weird002","cost":2}`),
	}
	out := RollupHistory(in, now)
	if len(out) != 2 {
		t.Fatalf("kept %d of 2 unparseable rows, want both", len(out))
	}
}

func TestRollupHistory_NilAndEmpty(t *testing.T) {
	now := time.Now()
	if got := RollupHistory(nil, now); len(got) != 0 {
		t.Fatalf("nil input -> %d rows", len(got))
	}
	if got := RollupHistory([]json.RawMessage{}, now); len(got) != 0 {
		t.Fatalf("empty input -> %d rows", len(got))
	}
}

// BucketHistoryUniform is what an explicit ?bucket= asks for: one flat bucket
// width over the whole range, no tiers.
func TestBucketHistoryUniform_OneBucketWidth(t *testing.T) {
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	var in []json.RawMessage
	for i := 0; i < 60; i++ {
		in = append(in, mkHist("sessAAAA", base.Add(time.Duration(i)*time.Minute), float64(i)))
	}
	out := BucketHistoryUniform(in, time.Hour)
	if len(out) != 2 {
		t.Fatalf("one hour of one session -> %d rows, want 2 (first and last)", len(out))
	}
	if got := histField(t, out[0], "cost").(float64); got != 0 {
		t.Fatalf("first kept row cost = %v, want 0", got)
	}
	if got := histField(t, out[1], "cost").(float64); got != 59 {
		t.Fatalf("last kept row cost = %v, want 59", got)
	}
}

func TestBucketHistoryUniform_ZeroBucketIsIdentity(t *testing.T) {
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	var in []json.RawMessage
	for i := 0; i < 10; i++ {
		in = append(in, mkHist("sessAAAA", base.Add(time.Duration(i)*time.Minute), float64(i)))
	}
	if got := BucketHistoryUniform(in, 0); len(got) != len(in) {
		t.Fatalf("bucket=0 -> %d rows, want the input untouched (%d)", len(got), len(in))
	}
}

// --- QueryHistory ---

func TestQueryHistory_ZeroQueryMatchesTheLegacyQuery(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		ts := base.Add(time.Duration(i) * time.Hour)
		if _, err := db.Exec("INSERT INTO history(ts, data) VALUES(?, ?)",
			ts.Format(time.RFC3339), string(mkHist("sessAAAA", ts, float64(i)))); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	legacy, err := queryRawColumn(db, "history",
		"SELECT id, data FROM history ORDER BY replace(ts, ' ', 'T') ASC")
	if err != nil {
		t.Fatalf("legacy: %v", err)
	}
	got, err := QueryHistory(db, HistoryQuery{})
	if err != nil {
		t.Fatalf("QueryHistory: %v", err)
	}
	if len(got) != len(legacy) {
		t.Fatalf("zero HistoryQuery returned %d rows, legacy returned %d", len(got), len(legacy))
	}
	for i := range got {
		if string(got[i]) != string(legacy[i]) {
			t.Fatalf("row %d differs:\n got %s\nwant %s", i, got[i], legacy[i])
		}
	}
}

func TestQueryHistory_FromToWindow(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 48; i++ {
		ts := base.Add(time.Duration(i) * time.Hour)
		if _, err := db.Exec("INSERT INTO history(ts, data) VALUES(?, ?)",
			ts.Format(time.RFC3339), string(mkHist("sessAAAA", ts, float64(i)))); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	from := base.Add(10 * time.Hour)
	to := base.Add(20 * time.Hour)
	got, err := QueryHistory(db, HistoryQuery{From: from, To: to})
	if err != nil {
		t.Fatalf("QueryHistory: %v", err)
	}
	if len(got) != 11 {
		t.Fatalf("window [10h,20h] returned %d rows, want 11", len(got))
	}
	for _, r := range got {
		ts, _ := time.Parse(time.RFC3339, histField(t, r, "ts").(string))
		if ts.Before(from) || ts.After(to) {
			t.Fatalf("row outside the requested window: %s", r)
		}
	}
}

// Legacy rows written by the CURRENT_TIMESTAMP default use a space separator.
func TestQueryHistory_WindowMatchesSpaceSeparatedTimestamps(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if _, err := db.Exec("INSERT INTO history(ts, data) VALUES(?, ?)",
		base.Format("2006-01-02 15:04:05"), string(mkHist("legacy01", base, 1))); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := QueryHistory(db, HistoryQuery{
		From: base.Add(-time.Hour), To: base.Add(time.Hour)})
	if err != nil {
		t.Fatalf("QueryHistory: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("space-separated row matched %d times, want 1", len(got))
	}
}

func TestQueryHistory_NoneReturnsEmptyNotNil(t *testing.T) {
	db := openTestDB(t)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if _, err := db.Exec("INSERT INTO history(ts, data) VALUES(?, ?)",
		base.Format(time.RFC3339), string(mkHist("sessAAAA", base, 1))); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := QueryHistory(db, HistoryQuery{Mode: HistoryNone})
	if err != nil {
		t.Fatalf("QueryHistory: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("HistoryNone returned %v, want a non-nil empty slice", got)
	}
}

// BuildDashboardData's existing two-argument form must keep producing exactly
// what it produces today — that is the whole backward-compatibility promise.
func TestBuildDashboardData_DefaultHistoryUnchanged(t *testing.T) {
	db := openTestDB(t)
	old := time.Now().Add(-200 * 24 * time.Hour)
	for i := 0; i < 50; i++ {
		ts := old.Add(time.Duration(i) * time.Minute)
		if _, err := db.Exec("INSERT INTO history(ts, data) VALUES(?, ?)",
			ts.UTC().Format(time.RFC3339), string(mkHist("sessAAAA", ts, float64(i)))); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	d, err := BuildDashboardData(db, "")
	if err != nil {
		t.Fatalf("BuildDashboardData: %v", err)
	}
	if len(d.History) != 50 {
		t.Fatalf("default BuildDashboardData returned %d history rows, want all 50", len(d.History))
	}
	r, err := BuildDashboardDataQuery(db, "", HistoryQuery{Mode: HistoryRollup})
	if err != nil {
		t.Fatalf("BuildDashboardDataQuery: %v", err)
	}
	if len(r.History) >= 50 {
		t.Fatalf("rollup returned %d rows, want fewer than 50", len(r.History))
	}
}

func TestCurrentSchemaVersionAccessor(t *testing.T) {
	if CurrentSchemaVersion() != currentSchemaVersion {
		t.Fatalf("CurrentSchemaVersion() = %d, want %d", CurrentSchemaVersion(), currentSchemaVersion)
	}
}

// activity-breakdown bins every history row by hour-of-day to draw a lifetime
// heatmap and a PEAK HOUR. Row counts are the one thing a rollup cannot
// preserve — thinning 300 snapshots in an hour down to 2 changes the count by
// construction — so the exact hourly histogram travels alongside the thinned
// array instead of the megabytes of rows it summarises.
func TestHistoryHourlyCounts_IsExactAndRollupIndependent(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	var in []json.RawMessage
	hour := time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC)
	for i := 0; i < 300; i++ {
		in = append(in, mkHist("busy0001", hour.Add(time.Duration(i)*10*time.Second), float64(i)))
	}
	in = append(in, mkHist("other001", time.Date(2026, 3, 2, 17, 30, 0, 0, time.UTC), 1))

	got := HistoryHourlyCounts(in)
	if got == nil {
		t.Fatal("HistoryHourlyCounts returned nil for a non-empty input")
	}
	if got.Start != "2026-03-01T03:00:00Z" {
		t.Fatalf("Start = %q, want the first row's hour", got.Start)
	}
	// 2026-03-01T03:00Z .. 2026-03-02T17:00Z inclusive is 39 hours.
	if len(got.Counts) != 39 {
		t.Fatalf("len(Counts) = %d, want 39", len(got.Counts))
	}
	if got.Counts[0] != 300 {
		t.Fatalf("first bucket = %d, want the true 300", got.Counts[0])
	}
	if got.Counts[38] != 1 {
		t.Fatalf("last bucket = %d, want 1", got.Counts[38])
	}
	var total int
	for _, n := range got.Counts {
		total += n
	}
	if total != len(in) {
		t.Fatalf("histogram totals %d, want every one of the %d rows", total, len(in))
	}
	if len(RollupHistory(in, now)) >= len(in) {
		t.Fatal("precondition: the rollup should have thinned this input")
	}
}

func TestHistoryHourlyCounts_SkipsUnparseableRows(t *testing.T) {
	got := HistoryHourlyCounts([]json.RawMessage{
		json.RawMessage(`{"sid":"a","ts":"nope"}`),
		json.RawMessage(`not json`),
		json.RawMessage(`{"sid":"a","ts":"2026-03-01T05:00:00Z"}`),
	})
	if got == nil || len(got.Counts) != 1 || got.Counts[0] != 1 {
		t.Fatalf("got %+v, want a single bucket holding the one readable row", got)
	}
	if got.Start != "2026-03-01T05:00:00Z" {
		t.Fatalf("Start = %q", got.Start)
	}
}

func TestHistoryHourlyCounts_EmptyIsNil(t *testing.T) {
	if got := HistoryHourlyCounts(nil); got != nil {
		t.Fatalf("nil input -> %+v, want nil", got)
	}
	if got := HistoryHourlyCounts([]json.RawMessage{json.RawMessage(`{"ts":"bad"}`)}); got != nil {
		t.Fatalf("no readable rows -> %+v, want nil", got)
	}
}

// The histogram rides along only when the array is actually thinned. A full
// response already has every row, so adding a field to it would change the
// default payload for no reason.
func TestBuildDashboardDataQuery_HourlyOnlyWithRollup(t *testing.T) {
	db := openTestDB(t)
	old := time.Now().Add(-200 * 24 * time.Hour)
	for i := 0; i < 50; i++ {
		ts := old.Add(time.Duration(i) * time.Minute)
		if _, err := db.Exec("INSERT INTO history(ts, data) VALUES(?, ?)",
			ts.UTC().Format(time.RFC3339), string(mkHist("sessAAAA", ts, float64(i)))); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	full, err := BuildDashboardData(db, "")
	if err != nil {
		t.Fatalf("BuildDashboardData: %v", err)
	}
	if full.HistoryHourly != nil {
		t.Fatalf("default payload grew a historyHourly field: %+v", full.HistoryHourly)
	}
	rolled, err := BuildDashboardDataQuery(db, "", HistoryQuery{Mode: HistoryRollup})
	if err != nil {
		t.Fatalf("BuildDashboardDataQuery: %v", err)
	}
	if rolled.HistoryHourly == nil {
		t.Fatal("rollup payload has no historyHourly")
	}
	var total int
	for _, n := range rolled.HistoryHourly.Counts {
		total += n
	}
	if total != 50 {
		t.Fatalf("historyHourly totals %d, want all 50 rows", total)
	}
}
