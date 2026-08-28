package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mustFinish runs fn and fails the test if it has not returned within d.
// Several of these tests exercise code paths that used to hold a read cursor
// open across a write on a pool limited to one connection — the symptom is a
// hard deadlock, not a wrong answer, so the assertion has to be a timeout.
func mustFinish(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not finish within %s — deadlocked on the connection pool", what, d)
	}
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func uuid(n int) string {
	return fmt.Sprintf("%08d-0000-4000-8000-%012d", n, n)
}

func writeSidecar(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	return p
}

// --- Finding 8: write-while-iterating pins the WAL ---

func TestSnapshotSidecarsToHistory_DoesNotWriteWhileCursorOpen(t *testing.T) {
	db := openTestDB(t)
	db.SetMaxOpenConns(1) // no-op once OpenDB does this; explicit for intent

	for i := 0; i < 5; i++ {
		mustExec(t, db, "INSERT INTO sessions(id, data, updated_at) VALUES(?, ?, ?)",
			uuid(i), fmt.Sprintf(`{"cumulative":{"cost":%d.5}}`, i), "2026-01-01T00:00:00Z")
	}

	last := map[string]float64{}
	mustFinish(t, 15*time.Second, "SnapshotSidecarsToHistory", func() {
		if err := SnapshotSidecarsToHistory(db, last); err != nil {
			t.Errorf("SnapshotSidecarsToHistory: %v", err)
		}
	})

	if n := countRows(t, db, "history"); n != 5 {
		t.Fatalf("history rows = %d, want 5", n)
	}
}

func TestSnapshotSidecarsToHistory_ReturnsQueryError(t *testing.T) {
	db := openTestDB(t)
	mustExec(t, db, "DROP TABLE sessions")
	if err := SnapshotSidecarsToHistory(db, map[string]float64{}); err == nil {
		t.Fatal("expected an error when the sessions query fails, got nil")
	}
}

// Finding 21: the dedup key is in-memory only, so every restart re-emitted one
// duplicate snapshot row per session.
func TestSnapshotSidecarsToHistory_SurvivesRestart(t *testing.T) {
	db := openTestDB(t)
	mustExec(t, db, "INSERT INTO sessions(id, data, updated_at) VALUES(?, ?, ?)",
		uuid(1), `{"cumulative":{"cost":1.5,"agent_calls":1}}`, "2026-01-01T00:00:00Z")

	if err := SnapshotSidecarsToHistory(db, map[string]float64{}); err != nil {
		t.Fatalf("first snapshot: %v", err)
	}

	// Backdate the row's ts column, leaving its payload alone. The unique
	// (ts, data) index can no longer catch a repeat, so only a dedup key
	// rebuilt from history itself can.
	mustExec(t, db, "UPDATE history SET ts = '2020-01-01T00:00:00Z'")

	// Fresh map == process restart.
	if err := SnapshotSidecarsToHistory(db, map[string]float64{}); err != nil {
		t.Fatalf("second snapshot: %v", err)
	}

	if n := countRows(t, db, "history"); n != 1 {
		t.Fatalf("history rows after simulated restart = %d, want 1", n)
	}
}

// --- Finding 7: connection pool limits ---

func TestOpenDB_ConstrainsConnectionPool(t *testing.T) {
	db := openTestDB(t)
	st := db.Stats()
	if st.MaxOpenConnections != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1 (SQLite has a single writer)", st.MaxOpenConnections)
	}
}

// --- Findings 3 + 17: WAL growth and synchronous=FULL ---

func TestOpenDB_WALPragmas(t *testing.T) {
	db := openTestDB(t)

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}

	var sync int
	if err := db.QueryRow("PRAGMA synchronous").Scan(&sync); err != nil {
		t.Fatalf("synchronous: %v", err)
	}
	if sync != 1 {
		t.Fatalf("synchronous = %d, want 1 (NORMAL) under WAL", sync)
	}

	var limit int64
	if err := db.QueryRow("PRAGMA journal_size_limit").Scan(&limit); err != nil {
		t.Fatalf("journal_size_limit: %v", err)
	}
	if limit <= 0 {
		t.Fatalf("journal_size_limit = %d, want a positive cap so the WAL cannot ratchet upward", limit)
	}
}

func TestCheckpointWAL_TruncatesTheWALFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "wal.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	for i := 0; i < 2000; i++ {
		mustExec(t, db, "INSERT INTO history(ts, data) VALUES(?, ?)",
			fmt.Sprintf("2026-01-01T00:00:%02dZ", i%60), fmt.Sprintf(`{"i":%d,"pad":"%s"}`, i, strings.Repeat("x", 512)))
	}

	walPath := dbPath + "-wal"
	before := int64(0)
	if fi, err := os.Stat(walPath); err == nil {
		before = fi.Size()
	}
	if before == 0 {
		t.Skip("no WAL file materialised; nothing to checkpoint")
	}

	if err := CheckpointWAL(db); err != nil {
		t.Fatalf("CheckpointWAL: %v", err)
	}

	fi, err := os.Stat(walPath)
	if err != nil {
		return // truncate-to-nothing is also an acceptable outcome
	}
	if fi.Size() >= before {
		t.Fatalf("WAL size after checkpoint = %d, want < %d", fi.Size(), before)
	}
	if fi.Size() != 0 {
		t.Fatalf("wal_checkpoint(TRUNCATE) left %d bytes behind", fi.Size())
	}

	// Data must survive the checkpoint.
	if n := countRows(t, db, "history"); n != 2000 {
		t.Fatalf("history rows after checkpoint = %d, want 2000", n)
	}
}

// --- Finding 16: DB file must not be world-readable ---

func TestOpenDB_CreatesPrivateFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fresh.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	// Force the WAL/shm sidecars into existence.
	mustExec(t, db, "INSERT INTO kv(key, value) VALUES('k','v')")

	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			continue // sidecar may not exist yet
		}
		if mode := fi.Mode().Perm(); mode != 0600 {
			t.Errorf("%s mode = %04o, want 0600 (it holds the VAPID private key)", filepath.Base(p), mode)
		}
	}
}

// --- Finding 4 (reader half): invalid sidecar JSON must never be stored ---

func TestImportSidecars_RejectsInvalidJSON(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()

	writeSidecar(t, dir, uuid(1)+".json", `{"cumulative":{"cost":1.0}}`)
	writeSidecar(t, dir, uuid(2)+".json", `{"cumulative":{"cost":`) // truncated
	writeSidecar(t, dir, uuid(3)+".json", ``)                       // empty

	st, err := ImportSidecars(db, dir)
	if err != nil {
		t.Fatalf("ImportSidecars: %v", err)
	}
	if st.Imported != 1 {
		t.Fatalf("Imported = %d, want 1", st.Imported)
	}
	if st.Invalid != 2 {
		t.Fatalf("Invalid = %d, want 2", st.Invalid)
	}

	var stored int
	db.QueryRow("SELECT COUNT(*) FROM sessions WHERE data = '' OR json_valid(data) = 0").Scan(&stored)
	if stored != 0 {
		t.Fatalf("%d invalid sidecar rows reached the sessions table", stored)
	}
	if n := countRows(t, db, "sessions"); n != 1 {
		t.Fatalf("sessions rows = %d, want 1", n)
	}
}

func TestBuildDashboardData_PayloadAlwaysEncodes(t *testing.T) {
	db := openTestDB(t)

	// A poisoned row that predates the validating importer.
	mustExec(t, db, "INSERT INTO sessions(id, data, updated_at) VALUES(?, ?, ?)",
		uuid(1), ``, "2026-01-02T00:00:00Z")
	mustExec(t, db, "INSERT INTO sessions(id, data, updated_at) VALUES(?, ?, ?)",
		uuid(2), `{"cumulative":{"cost":1.0}}`, "2026-01-01T00:00:00Z")
	mustExec(t, db, "INSERT INTO history(ts, data) VALUES(?, ?)", "2026-01-01T00:00:00Z", `{"cost":`)
	mustExec(t, db, "INSERT INTO history(ts, data) VALUES(?, ?)", "2026-01-02T00:00:00Z", `{"cost":2}`)
	mustExec(t, db, "INSERT INTO limit_history(ts, data) VALUES(?, ?)", "2026-01-01T00:00:00Z", ``)

	d, err := BuildDashboardData(db, "")
	if err != nil {
		t.Fatalf("BuildDashboardData: %v", err)
	}

	if _, err := json.Marshal(d); err != nil {
		t.Fatalf("dashboard payload failed to encode — one bad row took down the whole response: %v", err)
	}
	if len(d.Sidecars) != 1 {
		t.Fatalf("sidecars = %d, want 1 (the corrupt row must be dropped)", len(d.Sidecars))
	}
	if len(d.History) != 1 {
		t.Fatalf("history = %d, want 1", len(d.History))
	}
	if len(d.LimitHistory) != 0 {
		t.Fatalf("limitHistory = %d, want 0", len(d.LimitHistory))
	}
}

// --- Finding 13: query errors must not render as a silently empty dashboard ---

func TestBuildDashboardData_ReturnsQueryErrors(t *testing.T) {
	for _, table := range []string{"sessions", "history", "limit_history"} {
		t.Run(table, func(t *testing.T) {
			db := openTestDB(t)
			mustExec(t, db, "DROP TABLE "+table)
			if _, err := BuildDashboardData(db, ""); err == nil {
				t.Fatalf("dropping %s produced no error — the dashboard would render empty and silent", table)
			}
		})
	}
}

// --- Finding 5: ImportSidecars reported success on total failure ---

func TestImportSidecars_EmptyDirIsNotAnError(t *testing.T) {
	db := openTestDB(t)
	st, err := ImportSidecars(db, t.TempDir())
	if err != nil {
		t.Fatalf("empty dir: %v", err)
	}
	if st.Total != 0 || st.Imported != 0 {
		t.Fatalf("stats = %+v, want an all-zero result", st)
	}
}

func TestImportSidecars_MissingDirIsAnError(t *testing.T) {
	db := openTestDB(t)
	if _, err := ImportSidecars(db, filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected an error for a missing data directory")
	}
}

func TestImportSidecars_TotalFailureIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: unreadable files are still readable")
	}
	db := openTestDB(t)
	dir := t.TempDir()
	p := writeSidecar(t, dir, uuid(1)+".json", `{"cumulative":{"cost":1.0}}`)
	if err := os.Chmod(p, 0000); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	st, err := ImportSidecars(db, dir)
	if err == nil {
		t.Fatalf("every sidecar failed to read but ImportSidecars returned nil (stats=%+v)", st)
	}
	if st.Failed != 1 {
		t.Fatalf("Failed = %d, want 1", st.Failed)
	}
}

func TestImportFileData_PropagatesSidecarFailure(t *testing.T) {
	db := openTestDB(t)
	missing := filepath.Join(t.TempDir(), "nope")
	if err := ImportFileData(db, missing, t.TempDir()); err == nil {
		t.Fatal("ImportFileData swallowed a missing data directory")
	}
}

// --- Finding 14: sidecar import must be an allowlist ---

func TestImportSidecars_OnlyAcceptsSessionUUIDs(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()

	writeSidecar(t, dir, uuid(1)+".json", `{"cumulative":{"cost":1.0}}`)
	// Neither of these is a session; both used to become sessions rows and
	// their cumulative.cost fed the phantom-usage baseline.
	writeSidecar(t, dir, "settings-backup.json", `{"cumulative":{"cost":999.0}}`)
	writeSidecar(t, dir, "ratelimit-hints.json", `{"cumulative":{"cost":999.0}}`)
	writeSidecar(t, dir, "not-a-uuid-at-all-x.json", `{"cumulative":{"cost":999.0}}`)

	st, err := ImportSidecars(db, dir)
	if err != nil {
		t.Fatalf("ImportSidecars: %v", err)
	}
	if st.Imported != 1 {
		t.Fatalf("Imported = %d, want 1", st.Imported)
	}

	var ids []string
	rows, err := db.Query("SELECT id FROM sessions")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 1 || ids[0] != uuid(1) {
		t.Fatalf("sessions ids = %v, want only %s", ids, uuid(1))
	}
}

// --- Finding 11 + 23: unchanged sidecars must not be rewritten ---

func TestImportSidecars_SkipsUnchangedFiles(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	p := writeSidecar(t, dir, uuid(1)+".json", `{"cumulative":{"cost":1.0}}`)
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(p, stamp, stamp); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	st, err := ImportSidecars(db, dir)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if st.Imported != 1 || st.Unchanged != 0 {
		t.Fatalf("first import stats = %+v, want Imported=1 Unchanged=0", st)
	}

	st, err = ImportSidecars(db, dir)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if st.Imported != 0 || st.Unchanged != 1 {
		t.Fatalf("second import stats = %+v, want Imported=0 Unchanged=1 — unchanged rows are being rewritten into the WAL", st)
	}

	// A real change must still be picked up.
	if err := os.WriteFile(p, []byte(`{"cumulative":{"cost":2.0}}`), 0644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	newStamp := stamp.Add(time.Hour)
	if err := os.Chtimes(p, newStamp, newStamp); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	st, err = ImportSidecars(db, dir)
	if err != nil {
		t.Fatalf("third import: %v", err)
	}
	if st.Imported != 1 {
		t.Fatalf("third import stats = %+v, want Imported=1", st)
	}

	var data string
	if err := db.QueryRow("SELECT data FROM sessions WHERE id = ?", uuid(1)).Scan(&data); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(data, "2.0") {
		t.Fatalf("stored data = %s, want the updated sidecar", data)
	}
}

func TestScanSessionJSONLSummaries_CachesUnchangedFiles(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: unreadable files are still readable")
	}
	claudeDir := t.TempDir()
	projDir := filepath.Join(claudeDir, "projects", "proj")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sid := uuid(7)
	jsonlPath := filepath.Join(projDir, sid+".jsonl")
	if err := os.WriteFile(jsonlPath, []byte(`{"type":"summary","summary":"first pass"}`+"\n"), 0644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	got := scanSessionJSONLSummaries(claudeDir)
	if got[sid] != "first pass" {
		t.Fatalf("first scan = %v, want the summary", got)
	}

	// Make the file unreadable without changing its mtime. A cached scan still
	// returns the summary; a re-read cannot.
	if err := os.Chmod(jsonlPath, 0000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(jsonlPath, 0644) })

	got = scanSessionJSONLSummaries(claudeDir)
	if got[sid] != "first pass" {
		t.Fatalf("second scan = %v — every /api/data request re-reads every .jsonl on disk", got)
	}
}

func TestKVSet_SkipsUnchangedValues(t *testing.T) {
	db := openTestDB(t)
	KVSet(db, "k", `{"a":1}`)
	mustExec(t, db, "UPDATE kv SET updated_at = 'SENTINEL' WHERE key = 'k'")

	KVSet(db, "k", `{"a":1}`) // identical → must not rewrite the row
	var ts string
	if err := db.QueryRow("SELECT updated_at FROM kv WHERE key = 'k'").Scan(&ts); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	if ts != "SENTINEL" {
		t.Fatalf("unchanged KVSet rewrote the row (updated_at=%q)", ts)
	}

	KVSet(db, "k", `{"a":2}`) // changed → must rewrite
	if err := db.QueryRow("SELECT updated_at FROM kv WHERE key = 'k'").Scan(&ts); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	if ts == "SENTINEL" {
		t.Fatal("changed KVSet did not rewrite the row")
	}
	if got := string(KVGet(db, "k")); got != `{"a":2}` {
		t.Fatalf("KVGet = %s, want the new value", got)
	}
}

// --- Finding 10: unbounded history growth ---

func TestCompactHistory_EnforcesRetention(t *testing.T) {
	db := openTestDB(t)
	old := time.Now().UTC().Add(-400 * 24 * time.Hour).Format("2006-01-02T15:04:05Z")
	recent := time.Now().UTC().Add(-2 * time.Hour).Format("2006-01-02T15:04:05Z")

	mustExec(t, db, "INSERT INTO history(ts, data) VALUES(?, ?)", old, `{"cost":1}`)
	mustExec(t, db, "INSERT INTO history(ts, data) VALUES(?, ?)", recent, `{"cost":2}`)
	// A legacy row written by the CURRENT_TIMESTAMP default renders with a
	// space instead of a T (finding 24) and must still be matched.
	mustExec(t, db, "INSERT INTO history(ts, data) VALUES(?, ?)",
		time.Now().UTC().Add(-400*24*time.Hour).Format("2006-01-02 15:04:05"), `{"cost":3}`)

	n, err := CompactHistory(db, HistoryRetention)
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	if n != 2 {
		t.Fatalf("deleted %d rows, want 2", n)
	}
	if got := countRows(t, db, "history"); got != 1 {
		t.Fatalf("history rows = %d, want 1", got)
	}

	var ts string
	db.QueryRow("SELECT ts FROM history").Scan(&ts)
	if ts != recent {
		t.Fatalf("surviving row ts = %q, want the recent row", ts)
	}
}

func TestCompactHistory_KeepsEverythingInsideTheWindow(t *testing.T) {
	db := openTestDB(t)
	// Includes a point 200 days old: within a year, so it must survive.
	for i := 0; i < 10; i++ {
		ts := time.Now().UTC().Add(-time.Duration(i) * 20 * 24 * time.Hour).Format("2006-01-02T15:04:05Z")
		mustExec(t, db, "INSERT INTO history(ts, data) VALUES(?, ?)", ts, fmt.Sprintf(`{"i":%d}`, i))
	}
	n, err := CompactHistory(db, HistoryRetention)
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	if n != 0 {
		t.Fatalf("deleted %d rows inside the retention window, want 0", n)
	}
}

// --- Finding 12: CompactLimitHistory had no rollback path ---

func TestCompactLimitHistory_RollsBackOnDeleteFailure(t *testing.T) {
	db := openTestDB(t)
	dataDir := t.TempDir()

	// 200 dense points inside the 24h–7d band, which compacts to a 5-minute grid.
	base := time.Now().UTC().Add(-48 * time.Hour)
	for i := 0; i < 200; i++ {
		ts := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		mustExec(t, db, "INSERT INTO limit_history(ts, data) VALUES(?, ?)", ts, fmt.Sprintf(`{"ts":%q,"pctWeekly":%d}`, ts, i))
	}
	before := countRows(t, db, "limit_history")

	// Refuse to delete one of the rows compaction wants gone.
	mustExec(t, db, `CREATE TRIGGER block_delete BEFORE DELETE ON limit_history
		WHEN OLD.id = 100 BEGIN SELECT RAISE(ABORT, 'blocked'); END`)

	err := CompactLimitHistory(db, dataDir)
	if err == nil {
		t.Fatal("expected an error when a delete fails")
	}
	if after := countRows(t, db, "limit_history"); after != before {
		t.Fatalf("rows = %d, want %d — a partial compaction was committed", after, before)
	}
}

func TestCompactLimitHistory_PrunesOnSuccess(t *testing.T) {
	db := openTestDB(t)
	dataDir := t.TempDir()

	base := time.Now().UTC().Add(-48 * time.Hour)
	for i := 0; i < 200; i++ {
		ts := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		mustExec(t, db, "INSERT INTO limit_history(ts, data) VALUES(?, ?)", ts, fmt.Sprintf(`{"ts":%q,"pctWeekly":%d}`, ts, i))
	}

	mustFinish(t, 15*time.Second, "CompactLimitHistory", func() {
		if err := CompactLimitHistory(db, dataDir); err != nil {
			t.Errorf("CompactLimitHistory: %v", err)
		}
	})

	after := countRows(t, db, "limit_history")
	if after >= 200 {
		t.Fatalf("rows = %d, want fewer than 200", after)
	}
	if after == 0 {
		t.Fatal("compaction removed everything")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "limit-history.jsonl")); err != nil {
		t.Fatalf("JSONL was not rewritten: %v", err)
	}
}

// --- Finding 19: ImportJSONL discarded stmt.Exec's return values ---

func TestImportJSONL_ReportsWriteFailures(t *testing.T) {
	db := openTestDB(t)
	mustExec(t, db, `CREATE TRIGGER block_insert BEFORE INSERT ON history
		BEGIN SELECT RAISE(ABORT, 'blocked'); END`)

	tmp := filepath.Join(t.TempDir(), "data.jsonl")
	os.WriteFile(tmp, []byte(`{"ts":"2026-01-01T00:00:00Z","cost":1.0}`), 0644)

	if err := ImportJSONL(db, tmp, "history"); err == nil {
		t.Fatal("insert failure was swallowed — ImportJSONL returned nil")
	}
}

// --- Finding 25: ts-less lines were re-inserted on every import forever ---

func TestImportJSONL_TimestamplessLinesAreNotDuplicated(t *testing.T) {
	db := openTestDB(t)
	tmp := filepath.Join(t.TempDir(), "data.jsonl")
	os.WriteFile(tmp, []byte(`{"ts":"2026-01-01T00:00:00Z","cost":1.0}
{"cost":2.0}
{"cost":2.0}`), 0644)

	for i := 0; i < 3; i++ {
		if err := ImportJSONL(db, tmp, "history"); err != nil {
			t.Fatalf("import %d: %v", i, err)
		}
	}

	if n := countRows(t, db, "history"); n != 1 {
		t.Fatalf("history rows = %d, want 1 — a line with no ts is re-inserted on every import", n)
	}
}

// --- Finding 20: a scan error was treated as "no rows" ---

func TestMigrate_ScanErrorDoesNotResetTheSchema(t *testing.T) {
	db := openTestDB(t)
	mustExec(t, db, "UPDATE schema_version SET version = 'corrupt'")

	if err := migrate(db); err == nil {
		t.Fatal("expected migrate to fail on an unreadable schema_version")
	}
	if n := countRows(t, db, "schema_version"); n != 1 {
		t.Fatalf("schema_version rows = %d, want 1 — a second version-0 row was inserted", n)
	}
}

func TestMigrate_IsIdempotent(t *testing.T) {
	db := openTestDB(t)
	if err := migrate(db); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	var v int
	if err := db.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&v); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if v != currentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", v, currentSchemaVersion)
	}
	if n := countRows(t, db, "schema_version"); n != 1 {
		t.Fatalf("schema_version rows = %d, want 1", n)
	}
}

// --- Finding 9: six concurrent read snapshots per dashboard build ---

func TestBuildDashboardData_UsesOneCursorAtATime(t *testing.T) {
	db := openTestDB(t)
	db.SetMaxOpenConns(1)

	mustExec(t, db, "INSERT INTO sessions(id, data, updated_at) VALUES(?, ?, ?)",
		uuid(1), `{"cumulative":{"cost":1.0}}`, time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	mustExec(t, db, "INSERT INTO history(ts, data) VALUES(?, ?)",
		time.Now().UTC().Format("2006-01-02T15:04:05Z"), `{"cost":1}`)
	for i := 0; i < 4; i++ {
		ts := time.Now().UTC().Add(-time.Duration(i) * time.Hour).Format(time.RFC3339)
		mustExec(t, db, "INSERT INTO limit_history(ts, data) VALUES(?, ?)", ts, fmt.Sprintf(`{"ts":%q,"pctWeekly":%d}`, ts, 10+i))
	}

	mustFinish(t, 15*time.Second, "BuildDashboardData", func() {
		if _, err := BuildDashboardData(db, ""); err != nil {
			t.Errorf("BuildDashboardData: %v", err)
		}
	})
}

// --- Finding 24: RFC3339 writes vs a CURRENT_TIMESTAMP default ---

func TestBuildDashboardData_OrdersLegacyTimestampsCorrectly(t *testing.T) {
	db := openTestDB(t)

	// Same day, different times. ' ' sorts before 'T', so without normalising
	// the separator the older row wins a DESC ordering.
	mustExec(t, db, "INSERT INTO sessions(id, data, updated_at) VALUES(?, ?, ?)",
		uuid(1), `{"n":1}`, "2026-01-01T00:00:00Z")
	mustExec(t, db, "INSERT INTO sessions(id, data, updated_at) VALUES(?, ?, ?)",
		uuid(2), `{"n":2}`, "2026-01-01 12:00:00")

	d, err := BuildDashboardData(db, "")
	if err != nil {
		t.Fatalf("BuildDashboardData: %v", err)
	}
	if len(d.Sidecars) != 2 {
		t.Fatalf("sidecars = %d, want 2", len(d.Sidecars))
	}
	if d.Sidecars[0].ID != uuid(2) {
		t.Fatalf("newest sidecar = %s, want %s — a CURRENT_TIMESTAMP row sorted wrongly",
			d.Sidecars[0].ID, uuid(2))
	}
}

func TestKVSet_WritesAConsistentTimestampFormat(t *testing.T) {
	db := openTestDB(t)
	KVSet(db, "k", `{"a":1}`)
	var ts string
	if err := db.QueryRow("SELECT updated_at FROM kv WHERE key = 'k'").Scan(&ts); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := time.Parse(stampLayout, ts); err != nil {
		t.Fatalf("updated_at = %q, want the %s layout: %v", ts, stampLayout, err)
	}
}

// One broken source must not take down the others (finding 5's fix must not
// turn a sidecar failure into a total import outage).
func TestImportFileData_ContinuesAfterSidecarFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: unreadable files are still readable")
	}
	db := openTestDB(t)
	dataDir := t.TempDir()

	p := writeSidecar(t, dataDir, uuid(1)+".json", `{"cumulative":{"cost":1.0}}`)
	if err := os.Chmod(p, 0000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "usage-history.jsonl"),
		[]byte(`{"ts":"2026-01-01T00:00:00Z","cost":1.0}`), 0644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	err := ImportFileData(db, dataDir, t.TempDir())
	if err == nil {
		t.Fatal("expected the sidecar failure to be reported")
	}
	if !strings.Contains(err.Error(), "sidecars") {
		t.Fatalf("error does not name the failing source: %v", err)
	}
	if n := countRows(t, db, "history"); n != 1 {
		t.Fatalf("history rows = %d, want 1 — a sidecar failure stopped the usage history import", n)
	}
}
