package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ProgenyAlpha/periscope/internal/store"
)

// --- Bug: /api/plugins/{type} answers `null` for an existing-but-empty dir ---
//
// runtime.html's _loadAllWidgets does
//
//	const files = await resp.json();
//	const htmlFiles = files.filter(f => f.endsWith('.html'));
//
// so a `null` body is a TypeError, swallowed by that function's catch, and the
// dashboard silently boots with ZERO widgets and one console line. The missing
// -directory branch of the same handler already answers `[]`; only the empty
// -directory branch regressed, because `var names []string` stays nil when the
// loop body never runs.
func TestHandlePlugins_EmptyDirectoryIsAnEmptyArrayNotNull(t *testing.T) {
	externalLimiter.reset()
	generalLimiter.reset()
	app := newTestApp(t, "")
	handler := newTestHandler(app)

	for _, typ := range []string{"widgets", "themes", "pricing", "forecasters", "canvas", "vendor"} {
		if err := os.MkdirAll(filepath.Join(app.PluginDir, typ), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", typ, err)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/plugins/"+typ, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", typ, rec.Code)
		}
		// The JS calls .filter() on this directly. `null` is not a value it can
		// survive; `[]` is.
		var names []string
		if err := json.Unmarshal(rec.Body.Bytes(), &names); err != nil {
			t.Fatalf("%s: unmarshal %q: %v", typ, rec.Body.String(), err)
		}
		if names == nil {
			t.Errorf("%s: body = %q, want an empty JSON array; `null` makes "+
				"runtime.html's files.filter() throw and no widget loads at all",
				typ, rec.Body.String())
		}
	}
}

// The missing-directory branch must keep answering [] too — that is the case
// that already worked and the fix must not move it.
func TestHandlePlugins_MissingDirectoryIsStillAnEmptyArray(t *testing.T) {
	externalLimiter.reset()
	generalLimiter.reset()
	app := newTestApp(t, "")
	handler := newTestHandler(app)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/plugins/widgets", nil))
	if got := rec.Body.String(); got != "[]\n" {
		t.Errorf("body = %q, want \"[]\\n\"", got)
	}
}

// --- Bug: the /api/data read path reaches into the real $HOME ---
//
// annotateLiveEffort resolved its directory from os.UserHomeDir() rather than
// from app.HomeDir, which the App struct carries for exactly this purpose. Two
// consequences: an App pointed at some other home still reads (and reports)
// another installation's effort files, and every httptest that touches
// /api/data silently reads the developer's live ~/.periscope/effort — so the
// payload under test is not the payload the test seeded.
func TestHandleData_LiveEffortComesFromTheAppHomeNotTheProcessHome(t *testing.T) {
	externalLimiter.reset()
	generalLimiter.reset()
	app := newTestApp(t, "")
	handler := newTestHandler(app)

	// Nothing under the app's own home: the field must be absent, whatever the
	// machine running the test happens to have in ~/.periscope/effort.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/data", nil))
	var got struct {
		LiveEffort string `json:"live_effort"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.LiveEffort != "" {
		t.Errorf("live_effort = %q with no effort file under app.HomeDir (%s); "+
			"the handler is reading the process home instead",
			got.LiveEffort, app.HomeDir)
	}

	// And when the app's home does have one, it must be the one reported.
	effortDir := filepath.Join(app.HomeDir, "effort")
	if err := os.MkdirAll(effortDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(effortDir, "s1.json"),
		[]byte(`{"level":"medium"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/data", nil))
	if err := json.Unmarshal(rec2.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.LiveEffort != "medium" {
		t.Errorf("live_effort = %q, want %q", got.LiveEffort, "medium")
	}
}

// annotateLiveEffort is also called on the websocket broadcast path, so the
// same home has to reach it there. A direct unit assertion keeps the two
// callers honest.
func TestAnnotateLiveEffort_ReadsTheGivenHome(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "effort"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "effort", "a.json"),
		[]byte(`{"level":"low"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var d store.DashboardData
	annotateLiveEffort(home, &d)
	if d.LiveEffort != "low" {
		t.Errorf("LiveEffort = %q, want %q", d.LiveEffort, "low")
	}

	var empty store.DashboardData
	annotateLiveEffort(t.TempDir(), &empty)
	if empty.LiveEffort != "" {
		t.Errorf("LiveEffort = %q for an empty home, want %q", empty.LiveEffort, "")
	}
}
