package analytics

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// Both queries in phantom.go normalise the separator in SQL —
//
//	WHERE replace(ts, ' ', 'T') >= ?
//
// — precisely so a row that fell back to SQLite's CURRENT_TIMESTAMP default
// ("2026-01-01 00:00:00") is inside the window like any other. The Go side then
// parses only the 'T' forms and drops those rows again.
//
// For loadActiveMinutes that inverts the answer: a minute in which the CLI
// demonstrably ran is not indexed as active, so the rate-limit growth over it
// is attributed to PHANTOM usage — the widget reports spend from Claude.ai or
// some other client that never happened.
//
// store/history.go's parseHistoryTS already treats the space form as one of the
// separators "the ts field has ever carried"; this is the same table.

// seedStampedHistory writes a history row with the timestamp string given
// verbatim, so a test can pin the exact on-disk form.
func seedStampedHistory(t *testing.T, db *sql.DB, ts string) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"ts": ts, "sid": "s1", "cost": 1.0})
	if _, err := db.Exec(`INSERT INTO history (ts, data) VALUES (?, ?)`, ts, string(data)); err != nil {
		t.Fatalf("insert history %q: %v", ts, err)
	}
}

func seedStampedLimit(t *testing.T, db *sql.DB, ts string, pctWeekly float64) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"pctWeekly": pctWeekly})
	if _, err := db.Exec(`INSERT INTO limit_history (ts, data) VALUES (?, ?)`, ts, string(data)); err != nil {
		t.Fatalf("insert limit_history %q: %v", ts, err)
	}
}

// stampForms returns the same instant in every separator the ts column carries.
func stampForms(tm time.Time) (tForm, spaceForm string) {
	u := tm.UTC()
	return u.Format("2006-01-02T15:04:05Z"), u.Format("2006-01-02 15:04:05")
}

// A run of CLI activity must read as CLI activity whichever separator its rows
// were written with.
//
// Every snapshot below sits on a minute the CLI wrote a history row in, so the
// honest answer is "no phantom usage at all". The first block of rows carries
// the 'T' separator and the second carries the space, and the blocks are far
// enough apart that the ±5-minute tolerance cannot let one vouch for the other.
// A parser that only understands 'T' therefore reads the second block as an
// hour of rate-limit growth with nobody at the keyboard.
func TestCalcPhantomUsage_SpaceSeparatedHistoryStillCountsAsCLIActivity(t *testing.T) {
	db := openPhantomDB(t)
	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Minute)

	const localCost = 10.0
	seedSession(t, db, "sess-1", localCost, base)

	// Utilisation climbing by 2% every half hour for six hours.
	const steps = 12
	for i := 0; i <= steps; i++ {
		seedLimitHistory(t, db, base.Add(time.Duration(i*30)*time.Minute), float64(i)*2.0)
	}
	// A CLI history row on every one of those minutes: the first half written
	// with a T, the second half with the space SQLite's CURRENT_TIMESTAMP
	// default produces.
	for i := 0; i <= steps; i++ {
		tForm, spaceForm := stampForms(base.Add(time.Duration(i*30) * time.Minute))
		if i <= 5 {
			seedStampedHistory(t, db, tForm)
		} else {
			seedStampedHistory(t, db, spaceForm)
		}
	}

	got := CalcPhantomUsage(db)
	if got == nil {
		t.Fatal("CalcPhantomUsage returned nil")
	}
	if got.ExtraUsageTotal != 0 {
		t.Errorf("extraUsageTotal = %v%%, want 0: every rate-limit delta sits on a "+
			"minute the CLI wrote a history row in, so none of the growth is phantom",
			got.ExtraUsageTotal)
	}
	if got.PhantomCost != 0 {
		t.Errorf("phantomCost = $%v, want $0: the space-separated half of the "+
			"history was dropped by the timestamp parser, so the CLI's own usage "+
			"was billed to some other client", got.PhantomCost)
	}
	// The local total is read through a different query and must be unaffected.
	if got.LocalSessionTotal != localCost {
		t.Errorf("localSessionTotal = %v, want %v", got.LocalSessionTotal, localCost)
	}
}

// The limit series itself must survive the same separator. A dropped snapshot
// silently shortens the series; drop enough and CalcPhantomUsage falls through
// its len < 2 guard and reports source "none" for an install that has data.
func TestCalcPhantomUsage_SpaceSeparatedLimitSnapshotsAreNotDiscarded(t *testing.T) {
	for _, sep := range []string{"T", "space"} {
		t.Run(sep, func(t *testing.T) {
			db := openPhantomDB(t)
			base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Minute)

			seedSession(t, db, "sess-1", 10.0, base)
			for i := 0; i <= 6; i++ {
				ts := base.Add(time.Duration(i*10) * time.Minute)
				tForm, spaceForm := stampForms(ts)
				if sep == "T" {
					seedStampedLimit(t, db, tForm, float64(i)*2.0)
				} else {
					seedStampedLimit(t, db, spaceForm, float64(i)*2.0)
				}
			}
			// No history at all: all of the growth is genuinely phantom, which
			// is only reportable if the snapshots were read in the first place.
			got := CalcPhantomUsage(db)
			if got == nil {
				t.Fatal("CalcPhantomUsage returned nil")
			}
			if got.Source != "rate_delta" {
				t.Errorf("source = %q, want %q: the snapshots were discarded by the "+
					"timestamp parser, so the walk never ran", got.Source, "rate_delta")
			}
		})
	}
}
