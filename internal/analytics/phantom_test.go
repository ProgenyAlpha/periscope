package analytics

import (
	"database/sql"
	"encoding/json"
	"math"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openPhantomDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
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

func seedSession(t *testing.T, db *sql.DB, id string, cost float64, updatedAt time.Time) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{
		"cumulative": map[string]any{"cost": cost},
	})
	_, err := db.Exec(
		`INSERT INTO sessions (id, data, updated_at) VALUES (?, ?, ?)`,
		id, string(data), updatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
}

func seedLimitHistory(t *testing.T, db *sql.DB, ts time.Time, pctWeekly float64) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"pctWeekly": pctWeekly})
	_, err := db.Exec(
		`INSERT INTO limit_history (ts, data) VALUES (?, ?)`,
		ts.UTC().Format(time.RFC3339), string(data),
	)
	if err != nil {
		t.Fatalf("insert limit_history: %v", err)
	}
}

func seedHistory(t *testing.T, db *sql.DB, ts time.Time) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO history (ts, data) VALUES (?, ?)`,
		ts.UTC().Format(time.RFC3339), `{}`,
	)
	if err != nil {
		t.Fatalf("insert history: %v", err)
	}
}

func TestCalcPhantomUsage(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		name     string
		setup    func(t *testing.T, db *sql.DB)
		wantCost float64
		wantInf  bool
	}{
		{
			// Regression: all pctWeekly growth is phantom (cliPct==0).
			// Pre-fix: phantomCost = localTotal * (phantomPct / 0) = +Inf.
			// Post-fix: guard fires, returns fallback with PhantomCost==0.
			name: "cliPct_zero_returns_finite_zero",
			setup: func(t *testing.T, db *sql.DB) {
				seedSession(t, db, "s1", 10.0, now.Add(-24*time.Hour))
				seedLimitHistory(t, db, now.Add(-6*24*time.Hour), 0.0)
				seedLimitHistory(t, db, now.Add(-5*24*time.Hour), 3.0)
				seedLimitHistory(t, db, now.Add(-4*24*time.Hour), 6.0)
				// no history rows → cliPct==0, phantomPct==6.0
			},
			wantCost: 0,
			wantInf:  false,
		},
		{
			// Normal case: some intervals have CLI activity, some do not.
			// cliPct==5.0, phantomPct==2.0, localTotal==10.0
			// phantomCost = 10.0 * (2.0 / 5.0) = 4.0
			name: "mixed_cli_and_phantom",
			setup: func(t *testing.T, db *sql.DB) {
				seedSession(t, db, "s1", 10.0, now.Add(-24*time.Hour))
				t1 := now.Add(-6 * 24 * time.Hour)
				t2 := now.Add(-5 * 24 * time.Hour)
				t3 := now.Add(-4 * 24 * time.Hour)
				seedLimitHistory(t, db, t1, 0.0)
				seedLimitHistory(t, db, t2, 5.0) // delta=5.0
				seedLimitHistory(t, db, t3, 7.0) // delta=2.0
				// CLI activity falls inside the [t1, t2] window → cliPct += 5.0
				seedHistory(t, db, t1.Add(12*time.Hour))
				// no activity in [t2, t3] window → phantomPct += 2.0
			},
			wantCost: 4.0,
			wantInf:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openPhantomDB(t)
			tt.setup(t, db)
			got := CalcPhantomUsage(db)
			if math.IsInf(got.PhantomCost, 0) || math.IsNaN(got.PhantomCost) {
				t.Fatalf("PhantomCost = %v, want finite", got.PhantomCost)
			}
			if got.PhantomCost != tt.wantCost {
				t.Errorf("PhantomCost = %v, want %v", got.PhantomCost, tt.wantCost)
			}
		})
	}
}
