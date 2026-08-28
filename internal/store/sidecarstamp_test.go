package store

import (
	"strings"
	"testing"
)

// The dashboard does not trust the order querySidecars returns. It recomputes
// the newest sidecar itself, in runtime.html's _update:
//
//	for (const s of sidecars) {
//	  const upd = s.updated_at || '';
//	  if (!latest || upd > latestUpd) { latest = s; latestUpd = upd; }
//	}
//
// That is a lexicographic comparison of the updated_at strings exactly as
// shipped, and it decides the model badge, the effort badge, and
// updateStalenessBanner — which tells the user whether their data has stopped
// flowing. It gives the wrong answer the moment two rows disagree on the
// separator, because SQLite's CURRENT_TIMESTAMP default renders a space and
// ' ' sorts before 'T'.
//
// Today it cannot: sessions.updated_at is declared DATETIME, and the sqlite
// driver rewrites a space-separated value into RFC3339 on the way in. That is
// the entire safety margin, it is a driver behaviour rather than anything this
// package states, and history.ts and limit_history.ts — declared TEXT — do
// keep whatever separator they were written with. So the invariant the
// dashboard leans on is pinned here rather than left to a column type nobody
// would think to preserve.
func TestQuerySidecars_UpdatedAtIsNormalisedForClientSideComparison(t *testing.T) {
	db := openTestDB(t)

	// Same three instants, written in the two separators the column has held.
	// The newest is the one with a space, which is the case that breaks.
	seed := []struct{ id, updatedAt string }{
		{"11111111-1111-4111-8111-111111111111", "2026-03-01T08:00:00Z"},
		{"22222222-2222-4222-8222-222222222222", "2026-03-01T09:00:00Z"},
		{"33333333-3333-4333-8333-333333333333", "2026-03-01 10:00:00"},
	}
	for _, s := range seed {
		if _, err := db.Exec(
			"INSERT INTO sessions(id, data, updated_at) VALUES(?, ?, ?)",
			s.id, `{"cumulative":{"cost":1}}`, s.updatedAt); err != nil {
			t.Fatalf("seed %s: %v", s.id, err)
		}
	}

	got, err := querySidecars(db)
	if err != nil {
		t.Fatalf("querySidecars: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d sidecars, want 3", len(got))
	}

	// The server's own ordering is newest-first and already correct.
	const newest = "33333333-3333-4333-8333-333333333333"
	if got[0].ID != newest {
		t.Fatalf("got[0].ID = %q, want %q (server ordering)", got[0].ID, newest)
	}

	// No separator may reach the client that its comparison cannot handle.
	for _, e := range got {
		if strings.Contains(e.UpdatedAt, " ") {
			t.Errorf("sidecar %s updated_at = %q: a space-separated stamp sorts "+
				"before every 'T' stamp in the browser's string comparison",
				e.ID, e.UpdatedAt)
		}
	}

	// Reproduce runtime.html's pick exactly, and require it to agree with the
	// order the server chose.
	var latestID, latestUpd string
	for _, e := range got {
		if latestID == "" || e.UpdatedAt > latestUpd {
			latestID, latestUpd = e.ID, e.UpdatedAt
		}
	}
	if latestID != newest {
		t.Errorf("the dashboard's lexicographic pick chose %q, but the newest "+
			"session is %q; the model badge, the effort badge and the staleness "+
			"banner all follow this pick", latestID, newest)
	}
}
