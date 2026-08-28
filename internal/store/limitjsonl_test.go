package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// limit-history.jsonl is what internal/forecast reads to draw the burn-rate
// forecast, and what ImportFileData replays into a fresh DB. Writing it into
// ~/.claude/hooks/cost-state — a directory `periscope init` did not create —
// failed with ENOENT and was not even logged: the snapshot landed in the DB
// and vanished from disk.
func TestAppendLimitSnapshot_CreatesMissingDataDir(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "periscope.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	dataDir := filepath.Join(t.TempDir(), "hooks", "cost-state") // absent
	AppendLimitSnapshot(db, dataDir, json.RawMessage(`{"pct5hr":40,"pctWeekly":10}`))

	raw, err := os.ReadFile(filepath.Join(dataDir, "limit-history.jsonl"))
	if err != nil {
		t.Fatalf("snapshot never reached the JSONL: %v", err)
	}
	if !strings.Contains(string(raw), `"pct5hr":40`) {
		t.Errorf("JSONL contents = %q", raw)
	}

	var n int
	if err := db.QueryRow("SELECT count(*) FROM limit_history").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("limit_history rows = %d, want 1", n)
	}
}

// The compaction rewrite used os.Create, which truncates the live file before
// writing a byte. `periscope serve` runs it at startup while the statusline
// process reads the same file, so a reader could legitimately see an empty or
// half-written history.
func TestRewriteLimitHistoryJSONL_NeverExposesATruncatedFile(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "periscope.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const rows = 400
	base := time.Now().UTC().Add(-time.Duration(rows) * time.Minute)
	for i := 0; i < rows; i++ {
		ts := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		data := fmt.Sprintf(`{"ts":%q,"pct5hr":%d,"pad":%q}`, ts, i%100, strings.Repeat("x", 256))
		if _, err := db.Exec("INSERT INTO limit_history(ts, data) VALUES(?, ?)", ts, data); err != nil {
			t.Fatal(err)
		}
	}

	dataDir := t.TempDir()
	jsonlPath := filepath.Join(dataDir, "limit-history.jsonl")
	if err := rewriteLimitHistoryJSONL(db, dataDir); err != nil {
		t.Fatal(err)
	}
	full, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatal(err)
	}
	wantLines := strings.Count(string(full), "\n")

	// A reader polling the file while it is rewritten, exactly as the
	// statusline process does.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var torn string
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			raw, err := os.ReadFile(jsonlPath)
			if err != nil {
				continue
			}
			if got := strings.Count(string(raw), "\n"); got != wantLines {
				mu.Lock()
				if torn == "" {
					torn = fmt.Sprintf("read %d lines mid-rewrite, want %d (%d bytes)", got, wantLines, len(raw))
				}
				mu.Unlock()
				return
			}
		}
	}()

	for i := 0; i < 60; i++ {
		if err := rewriteLimitHistoryJSONL(db, dataDir); err != nil {
			close(stop)
			wg.Wait()
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if torn != "" {
		t.Fatalf("a concurrent reader saw a partial limit history: %s", torn)
	}
}

// The rewrite must also work on a fresh install where the data directory does
// not exist yet.
func TestRewriteLimitHistoryJSONL_CreatesMissingDataDir(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "periscope.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec("INSERT INTO limit_history(ts, data) VALUES(?, ?)", ts, `{"pct5hr":1}`); err != nil {
		t.Fatal(err)
	}

	dataDir := filepath.Join(t.TempDir(), "hooks", "cost-state")
	if err := rewriteLimitHistoryJSONL(db, dataDir); err != nil {
		t.Fatalf("rewrite into a missing data dir: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(dataDir, "limit-history.jsonl")); err != nil {
		t.Fatalf("JSONL not written: %v", err)
	}
}
