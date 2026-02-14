package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// --- ImportJSONL ---

func TestImportJSONL_Valid(t *testing.T) {
	db := openTestDB(t)
	tmp := filepath.Join(t.TempDir(), "data.jsonl")
	lines := `{"ts":"2026-01-01T00:00:00Z","cost":1.0}
{"ts":"2026-01-02T00:00:00Z","cost":2.0}
{"ts":"2026-01-03T00:00:00Z","cost":3.0}`
	os.WriteFile(tmp, []byte(lines), 0644)

	if err := ImportJSONL(db, tmp, "history"); err != nil {
		t.Fatalf("ImportJSONL: %v", err)
	}

	var count int
	db.QueryRow("SELECT COUNT(*) FROM history").Scan(&count)
	if count != 3 {
		t.Fatalf("expected 3 rows, got %d", count)
	}
}

func TestImportJSONL_InvalidTable(t *testing.T) {
	db := openTestDB(t)
	tmp := filepath.Join(t.TempDir(), "data.jsonl")
	os.WriteFile(tmp, []byte(`{"ts":"2026-01-01T00:00:00Z"}`), 0644)

	err := ImportJSONL(db, tmp, "evil_table")
	if err == nil {
		t.Fatal("expected error for invalid table name")
	}
}

func TestImportJSONL_MissingFile(t *testing.T) {
	db := openTestDB(t)
	err := ImportJSONL(db, filepath.Join(t.TempDir(), "nonexistent.jsonl"), "history")
	if err != nil {
		t.Fatalf("expected nil for missing file, got: %v", err)
	}
}

func TestImportJSONL_MalformedLines(t *testing.T) {
	db := openTestDB(t)
	tmp := filepath.Join(t.TempDir(), "data.jsonl")
	lines := `{"ts":"2026-01-01T00:00:00Z","cost":1.0}
not valid json at all
{"ts":"2026-01-02T00:00:00Z","cost":2.0}
{broken
{"ts":"2026-01-03T00:00:00Z","cost":3.0}`
	os.WriteFile(tmp, []byte(lines), 0644)

	if err := ImportJSONL(db, tmp, "history"); err != nil {
		t.Fatalf("ImportJSONL: %v", err)
	}

	var count int
	db.QueryRow("SELECT COUNT(*) FROM history").Scan(&count)
	if count != 3 {
		t.Fatalf("expected 3 valid rows, got %d", count)
	}
}

func TestImportJSONL_DedupByCount(t *testing.T) {
	db := openTestDB(t)
	tmp := filepath.Join(t.TempDir(), "data.jsonl")
	lines := `{"ts":"2026-01-01T00:00:00Z","cost":1.0}
{"ts":"2026-01-02T00:00:00Z","cost":2.0}`
	os.WriteFile(tmp, []byte(lines), 0644)

	ImportJSONL(db, tmp, "history")
	ImportJSONL(db, tmp, "history")

	var count int
	db.QueryRow("SELECT COUNT(*) FROM history").Scan(&count)
	if count != 2 {
		t.Fatalf("expected 2 rows after double import, got %d", count)
	}
}

// --- KVGet / KVSet ---

func TestKV_RoundTrip(t *testing.T) {
	db := openTestDB(t)
	KVSet(db, "test-key", `{"hello":"world"}`)
	got := KVGet(db, "test-key")
	if string(got) != `{"hello":"world"}` {
		t.Fatalf("expected round-trip value, got: %s", got)
	}
}

func TestKV_MissingKey(t *testing.T) {
	db := openTestDB(t)
	got := KVGet(db, "nonexistent")
	if got != nil {
		t.Fatalf("expected nil for missing key, got: %s", got)
	}
}

func TestKV_Overwrite(t *testing.T) {
	db := openTestDB(t)
	KVSet(db, "key", "v1")
	KVSet(db, "key", "v2")
	got := KVGet(db, "key")
	if string(got) != "v2" {
		t.Fatalf("expected v2, got: %s", got)
	}
}

// --- AppendLimitSnapshot ---

func TestAppendLimitSnapshot_FirstInsert(t *testing.T) {
	db := openTestDB(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	os.MkdirAll(dataDir, 0755)

	usage := json.RawMessage(`{"pct5hr":50,"pctWeekly":30,"pctSonnet":20}`)
	AppendLimitSnapshot(db, dataDir, usage)

	var count int
	db.QueryRow("SELECT COUNT(*) FROM limit_history").Scan(&count)
	if count != 1 {
		t.Fatalf("expected 1 entry, got %d", count)
	}
}

func TestAppendLimitSnapshot_TimeDedup(t *testing.T) {
	db := openTestDB(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	os.MkdirAll(dataDir, 0755)

	// Pre-insert an entry with ts=now so the next call deduplicates by time
	now := time.Now().Format(time.RFC3339)
	data := fmt.Sprintf(`{"ts":"%s","pct5hr":50,"pctWeekly":30,"pctSonnet":20}`, now)
	db.Exec("INSERT INTO limit_history(ts, data) VALUES(?, ?)", now, data)

	usage := json.RawMessage(`{"pct5hr":99,"pctWeekly":99,"pctSonnet":99}`)
	AppendLimitSnapshot(db, dataDir, usage)

	var count int
	db.QueryRow("SELECT COUNT(*) FROM limit_history").Scan(&count)
	if count != 1 {
		t.Fatalf("expected 1 (time dedup), got %d", count)
	}
}

func TestAppendLimitSnapshot_ValueDedup(t *testing.T) {
	db := openTestDB(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	os.MkdirAll(dataDir, 0755)

	// Insert entry 3 minutes ago with same pct values
	threeMinAgo := time.Now().Add(-3 * time.Minute).Format(time.RFC3339)
	data := fmt.Sprintf(`{"ts":"%s","pct5hr":50,"pctWeekly":30,"pctSonnet":20}`, threeMinAgo)
	db.Exec("INSERT INTO limit_history(ts, data) VALUES(?, ?)", threeMinAgo, data)

	usage := json.RawMessage(`{"pct5hr":50,"pctWeekly":30,"pctSonnet":20}`)
	AppendLimitSnapshot(db, dataDir, usage)

	var count int
	db.QueryRow("SELECT COUNT(*) FROM limit_history").Scan(&count)
	if count != 1 {
		t.Fatalf("expected 1 (value dedup), got %d", count)
	}
}

func TestAppendLimitSnapshot_DifferentValues(t *testing.T) {
	db := openTestDB(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	os.MkdirAll(dataDir, 0755)

	// Insert entry 3 minutes ago with different pct values
	threeMinAgo := time.Now().Add(-3 * time.Minute).Format(time.RFC3339)
	data := fmt.Sprintf(`{"ts":"%s","pct5hr":10,"pctWeekly":10,"pctSonnet":10}`, threeMinAgo)
	db.Exec("INSERT INTO limit_history(ts, data) VALUES(?, ?)", threeMinAgo, data)

	usage := json.RawMessage(`{"pct5hr":50,"pctWeekly":30,"pctSonnet":20}`)
	AppendLimitSnapshot(db, dataDir, usage)

	var count int
	db.QueryRow("SELECT COUNT(*) FROM limit_history").Scan(&count)
	if count != 2 {
		t.Fatalf("expected 2 (different values), got %d", count)
	}
}

// --- SnapshotSidecarsToHistory ---

func TestSnapshotSidecarsToHistory_InsertsEntry(t *testing.T) {
	db := openTestDB(t)
	sidecar := `{"cumulative":{"cost":1.50,"input":100,"cache_read":50,"cache_write":25,"output":200,"agent_calls":1,"tool_calls":2,"chat_calls":3}}`
	db.Exec("INSERT INTO sessions(id, data, updated_at) VALUES(?, ?, CURRENT_TIMESTAMP)", "sess-0001", sidecar)

	last := make(map[string]float64)
	SnapshotSidecarsToHistory(db, last)

	var count int
	db.QueryRow("SELECT COUNT(*) FROM history").Scan(&count)
	if count != 1 {
		t.Fatalf("expected 1 history entry, got %d", count)
	}
}

func TestSnapshotSidecarsToHistory_CostDedup(t *testing.T) {
	db := openTestDB(t)
	sidecar := `{"cumulative":{"cost":1.50,"input":100,"cache_read":50,"cache_write":25,"output":200,"agent_calls":1,"tool_calls":2,"chat_calls":3}}`
	db.Exec("INSERT INTO sessions(id, data, updated_at) VALUES(?, ?, CURRENT_TIMESTAMP)", "sess-0001", sidecar)

	last := make(map[string]float64)
	SnapshotSidecarsToHistory(db, last)
	SnapshotSidecarsToHistory(db, last) // same cost → should skip

	var count int
	db.QueryRow("SELECT COUNT(*) FROM history").Scan(&count)
	if count != 1 {
		t.Fatalf("expected 1 (cost dedup), got %d", count)
	}
}

func TestSnapshotSidecarsToHistory_NewCost(t *testing.T) {
	db := openTestDB(t)
	sidecar := `{"cumulative":{"cost":1.50,"input":100,"cache_read":50,"cache_write":25,"output":200,"agent_calls":1,"tool_calls":2,"chat_calls":3}}`
	db.Exec("INSERT INTO sessions(id, data, updated_at) VALUES(?, ?, CURRENT_TIMESTAMP)", "sess-0001", sidecar)

	last := make(map[string]float64)
	SnapshotSidecarsToHistory(db, last)

	// Update cost
	sidecar2 := `{"cumulative":{"cost":2.00,"input":200,"cache_read":100,"cache_write":50,"output":400,"agent_calls":2,"tool_calls":4,"chat_calls":6}}`
	db.Exec("UPDATE sessions SET data = ? WHERE id = ?", sidecar2, "sess-0001")
	SnapshotSidecarsToHistory(db, last)

	var count int
	db.QueryRow("SELECT COUNT(*) FROM history").Scan(&count)
	if count != 2 {
		t.Fatalf("expected 2 (new cost), got %d", count)
	}
}

// --- BuildDashboardData ---

func TestBuildDashboardData_EmptyDB(t *testing.T) {
	db := openTestDB(t)
	d, err := BuildDashboardData(db)
	if err != nil {
		t.Fatalf("BuildDashboardData: %v", err)
	}
	if d.Sessions == nil {
		t.Fatal("Sessions should be non-nil empty slice")
	}
	if len(d.Sessions) != 0 {
		t.Fatalf("Sessions should be empty, got %d", len(d.Sessions))
	}
	if d.History == nil {
		t.Fatal("History should be non-nil empty slice")
	}
	if len(d.History) != 0 {
		t.Fatalf("History should be empty, got %d", len(d.History))
	}
	if d.Sidecars == nil {
		t.Fatal("Sidecars should be non-nil empty slice")
	}
	if len(d.Sidecars) != 0 {
		t.Fatalf("Sidecars should be empty, got %d", len(d.Sidecars))
	}
	if d.LimitHistory == nil {
		t.Fatal("LimitHistory should be non-nil empty slice")
	}
	if len(d.LimitHistory) != 0 {
		t.Fatalf("LimitHistory should be empty, got %d", len(d.LimitHistory))
	}
}

func TestBuildDashboardData_WithData(t *testing.T) {
	db := openTestDB(t)

	// Insert a session
	db.Exec("INSERT INTO sessions(id, data, updated_at) VALUES(?, ?, CURRENT_TIMESTAMP)",
		"sess-1", `{"cumulative":{"cost":1.0}}`)

	// Insert history
	db.Exec("INSERT INTO history(ts, data) VALUES(?, ?)",
		"2026-01-01T00:00:00Z", `{"cost":1.0}`)
	db.Exec("INSERT INTO history(ts, data) VALUES(?, ?)",
		"2026-01-02T00:00:00Z", `{"cost":2.0}`)

	// Insert limit history
	db.Exec("INSERT INTO limit_history(ts, data) VALUES(?, ?)",
		"2026-01-01T00:00:00Z", `{"pct5hr":50}`)

	d, err := BuildDashboardData(db)
	if err != nil {
		t.Fatalf("BuildDashboardData: %v", err)
	}

	if len(d.Sidecars) != 1 {
		t.Fatalf("expected 1 sidecar, got %d", len(d.Sidecars))
	}
	if len(d.History) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(d.History))
	}
	if len(d.LimitHistory) != 1 {
		t.Fatalf("expected 1 limit history entry, got %d", len(d.LimitHistory))
	}
}
