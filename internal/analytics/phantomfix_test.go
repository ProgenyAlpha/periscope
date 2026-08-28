package analytics

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// openSerialDB mirrors the production pool: SQLite gets a single connection, so
// any code path that holds one cursor open while opening another deadlocks
// instead of quietly burning a second read snapshot.
func openSerialDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "phantom.db")
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, data TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE history (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE limit_history (id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, data TEXT NOT NULL)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

// Finding 9/22: CalcPhantomUsage opened three cursors and kept all of them open
// until the function returned.
func TestCalcPhantomUsage_UsesOneCursorAtATime(t *testing.T) {
	db := openSerialDB(t)

	now := time.Now().UTC()
	data, _ := json.Marshal(map[string]any{"cumulative": map[string]any{"cost": 12.5}})
	if _, err := db.Exec(`INSERT INTO sessions(id, data, updated_at) VALUES(?, ?, ?)`,
		"11111111-1111-4111-8111-111111111111", string(data), now.Format("2006-01-02T15:04:05Z")); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	for i := 0; i < 6; i++ {
		ts := now.Add(-time.Duration(6-i) * time.Hour)
		payload := fmt.Sprintf(`{"ts":%q,"pctWeekly":%d}`, ts.Format(time.RFC3339), i*3)
		if _, err := db.Exec(`INSERT INTO limit_history(ts, data) VALUES(?, ?)`, ts.Format(time.RFC3339), payload); err != nil {
			t.Fatalf("seed limit_history: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO history(ts, data) VALUES(?, ?)`,
			ts.Format("2006-01-02T15:04:05Z"), `{"cost":1}`); err != nil {
			t.Fatalf("seed history: %v", err)
		}
	}

	done := make(chan *PhantomData, 1)
	go func() { done <- CalcPhantomUsage(db) }()
	select {
	case got := <-done:
		if got == nil {
			t.Fatal("CalcPhantomUsage returned nil")
		}
		if got.LocalSessionTotal != 12.5 {
			t.Fatalf("LocalSessionTotal = %v, want 12.5", got.LocalSessionTotal)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("CalcPhantomUsage deadlocked — it holds several read cursors open at once")
	}
}

// Finding 22: the scan loops ignored rows.Err() and swallowed Scan failures, so
// a truncated read silently understated cost — and the understated number was
// then reported with "estimated" confidence as though it were sound.
func TestCalcPhantomUsage_DoesNotReportATruncatedRead(t *testing.T) {
	db := openSerialDB(t)
	// sessions.data is nullable here so a NULL makes Scan fail mid-loop, which
	// is what a truncated read looks like from the caller's side.
	if _, err := db.Exec(`DROP TABLE sessions`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, data TEXT, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	stamp := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	data, _ := json.Marshal(map[string]any{"cumulative": map[string]any{"cost": 5.0}})
	if _, err := db.Exec(`INSERT INTO sessions(id, data, updated_at) VALUES('s1', ?, ?)`, string(data), stamp); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO sessions(id, data, updated_at) VALUES('s2', NULL, ?)`, stamp); err != nil {
		t.Fatalf("seed null: %v", err)
	}

	got := CalcPhantomUsage(db)
	if got == nil {
		t.Fatal("nil result")
	}
	if got.LocalSessionTotal != 0 || got.Confidence != "none" {
		t.Fatalf("got %+v — a failed read was reported as a real total", got)
	}
}
