package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ProgenyAlpha/periscope/internal/store"
	"github.com/gorilla/websocket"
)

// Read-path coverage for every GET/read route registered in buildMux.
//
// The write paths have forty tests. These are the reads: the handlers that
// assemble what the dashboard draws, asserted on RESPONSE CONTENT rather than
// on the status line, because a 200 carrying `null` where the JS expects an
// array is exactly the failure this file exists to catch.

// --- helpers ---

func readGET(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	externalLimiter.reset()
	generalLimiter.reset()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// decodeData returns the /api/data body as a generic map, failing the test if
// the document is not complete, valid JSON.
func decodeData(t *testing.T, rec *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/data status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, rec.Body.String())
	}
	return m
}

// seedSidecar writes one session row exactly as ImportSidecars would.
func readSeedSidecar(t *testing.T, app *App, id, data, updatedAt string) {
	t.Helper()
	if _, err := app.DB.Exec(
		"INSERT INTO sessions(id, data, updated_at) VALUES(?, ?, ?)", id, data, updatedAt); err != nil {
		t.Fatalf("seed sidecar %s: %v", id, err)
	}
}

func readSeedHistory(t *testing.T, app *App, ts, data string) {
	t.Helper()
	if _, err := app.DB.Exec("INSERT INTO history(ts, data) VALUES(?, ?)", ts, data); err != nil {
		t.Fatalf("seed history %s: %v", ts, err)
	}
}

func readSeedLimit(t *testing.T, app *App, ts, data string) {
	t.Helper()
	if _, err := app.DB.Exec("INSERT INTO limit_history(ts, data) VALUES(?, ?)", ts, data); err != nil {
		t.Fatalf("seed limit_history %s: %v", ts, err)
	}
}

// --- GET /api/data, fresh install ---

// A brand-new install has an empty database and no cache rows at all. Every
// array the dashboard iterates must still be present and empty. A `null` in
// any of these is a TypeError in the first widget that reaches it, and the
// dashboard renders blank with one console line.
func TestAPIData_EmptyDatabaseHasEveryArrayPresentAndEmpty(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)

	m := decodeData(t, readGET(t, h, "/api/data"))

	// Keys the widgets index into with .length / .filter / for-of.
	for _, key := range []string{"sessions", "history", "sidecars", "limitHistory"} {
		raw, ok := m[key]
		if !ok {
			t.Errorf("key %q missing from the payload entirely", key)
			continue
		}
		if string(raw) == "null" {
			t.Errorf("%q = null; the dashboard iterates this and would throw", key)
			continue
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			t.Errorf("%q is not an array: %s", key, raw)
			continue
		}
		if len(arr) != 0 {
			t.Errorf("%q has %d entries on an empty database, want 0", key, len(arr))
		}
	}

	// generatedAt must be a timestamp the browser's Date() can parse.
	var generatedAt string
	if err := json.Unmarshal(m["generatedAt"], &generatedAt); err != nil {
		t.Fatalf("generatedAt: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, generatedAt); err != nil {
		t.Errorf("generatedAt = %q is not RFC3339: %v", generatedAt, err)
	}

	// The KV-backed objects are absent-as-null by design; the widgets guard
	// them with `|| {}`. Assert that is what they are, so a change to a
	// different empty shape (0, "", []) is caught here rather than in a chart.
	for _, key := range []string{"usageConfig", "statuslineConfig", "liveUsage", "profile", "sessionMeta", "layout"} {
		raw, ok := m[key]
		if !ok {
			t.Errorf("key %q missing from the payload entirely", key)
			continue
		}
		if string(raw) != "null" {
			t.Errorf("%q = %s on an empty database, want null", key, raw)
		}
	}

	// Optional keys must be omitted, not null-valued.
	for _, key := range []string{"teams", "historyHourly", "live_effort"} {
		if raw, ok := m[key]; ok && string(raw) == "null" {
			t.Errorf("%q is present as null; it is omitempty and should be absent", key)
		}
	}

	// phantomUsage is a pointer with omitempty but analytics always returns a
	// value, so it must be an object with the fields the widget reads.
	var phantom map[string]any
	if err := json.Unmarshal(m["phantomUsage"], &phantom); err != nil {
		t.Fatalf("phantomUsage is not an object: %s", m["phantomUsage"])
	}
	for _, f := range []string{"phantomCost", "source"} {
		if _, ok := phantom[f]; !ok {
			t.Errorf("phantomUsage missing %q: %v", f, phantom)
		}
	}
}

// --- GET /api/data, one session and one sidecar ---

// The values seeded must actually come back out. A payload builder that
// returned well-formed empty arrays for a populated database would pass every
// shape assertion above and still be completely broken.
func TestAPIData_SeededSessionAndSidecarAppearInThePayload(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)

	const sid = "aaaaaaaa-1111-4111-8111-111111111111"
	sidecar := `{"session_id":"` + sid + `","cumulative":{"cost":12.34,"input":1000,` +
		`"cache_read":2000,"cache_write":300,"output":400,"agent_calls":2,` +
		`"tool_calls":5,"chat_calls":7},"model":"claude-opus-4-20250514"}`
	readSeedSidecar(t, app, sid, sidecar, "2026-03-01T10:00:00Z")
	readSeedHistory(t, app, "2026-03-01T09:00:00Z",
		`{"sid":"`+sid+`","ts":"2026-03-01T09:00:00Z","cost":1.0,"input":100,"cr":10,"cw":5,"out":50,"turns":1}`)
	readSeedHistory(t, app, "2026-03-01T10:00:00Z",
		`{"sid":"`+sid+`","ts":"2026-03-01T10:00:00Z","cost":12.34,"input":1000,"cr":2000,"cw":300,"out":400,"turns":14}`)
	readSeedLimit(t, app, "2026-03-01T10:00:00Z", `{"ts":"2026-03-01T10:00:00Z","five_hour":{"utilization":42}}`)
	store.KVSet(app.DB, "cache:usage-api", `{"five_hour":{"utilization":42}}`)
	store.KVSet(app.DB, "config:usage", `{"plan":"max20"}`)

	m := decodeData(t, readGET(t, h, "/api/data"))

	// sidecars: the id and the cumulative block the widgets read.
	var sidecars []store.SidecarEntry
	if err := json.Unmarshal(m["sidecars"], &sidecars); err != nil {
		t.Fatalf("sidecars: %v", err)
	}
	if len(sidecars) != 1 {
		t.Fatalf("sidecars = %d entries, want 1", len(sidecars))
	}
	if sidecars[0].ID != sid {
		t.Errorf("sidecar id = %q, want %q", sidecars[0].ID, sid)
	}
	var body struct {
		Cumulative struct {
			Cost      float64 `json:"cost"`
			Input     int     `json:"input"`
			CacheRead int     `json:"cache_read"`
			ToolCalls int     `json:"tool_calls"`
		} `json:"cumulative"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(sidecars[0].Data, &body); err != nil {
		t.Fatalf("sidecar data: %v", err)
	}
	if body.Cumulative.Cost != 12.34 {
		t.Errorf("sidecar cost = %v, want 12.34", body.Cumulative.Cost)
	}
	if body.Cumulative.Input != 1000 || body.Cumulative.CacheRead != 2000 || body.Cumulative.ToolCalls != 5 {
		t.Errorf("sidecar counters = %+v, want input 1000 / cache_read 2000 / tool_calls 5", body.Cumulative)
	}
	if body.Model != "claude-opus-4-20250514" {
		t.Errorf("sidecar model = %q", body.Model)
	}
	if sidecars[0].UpdatedAt != "2026-03-01T10:00:00Z" {
		t.Errorf("sidecar updated_at = %q", sidecars[0].UpdatedAt)
	}

	// history: both rows, oldest first, verbatim.
	var history []map[string]any
	if err := json.Unmarshal(m["history"], &history); err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("history = %d rows, want 2", len(history))
	}
	if history[0]["ts"] != "2026-03-01T09:00:00Z" || history[1]["ts"] != "2026-03-01T10:00:00Z" {
		t.Errorf("history is not oldest-first: %v then %v", history[0]["ts"], history[1]["ts"])
	}
	// cost-overview computes max-min per sid; both endpoints must be intact.
	if history[0]["cost"] != 1.0 || history[1]["cost"] != 12.34 {
		t.Errorf("history costs = %v, %v; want 1 and 12.34", history[0]["cost"], history[1]["cost"])
	}

	var limits []map[string]any
	if err := json.Unmarshal(m["limitHistory"], &limits); err != nil {
		t.Fatalf("limitHistory: %v", err)
	}
	if len(limits) != 1 {
		t.Fatalf("limitHistory = %d rows, want 1", len(limits))
	}

	// The KV caches must reach the payload, not just be readable from the DB.
	if !bytes.Contains(m["liveUsage"], []byte(`"utilization":42`)) {
		t.Errorf("liveUsage = %s, want the seeded cache:usage-api row", m["liveUsage"])
	}
	if !bytes.Contains(m["usageConfig"], []byte(`"plan":"max20"`)) {
		t.Errorf("usageConfig = %s, want the seeded config:usage row", m["usageConfig"])
	}
}

// --- corrupt rows ---

// A poisoned row in ANY of the three JSON-column tables must cost that row
// only. querySidecars already had a regression test; history and
// limit_history go through a different function and had none.
func TestAPIData_CorruptRowInAnyTableCostsOnlyThatRow(t *testing.T) {
	tests := []struct {
		name    string
		seed    func(t *testing.T, app *App)
		key     string
		wantLen int
	}{
		{
			name: "sessions",
			seed: func(t *testing.T, app *App) {
				readSeedSidecar(t, app, "bbbbbbbb-2222-4222-8222-222222222222", `{"cumulative":{"cost":1}}`, "2026-01-01T00:00:00Z")
				readSeedSidecar(t, app, "cccccccc-3333-4333-8333-333333333333", `{"cumulative":`, "2026-01-02T00:00:00Z")
				readSeedSidecar(t, app, "dddddddd-4444-4444-8444-444444444444", "", "2026-01-03T00:00:00Z")
			},
			key: "sidecars", wantLen: 1,
		},
		{
			name: "history",
			seed: func(t *testing.T, app *App) {
				readSeedHistory(t, app, "2026-01-01T00:00:00Z", `{"sid":"s","ts":"2026-01-01T00:00:00Z","cost":1}`)
				readSeedHistory(t, app, "2026-01-02T00:00:00Z", `{"sid":"s","cost":`)
				readSeedHistory(t, app, "2026-01-03T00:00:00Z", "")
				readSeedHistory(t, app, "2026-01-04T00:00:00Z", "not json at all")
			},
			key: "history", wantLen: 1,
		},
		{
			name: "limit_history",
			seed: func(t *testing.T, app *App) {
				readSeedLimit(t, app, "2026-01-01T00:00:00Z", `{"five_hour":{"utilization":1}}`)
				readSeedLimit(t, app, "2026-01-02T00:00:00Z", `{"five_hour":`)
				readSeedLimit(t, app, "2026-01-03T00:00:00Z", "")
			},
			key: "limitHistory", wantLen: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestApp(t, "")
			h := newTestHandler(app)
			tc.seed(t, app)

			rec := readGET(t, h, "/api/data")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (a bad row must not take down the response)", rec.Code)
			}
			m := decodeData(t, rec)

			var arr []json.RawMessage
			if err := json.Unmarshal(m[tc.key], &arr); err != nil {
				t.Fatalf("%s: %v", tc.key, err)
			}
			if len(arr) != tc.wantLen {
				t.Errorf("%s = %d rows, want %d (only the corrupt rows dropped)", tc.key, len(arr), tc.wantLen)
			}
			// And the good row is intact, not a placeholder.
			if len(arr) > 0 && !json.Valid(arr[0]) {
				t.Errorf("surviving row is not valid JSON: %s", arr[0])
			}
		})
	}
}

// A corrupt row in every table at once still leaves a complete, parseable
// document — the failure mode the buffered encode was introduced to close.
func TestAPIData_CorruptRowsInEveryTableStillYieldACompleteDocument(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)
	readSeedSidecar(t, app, "eeeeeeee-5555-4555-8555-555555555555", "", "2026-01-01T00:00:00Z")
	readSeedHistory(t, app, "2026-01-01T00:00:00Z", "")
	readSeedLimit(t, app, "2026-01-01T00:00:00Z", "")

	rec := readGET(t, h, "/api/data")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	m := decodeData(t, rec)
	for _, key := range []string{"sessions", "history", "sidecars", "limitHistory"} {
		if string(m[key]) != "[]" {
			t.Errorf("%s = %s, want []", key, m[key])
		}
	}
	// A truncated body would decode as far as it got; require the last key too.
	if _, ok := m["phantomUsage"]; !ok {
		t.Error("payload is truncated: phantomUsage missing")
	}
}

// --- /api/data history controls ---

func TestAPIData_HistoryModeShapes(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-90 * 24 * time.Hour)

	app := newTestApp(t, "")
	h := newTestHandler(app)
	// Twelve old rows in one hour, which the tiered rollup must thin, plus one
	// recent row it must keep verbatim.
	for i := 0; i < 12; i++ {
		ts := old.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		readSeedHistory(t, app, ts, fmt.Sprintf(`{"sid":"s1","ts":%q,"cost":%d}`, ts, i))
	}
	recent := now.Add(-time.Hour).Format(time.RFC3339)
	readSeedHistory(t, app, recent, fmt.Sprintf(`{"sid":"s1","ts":%q,"cost":99}`, recent))

	full := decodeData(t, readGET(t, h, "/api/data"))
	var fullRows []json.RawMessage
	json.Unmarshal(full["history"], &fullRows)
	if len(fullRows) != 13 {
		t.Fatalf("default history = %d rows, want all 13", len(fullRows))
	}
	if _, ok := full["historyHourly"]; ok {
		t.Error("historyHourly present on an unthinned response; it is only for rollups")
	}

	roll := decodeData(t, readGET(t, h, "/api/data?history=rollup"))
	var rollRows []json.RawMessage
	json.Unmarshal(roll["history"], &rollRows)
	if len(rollRows) >= 13 {
		t.Errorf("rollup history = %d rows, want fewer than 13", len(rollRows))
	}
	if len(rollRows) == 0 {
		t.Error("rollup history is empty; the recent row must survive verbatim")
	}
	// The exact hourly histogram travels alongside, because a rollup cannot
	// preserve a row COUNT and activity-breakdown's peak hour is a count.
	var hourly store.HistoryHourly
	if err := json.Unmarshal(roll["historyHourly"], &hourly); err != nil {
		t.Fatalf("historyHourly missing or malformed on a rollup: %s", roll["historyHourly"])
	}
	total := 0
	for _, c := range hourly.Counts {
		total += c
	}
	if total != 13 {
		t.Errorf("historyHourly counts sum to %d, want the full 13 rows", total)
	}

	none := decodeData(t, readGET(t, h, "/api/data?history=none"))
	if string(none["history"]) != "[]" {
		t.Errorf("history=none gave %s, want [] (the key must stay)", none["history"])
	}
	// The rest of the payload is unaffected.
	if _, ok := none["sidecars"]; !ok {
		t.Error("history=none dropped sidecars too")
	}
}

func TestAPIData_RejectsBadHistoryControls(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)

	for _, tc := range []struct{ name, query, wantMsg string }{
		{"unknown mode", "?history=sideways", "history must be one of"},
		{"unparseable from", "?from=yesterday", "from:"},
		{"unparseable to", "?from=2026-01-01&to=lunchtime", "to:"},
		{"inverted window", "?from=2026-02-01&to=2026-01-01", "to must be after from"},
		{"equal window", "?from=2026-01-01&to=2026-01-01", "to must be after from"},
		{"sub-minute bucket", "?bucket=10s", "at least"},
		{"non-duration bucket", "?bucket=banana", "not a duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := readGET(t, h, "/api/data"+tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			var e struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("error body is not JSON: %s", rec.Body.String())
			}
			if !strings.Contains(e.Error, tc.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", e.Error, tc.wantMsg)
			}
		})
	}
}

// --- GET /api/history ---

func TestAPIHistory_WindowResponseContent(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)

	base := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		ts := base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)
		readSeedHistory(t, app, ts, fmt.Sprintf(`{"sid":"s1","ts":%q,"cost":%d}`, ts, i))
	}
	// One row outside the window, which must not be returned.
	outside := base.Add(-48 * time.Hour).Format(time.RFC3339)
	readSeedHistory(t, app, outside, fmt.Sprintf(`{"sid":"s1","ts":%q,"cost":999}`, outside))

	from := base.Add(-time.Minute).Format(time.RFC3339)
	to := base.Add(6 * time.Hour).Format(time.RFC3339)
	rec := readGET(t, h, "/api/history?from="+from+"&to="+to)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		From      string            `json:"from"`
		To        string            `json:"to"`
		Bucket    string            `json:"bucket"`
		Rows      int               `json:"rows"`
		Truncated bool              `json:"truncated"`
		History   []json.RawMessage `json:"history"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, rec.Body.String())
	}
	if len(resp.History) != 6 {
		t.Errorf("history = %d rows, want the 6 inside the window", len(resp.History))
	}
	// rows must describe the array actually shipped; a mismatch is how a
	// truncation would go unnoticed.
	if resp.Rows != len(resp.History) {
		t.Errorf("rows = %d but history has %d entries", resp.Rows, len(resp.History))
	}
	if resp.Truncated {
		t.Error("truncated = true for 6 rows")
	}
	if resp.Bucket != "0s" {
		t.Errorf("bucket = %q, want %q for an unbucketed request", resp.Bucket, "0s")
	}
	// The echoed window is what the caller asked for, so a client can tell a
	// clamped request from an honoured one.
	if gotFrom, err := time.Parse(time.RFC3339, resp.From); err != nil {
		t.Errorf("from = %q is not RFC3339", resp.From)
	} else if !gotFrom.Equal(base.Add(-time.Minute)) {
		t.Errorf("from echoed as %v, want %v", gotFrom, base.Add(-time.Minute))
	}
	if gotTo, err := time.Parse(time.RFC3339, resp.To); err != nil {
		t.Errorf("to = %q is not RFC3339", resp.To)
	} else if !gotTo.Equal(base.Add(6 * time.Hour)) {
		t.Errorf("to echoed as %v, want %v", gotTo, base.Add(6*time.Hour))
	}
	// The out-of-window row really is gone, not merely counted out.
	if bytes.Contains(rec.Body.Bytes(), []byte(`"cost":999`)) {
		t.Error("a row outside the window was returned")
	}
}

func TestAPIHistory_BucketDownsamplesAndIsReported(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)

	base := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ { // one row a minute for an hour
		ts := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		readSeedHistory(t, app, ts, fmt.Sprintf(`{"sid":"s1","ts":%q,"cost":%d}`, ts, i))
	}

	from := base.Add(-time.Minute).Format(time.RFC3339)
	to := base.Add(2 * time.Hour).Format(time.RFC3339)
	rec := readGET(t, h, "/api/history?from="+from+"&to="+to+"&bucket=1h")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Bucket  string            `json:"bucket"`
		Rows    int               `json:"rows"`
		History []json.RawMessage `json:"history"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Bucket != "1h0m0s" {
		t.Errorf("bucket = %q, want %q", resp.Bucket, "1h0m0s")
	}
	if len(resp.History) >= 60 {
		t.Errorf("bucketed history = %d rows, want fewer than the 60 seeded", len(resp.History))
	}
	if resp.Rows != len(resp.History) {
		t.Errorf("rows = %d but history has %d", resp.Rows, len(resp.History))
	}
	// The rows that survive are the ORIGINALS, so the first and last minute of
	// the hour must both still be there — that is what makes a windowed delta
	// exact rather than approximate.
	body := rec.Body.String()
	if !strings.Contains(body, `"cost":0`) {
		t.Error("the bucket's first row was dropped; a delta computed from this is wrong")
	}
	if !strings.Contains(body, `"cost":59`) {
		t.Error("the bucket's last row was dropped; a delta computed from this is wrong")
	}
}

func TestAPIHistory_EmptyWindowIsAnEmptyArrayNotNull(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)

	rec := readGET(t, h, "/api/history?from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	json.Unmarshal(rec.Body.Bytes(), &raw)
	if string(raw["history"]) != "[]" {
		t.Errorf("history = %s for an empty window, want []", raw["history"])
	}
	if string(raw["rows"]) != "0" {
		t.Errorf("rows = %s, want 0", raw["rows"])
	}
}

func TestAPIHistory_ValidationRejections(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)

	for _, tc := range []struct{ name, query, wantMsg string }{
		{"no from", "", "from is required"},
		{"empty from", "?from=", "from is required"},
		{"unparseable from", "?from=whenever", "from:"},
		{"unparseable to", "?from=2026-01-01&to=whenever", "to:"},
		{"inverted", "?from=2026-02-01T00:00:00Z&to=2026-01-01T00:00:00Z", "to must be after from"},
		{"equal", "?from=2026-01-01T00:00:00Z&to=2026-01-01T00:00:00Z", "to must be after from"},
		{"sub-minute bucket", "?from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z&bucket=1s", "at least"},
		{"negative bucket", "?from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z&bucket=-1h", "negative"},
		{"nonsense bucket", "?from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z&bucket=soon", "not a duration"},
		// The whole point of the endpoint's window being mandatory: a caller
		// cannot use it as a second way to ask for the entire table.
		{"unbucketed year", "?from=2025-01-01T00:00:00Z&to=2025-12-01T00:00:00Z", "full-resolution limit"},
		{"beyond retention", "?from=2000-01-01T00:00:00Z&to=2026-01-01T00:00:00Z&bucket=24h", "retention window"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := readGET(t, h, "/api/history"+tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			var e struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("error body is not JSON: %s", rec.Body.String())
			}
			if !strings.Contains(e.Error, tc.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", e.Error, tc.wantMsg)
			}
		})
	}
}

// A wide span IS allowed once the caller says what bucket it wants — the
// rejection above must be about resolution, not about reach.
func TestAPIHistory_WideSpanWithBucketIsServed(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)
	readSeedHistory(t, app, "2026-01-15T00:00:00Z", `{"sid":"s","ts":"2026-01-15T00:00:00Z","cost":1}`)

	rec := readGET(t, h, "/api/history?from=2026-01-01T00:00:00Z&to=2026-06-01T00:00:00Z&bucket=24h")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		History []json.RawMessage `json:"history"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.History) != 1 {
		t.Errorf("history = %d rows, want the 1 seeded", len(resp.History))
	}
}

func TestAPIHistory_AcceptsEveryTimestampFormTheBrowserProduces(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)

	base := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	readSeedHistory(t, app, base.Format(time.RFC3339),
		fmt.Sprintf(`{"sid":"s","ts":%q,"cost":7}`, base.Format(time.RFC3339)))

	from := base.Add(-time.Hour)
	to := base.Add(time.Hour)
	for _, tc := range []struct{ name, q string }{
		{"RFC3339", "from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339)},
		{"RFC3339Nano (Date#toISOString)", "from=" + from.Format(time.RFC3339Nano) + "&to=" + to.Format(time.RFC3339Nano)},
		{"epoch seconds", fmt.Sprintf("from=%d&to=%d", from.Unix(), to.Unix())},
		{"epoch millis (Date#getTime)", fmt.Sprintf("from=%d&to=%d", from.UnixMilli(), to.UnixMilli())},
		{"bare date", "from=2026-03-10&to=2026-03-11"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := readGET(t, h, "/api/history?"+tc.q)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"cost":7`) {
				t.Errorf("the seeded row was not returned: %s", rec.Body.String())
			}
		})
	}
}

func TestAPIHistory_RejectsNonGET(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		externalLimiter.reset()
		generalLimiter.reset()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/api/history?from=2026-01-01T00:00:00Z", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
	}
}

// --- gzip vs identity ---

// Both encodings must decode to the SAME bytes. A compressed response that
// differs from the identity one is a payload the dashboard renders differently
// depending on what its browser negotiated.
func TestReadEndpoints_GzipDecodesToTheIdenticalBytes(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)

	base := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 400; i++ { // enough to clear gzipMinSize
		ts := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		readSeedHistory(t, app, ts, fmt.Sprintf(
			`{"sid":"session-%d","ts":%q,"cost":%d.5,"input":%d,"cr":%d,"cw":%d,"out":%d,"turns":%d}`,
			i%4, ts, i, i*10, i*20, i*3, i*7, i))
	}

	for _, path := range []string{
		"/api/data",
		"/api/data?history=rollup",
		"/api/history?from=2026-03-10T00:00:00Z&to=2026-03-10T23:00:00Z",
	} {
		t.Run(path, func(t *testing.T) {
			externalLimiter.reset()
			generalLimiter.reset()
			plainRec := httptest.NewRecorder()
			h.ServeHTTP(plainRec, httptest.NewRequest(http.MethodGet, path, nil))

			externalLimiter.reset()
			generalLimiter.reset()
			gzReq := httptest.NewRequest(http.MethodGet, path, nil)
			gzReq.Header.Set("Accept-Encoding", "gzip")
			gzRec := httptest.NewRecorder()
			h.ServeHTTP(gzRec, gzReq)

			if plainRec.Code != http.StatusOK || gzRec.Code != http.StatusOK {
				t.Fatalf("status: identity %d, gzip %d", plainRec.Code, gzRec.Code)
			}
			if enc := gzRec.Header().Get("Content-Encoding"); enc != "gzip" {
				t.Fatalf("Content-Encoding = %q, want gzip (payload too small to exercise this?)", enc)
			}
			if enc := plainRec.Header().Get("Content-Encoding"); enc != "" {
				t.Errorf("identity response carries Content-Encoding %q", enc)
			}
			// A cache must know this URL varies, even from the identity reply.
			for name, rec := range map[string]*httptest.ResponseRecorder{"identity": plainRec, "gzip": gzRec} {
				if v := rec.Header().Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
					t.Errorf("%s: Vary = %q, want it to include Accept-Encoding", name, v)
				}
			}

			zr, err := gzip.NewReader(bytes.NewReader(gzRec.Body.Bytes()))
			if err != nil {
				t.Fatalf("gzip.NewReader: %v", err)
			}
			decoded, err := io.ReadAll(zr)
			if err != nil {
				t.Fatalf("gunzip: %v", err)
			}
			if err := zr.Close(); err != nil {
				t.Fatalf("gzip stream is not complete: %v", err)
			}

			// generatedAt differs between the two calls by construction, so
			// compare everything else field by field.
			var a, b map[string]json.RawMessage
			if err := json.Unmarshal(plainRec.Body.Bytes(), &a); err != nil {
				t.Fatalf("identity body: %v", err)
			}
			if err := json.Unmarshal(decoded, &b); err != nil {
				t.Fatalf("decompressed body: %v", err)
			}
			delete(a, "generatedAt")
			delete(b, "generatedAt")
			if len(a) != len(b) {
				t.Fatalf("key counts differ: identity %d, gzip %d", len(a), len(b))
			}
			for k, av := range a {
				bv, ok := b[k]
				if !ok {
					t.Errorf("key %q missing from the decompressed body", k)
					continue
				}
				if !bytes.Equal(av, bv) {
					t.Errorf("key %q differs between encodings", k)
				}
			}
			// Content-Length must describe the compressed bytes actually sent.
			if cl := gzRec.Header().Get("Content-Length"); cl != fmt.Sprint(gzRec.Body.Len()) {
				t.Errorf("Content-Length = %q but %d bytes were written", cl, gzRec.Body.Len())
			}
		})
	}
}

// --- websocket: rollup vs full ---

// Two clients connected at once must each get the shape they asked for, from
// the SAME broadcast, and both must decode.
func TestWS_RollupAndFullClientsGetTheirOwnShapeFromOneBroadcast(t *testing.T) {
	app := newTestApp(t, "")
	srv := httptest.NewServer(newTestHandler(app))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	dial := func(query string) *websocket.Conn {
		t.Helper()
		c, _, err := websocket.DefaultDialer.Dial(wsURL+"/ws"+query, nil)
		if err != nil {
			t.Fatalf("dial %q: %v", query, err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	fullConn := dial("")
	rollConn := dial("?history=rollup")

	// Let both registrations land before broadcasting.
	deadline := time.Now().Add(2 * time.Second)
	for app.Hub.clientCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if app.Hub.clientCount() != 2 {
		t.Fatalf("clientCount = %d, want 2", app.Hub.clientCount())
	}

	now := time.Now().UTC()
	old := now.Add(-90 * 24 * time.Hour)
	data := &store.DashboardData{
		Sessions:     []any{},
		Sidecars:     []store.SidecarEntry{},
		LimitHistory: []json.RawMessage{},
	}
	for i := 0; i < 20; i++ {
		ts := old.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		data.History = append(data.History,
			json.RawMessage(fmt.Sprintf(`{"sid":"s1","ts":%q,"cost":%d}`, ts, i)))
	}
	broadcastDashboardData(app, data)

	read := func(c *websocket.Conn, name string) map[string]json.RawMessage {
		t.Helper()
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, raw, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("%s read: %v", name, err)
		}
		var env struct {
			Type    string                     `json:"type"`
			Payload map[string]json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("%s envelope: %v\n%s", name, err, raw)
		}
		if env.Type != "data" {
			t.Fatalf("%s type = %q, want data", name, env.Type)
		}
		return env.Payload
	}

	fullPayload := read(fullConn, "full")
	rollPayload := read(rollConn, "rollup")

	var fullRows, rollRows []json.RawMessage
	if err := json.Unmarshal(fullPayload["history"], &fullRows); err != nil {
		t.Fatalf("full history: %v", err)
	}
	if err := json.Unmarshal(rollPayload["history"], &rollRows); err != nil {
		t.Fatalf("rollup history: %v", err)
	}
	if len(fullRows) != 20 {
		t.Errorf("full client got %d rows, want all 20", len(fullRows))
	}
	if len(rollRows) >= 20 {
		t.Errorf("rollup client got %d rows, want fewer than 20", len(rollRows))
	}
	if len(rollRows) == 0 {
		t.Error("rollup client got no history at all")
	}

	// Only the thinned frame carries the exact hourly counts, and they must
	// still describe every original row.
	if _, ok := fullPayload["historyHourly"]; ok {
		t.Error("the full frame carries historyHourly; it summarises rows it already has")
	}
	var hourly store.HistoryHourly
	if err := json.Unmarshal(rollPayload["historyHourly"], &hourly); err != nil {
		t.Fatalf("rollup frame is missing historyHourly: %s", rollPayload["historyHourly"])
	}
	total := 0
	for _, c := range hourly.Counts {
		total += c
	}
	if total != 20 {
		t.Errorf("historyHourly counts sum to %d, want 20", total)
	}

	// Everything outside history is identical between the two shapes.
	for _, key := range []string{"sessions", "sidecars", "limitHistory", "generatedAt"} {
		if !bytes.Equal(fullPayload[key], rollPayload[key]) {
			t.Errorf("%q differs between the full and rollup frames: %s vs %s",
				key, fullPayload[key], rollPayload[key])
		}
	}
}

// A client that asks for a shape nobody implements gets the historical one,
// never an error or an empty array.
func TestWS_UnknownHistoryParamServesTheFullShape(t *testing.T) {
	app := newTestApp(t, "")
	srv := httptest.NewServer(newTestHandler(app))
	defer srv.Close()

	c, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/ws?history=sideways", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	deadline := time.Now().Add(2 * time.Second)
	for app.Hub.clientCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if app.Hub.wantsRollup() {
		t.Fatal("an unrecognised history param opted the client into the rollup")
	}

	now := time.Now().UTC().Add(-90 * 24 * time.Hour)
	data := &store.DashboardData{}
	for i := 0; i < 20; i++ {
		ts := now.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		data.History = append(data.History,
			json.RawMessage(fmt.Sprintf(`{"sid":"s1","ts":%q,"cost":%d}`, ts, i)))
	}
	broadcastDashboardData(app, data)

	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var env struct {
		Payload struct {
			History []json.RawMessage `json:"history"`
		} `json:"payload"`
	}
	json.Unmarshal(raw, &env)
	if len(env.Payload.History) != 20 {
		t.Errorf("history = %d rows, want all 20", len(env.Payload.History))
	}
}

// --- the remaining read routes ---

func TestAPIHealth_ReportsTheClientCount(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)

	rec := readGET(t, h, "/api/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	var got struct {
		OK      bool `json:"ok"`
		Clients *int `json:"clients"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.OK {
		t.Error("ok = false")
	}
	if got.Clients == nil {
		t.Fatal("clients missing")
	}
	if *got.Clients != 0 {
		t.Errorf("clients = %d with nothing connected, want 0", *got.Clients)
	}
}

func TestAPILayout_GETRoundTripsWhatWasSaved(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)

	// Absent: `null`, which is what runtime.html's `|| {}` expects.
	rec := readGET(t, h, "/api/layout")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "null" {
		t.Errorf("body = %q with no saved layout, want null", got)
	}

	const layout = `{"cost-overview":{"x":0,"y":0,"w":12,"h":4}}`
	store.KVSet(app.DB, "config:layout", layout)

	rec2 := readGET(t, h, "/api/layout")
	var got map[string]struct{ X, Y, W, H int }
	if err := json.Unmarshal(rec2.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, rec2.Body.String())
	}
	if got["cost-overview"].W != 12 || got["cost-overview"].H != 4 {
		t.Errorf("layout = %+v, want w=12 h=4", got["cost-overview"])
	}

	// The same value must also reach /api/data, which is where the dashboard
	// actually reads it on boot.
	m := decodeData(t, readGET(t, h, "/api/data"))
	if !bytes.Contains(m["layout"], []byte(`"w":12`)) {
		t.Errorf("/api/data layout = %s, want the saved layout", m["layout"])
	}
}

func TestAPIStatusline_GETReportsWhetherTheHookIsInstalled(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)
	settingsPath := filepath.Join(app.ClaudeDir, "settings.json")

	// Missing settings.json: not enabled, and not a 500.
	rec := readGET(t, h, "/api/statusline")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d for a missing settings.json, want 200", rec.Code)
	}
	var got struct {
		Enabled bool `json:"enabled"`
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Enabled {
		t.Error("enabled = true with no settings.json")
	}

	// Unparseable settings.json: still not a 500.
	os.WriteFile(settingsPath, []byte("{ this is not json"), 0o644)
	rec2 := readGET(t, h, "/api/statusline")
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d for an unparseable settings.json, want 200", rec2.Code)
	}
	json.Unmarshal(rec2.Body.Bytes(), &got)
	if got.Enabled {
		t.Error("enabled = true for an unparseable settings.json")
	}

	// Present: enabled.
	os.WriteFile(settingsPath, []byte(`{"statusLine":{"type":"command","command":"periscope statusline"}}`), 0o644)
	rec3 := readGET(t, h, "/api/statusline")
	json.Unmarshal(rec3.Body.Bytes(), &got)
	if !got.Enabled {
		t.Errorf("enabled = false with statusLine configured; body = %s", rec3.Body.String())
	}

	// Present but unrelated keys: not enabled.
	os.WriteFile(settingsPath, []byte(`{"hooks":{}}`), 0o644)
	rec4 := readGET(t, h, "/api/statusline")
	json.Unmarshal(rec4.Body.Bytes(), &got)
	if got.Enabled {
		t.Error("enabled = true for a settings.json with no statusLine key")
	}
}

func TestAPIPricing_ServesTheCacheWithoutReachingTheNetwork(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)

	// A fresh cache file is what keeps this test hermetic: FetchLiteLLMPricing
	// returns it untouched when it is under 24h old and never opens a socket.
	cache := fmt.Sprintf(`{"fetched_at":%d,"data":{"claude-opus-4-20250514":{"input":15,"output":75}}}`,
		time.Now().Unix())
	if err := os.WriteFile(filepath.Join(app.DataDir, "litellm-pricing-cache.json"), []byte(cache), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	rec := readGET(t, h, "/api/pricing")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	var models map[string]map[string]float64
	if err := json.Unmarshal(rec.Body.Bytes(), &models); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, rec.Body.String())
	}
	m, ok := models["claude-opus-4-20250514"]
	if !ok {
		t.Fatalf("the cached model is missing: %v", models)
	}
	if m["input"] != 15 || m["output"] != 75 {
		t.Errorf("prices = %v, want input 15 / output 75", m)
	}
}

func TestAPIPlugins_ListsAndServesFiles(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)

	widgets := filepath.Join(app.PluginDir, "widgets")
	os.MkdirAll(filepath.Join(widgets, "nested"), 0o755)
	os.WriteFile(filepath.Join(widgets, "cost-overview.html"),
		[]byte("<style>.a{}</style><script>Periscope.registerWidget({id:'cost-overview'})</script>"), 0o644)
	os.WriteFile(filepath.Join(widgets, "tool-usage.html"), []byte("<script>x</script>"), 0o644)

	rec := readGET(t, h, "/api/plugins/widgets")
	var names []string
	if err := json.Unmarshal(rec.Body.Bytes(), &names); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, rec.Body.String())
	}
	if len(names) != 2 {
		t.Errorf("names = %v, want the 2 files and not the subdirectory", names)
	}
	for _, want := range []string{"cost-overview.html", "tool-usage.html"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q missing from %v", want, names)
		}
	}

	// Fetching one back gives the file, with the type the runtime's new
	// Function() path needs the browser not to sniff wrongly.
	rec2 := readGET(t, h, "/api/plugins/widgets/cost-overview.html")
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d", rec2.Code)
	}
	if ct := rec2.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec2.Body.String(), "registerWidget") {
		t.Errorf("body = %q, want the widget source", rec2.Body.String())
	}

	// Content types the dashboard depends on for its other plugin kinds.
	os.MkdirAll(filepath.Join(app.PluginDir, "themes"), 0o755)
	os.WriteFile(filepath.Join(app.PluginDir, "themes", "dark.toml"), []byte("name='dark'"), 0o644)
	os.MkdirAll(filepath.Join(app.PluginDir, "vendor"), 0o755)
	os.WriteFile(filepath.Join(app.PluginDir, "vendor", "grid.js"), []byte("void 0;"), 0o644)
	os.WriteFile(filepath.Join(app.PluginDir, "vendor", "grid.css"), []byte(".g{}"), 0o644)
	for _, tc := range []struct{ path, wantType string }{
		{"/api/plugins/themes/dark.toml", "application/toml"},
		{"/api/plugins/vendor/grid.js", "application/javascript"},
		{"/api/plugins/vendor/grid.css", "text/css"},
	} {
		r := readGET(t, h, tc.path)
		if r.Code != http.StatusOK {
			t.Errorf("%s: status = %d", tc.path, r.Code)
			continue
		}
		if ct := r.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.wantType) {
			t.Errorf("%s: Content-Type = %q, want %q", tc.path, ct, tc.wantType)
		}
	}

	// Unknown type, missing file, a directory asked for as a file.
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/api/plugins/secrets", http.StatusNotFound},
		{"/api/plugins/widgets/nope.html", http.StatusNotFound},
		{"/api/plugins/widgets/nested", http.StatusNotFound},
	} {
		r := readGET(t, h, tc.path)
		if r.Code != tc.want {
			t.Errorf("%s: status = %d, want %d (body %q)", tc.path, r.Code, tc.want, r.Body.String())
		}
	}
}

// A traversing plugin name must never yield a file outside the plugin
// directory. The request is handed to handlePlugins directly because
// http.ServeMux rewrites a dotted path with a 301 before any handler sees it,
// which would make a mux-level test prove only that ServeMux cleans paths.
func TestHandlePlugins_TraversingNameCannotEscapeThePluginDir(t *testing.T) {
	app := newTestApp(t, "")
	os.MkdirAll(filepath.Join(app.PluginDir, "widgets"), 0o755)
	secret := filepath.Join(app.PluginDir, "runtime.html")
	if err := os.WriteFile(secret, []byte("SECRET-SHELL"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	outside := filepath.Join(filepath.Dir(app.PluginDir), "outside.txt")
	if err := os.WriteFile(outside, []byte("SECRET-OUTSIDE"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, name := range []string{
		"../runtime.html",
		"../../outside.txt",
		"..%2Fruntime.html",
		"/etc/passwd",
		"....//runtime.html",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/plugins/widgets/x", nil)
		// Set the path after construction so it reaches the handler uncleaned.
		req.URL.Path = "/api/plugins/widgets/" + name
		handlePlugins(app, rec, req)

		body := rec.Body.String()
		for _, leak := range []string{"SECRET-SHELL", "SECRET-OUTSIDE", "root:"} {
			if strings.Contains(body, leak) {
				t.Errorf("%q leaked %q (status %d)", name, leak, rec.Code)
			}
		}
		if rec.Code == http.StatusOK {
			t.Errorf("%q returned 200; body = %q", name, body)
		}
	}
}

func TestDashboardRoot_ServesRuntimeOr404(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)

	// Not yet extracted: an actionable 404, not a 200 with an empty page.
	rec := readGET(t, h, "/")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d with no runtime.html, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "periscope init") {
		t.Errorf("the 404 does not say how to fix it: %q", rec.Body.String())
	}

	const shell = "<!doctype html><title>Periscope</title><div id=\"widget-grid\"></div>"
	if err := os.WriteFile(filepath.Join(app.PluginDir, "runtime.html"), []byte(shell), 0o644); err != nil {
		t.Fatalf("write runtime: %v", err)
	}
	rec2 := readGET(t, h, "/")
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec2.Code)
	}
	if ct := rec2.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if rec2.Body.String() != shell {
		t.Errorf("body = %q, want the runtime shell verbatim", rec2.Body.String())
	}

	// Anything else under / is a 404, not the dashboard.
	if r := readGET(t, h, "/not-a-route"); r.Code != http.StatusNotFound {
		t.Errorf("/not-a-route: status = %d, want 404", r.Code)
	}
}

func TestStaticRoutes_ServeThePWAFiles(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)

	staticDir := filepath.Join(app.PluginDir, "static")
	os.MkdirAll(staticDir, 0o755)
	os.WriteFile(filepath.Join(staticDir, "manifest.json"), []byte(`{"name":"Periscope"}`), 0o644)
	os.WriteFile(filepath.Join(staticDir, "sw.js"), []byte("self.addEventListener('fetch',()=>{});"), 0o644)
	os.WriteFile(filepath.Join(staticDir, "icon.png"), []byte("\x89PNG\r\n\x1a\n"), 0o644)

	rec := readGET(t, h, "/manifest.json")
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest: status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/manifest+json") {
		t.Errorf("manifest Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "Periscope") {
		t.Errorf("manifest body = %q", rec.Body.String())
	}

	rec2 := readGET(t, h, "/sw.js")
	if rec2.Code != http.StatusOK {
		t.Fatalf("sw.js: status = %d", rec2.Code)
	}
	// Without this a service worker may not control the whole origin.
	if got := rec2.Header().Get("Service-Worker-Allowed"); got != "/" {
		t.Errorf("Service-Worker-Allowed = %q, want /", got)
	}
	if ct := rec2.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("sw.js Content-Type = %q", ct)
	}

	rec3 := readGET(t, h, "/static/icon.png")
	if rec3.Code != http.StatusOK {
		t.Errorf("/static/icon.png: status = %d", rec3.Code)
	}

	// Traversal out of the static dir.
	os.WriteFile(filepath.Join(app.PluginDir, "runtime.html"), []byte("SECRET-SHELL"), 0o644)
	for _, p := range []string{"/static/../runtime.html", "/static/..%2Fruntime.html"} {
		r := readGET(t, h, p)
		if strings.Contains(r.Body.String(), "SECRET-SHELL") {
			t.Errorf("%s escaped the static directory", p)
		}
	}
}

func TestAPIPushPublicKey_ReturnsAStableKey(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)

	rec := readGET(t, h, "/api/push/public-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		PublicKey string `json:"publicKey"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, rec.Body.String())
	}
	if got.PublicKey == "" {
		t.Fatal("publicKey is empty; the browser cannot subscribe without it")
	}

	// Generated once and reused: a key that rotated per request would silently
	// invalidate every existing subscription.
	rec2 := readGET(t, h, "/api/push/public-key")
	var again struct {
		PublicKey string `json:"publicKey"`
	}
	json.Unmarshal(rec2.Body.Bytes(), &again)
	if again.PublicKey != got.PublicKey {
		t.Errorf("publicKey changed between calls: %q then %q", got.PublicKey, again.PublicKey)
	}
}

// --- end to end: a fresh install with files on disk and nothing in the DB ---

// /api/data imports the on-disk sources before it builds the payload, so the
// very first request a browser makes after a hook has run must already carry
// that session. Everything above seeds the tables directly; this one starts
// where a real install does — an empty database and a sidecar file — and
// exercises the whole import → build → encode chain the handler owns.
func TestAPIData_FirstRequestAfterAHookRunCarriesTheSession(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)

	const sid = "9f8e7d6c-5b4a-4392-8180-7f6e5d4c3b2a"
	sidecar := `{"lastOffset":42,"project":"periscope","effortLevel":"high",` +
		`"cumulative":{"input":1234,"cache_read":5678,"cache_write":90,"output":321,` +
		`"cost":4.56,"agent_calls":1,"tool_calls":3,"chat_calls":2,` +
		`"tools":{"Read":{"calls":2,"weighted":2},"Bash":{"calls":1,"weighted":1}}},` +
		`"lastTurn":{"cost":0.5,"type":"agent","model":"claude-sonnet-4-20250514","tools":["Read"]},` +
		`"models":{"claude-sonnet-4-20250514":3}}`
	if err := os.WriteFile(filepath.Join(app.DataDir, sid+".json"), []byte(sidecar), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	// The usage history JSONL the Stop hook appends to.
	histLine := `{"ts":"2026-03-01T12:00:00Z","sid":"9f8e7d6c","input":1234,"cr":5678,` +
		`"cw":90,"out":321,"cost":4.56,"turns":6,"effort":"high"}` + "\n"
	if err := os.WriteFile(filepath.Join(app.DataDir, "usage-history.jsonl"), []byte(histLine), 0o644); err != nil {
		t.Fatalf("write history jsonl: %v", err)
	}

	// Nothing has been imported yet.
	var n int
	if err := app.DB.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("the database is not empty before the request: %d sessions", n)
	}

	m := decodeData(t, readGET(t, h, "/api/data"))

	var sidecars []store.SidecarEntry
	if err := json.Unmarshal(m["sidecars"], &sidecars); err != nil {
		t.Fatalf("sidecars: %v", err)
	}
	if len(sidecars) != 1 {
		t.Fatalf("sidecars = %d, want the 1 file on disk", len(sidecars))
	}
	if sidecars[0].ID != sid {
		t.Errorf("sidecar id = %q, want %q", sidecars[0].ID, sid)
	}
	// The blob must arrive intact: the widgets read every one of these.
	var got struct {
		Project     string `json:"project"`
		EffortLevel string `json:"effortLevel"`
		Cumulative  struct {
			Cost  float64 `json:"cost"`
			Tools map[string]struct {
				Calls int `json:"calls"`
			} `json:"tools"`
		} `json:"cumulative"`
		LastTurn struct {
			Model string `json:"model"`
		} `json:"lastTurn"`
		Models map[string]int `json:"models"`
	}
	if err := json.Unmarshal(sidecars[0].Data, &got); err != nil {
		t.Fatalf("sidecar data: %v", err)
	}
	if got.Project != "periscope" || got.EffortLevel != "high" {
		t.Errorf("project/effort = %q/%q", got.Project, got.EffortLevel)
	}
	if got.Cumulative.Cost != 4.56 {
		t.Errorf("cost = %v, want 4.56", got.Cumulative.Cost)
	}
	if got.Cumulative.Tools["Read"].Calls != 2 || got.Cumulative.Tools["Bash"].Calls != 1 {
		t.Errorf("tool counts = %+v, want Read 2 / Bash 1", got.Cumulative.Tools)
	}
	if got.LastTurn.Model != "claude-sonnet-4-20250514" {
		t.Errorf("lastTurn.model = %q", got.LastTurn.Model)
	}
	if got.Models["claude-sonnet-4-20250514"] != 3 {
		t.Errorf("models = %+v", got.Models)
	}

	var history []map[string]any
	if err := json.Unmarshal(m["history"], &history); err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("history = %d rows, want the 1 JSONL line", len(history))
	}
	if history[0]["cost"] != 4.56 || history[0]["sid"] != "9f8e7d6c" {
		t.Errorf("history row = %v", history[0])
	}

	// A second request must not duplicate either source.
	m2 := decodeData(t, readGET(t, h, "/api/data"))
	var sidecars2 []store.SidecarEntry
	var history2 []json.RawMessage
	json.Unmarshal(m2["sidecars"], &sidecars2)
	json.Unmarshal(m2["history"], &history2)
	if len(sidecars2) != 1 || len(history2) != 1 {
		t.Errorf("a second GET duplicated rows: %d sidecars, %d history",
			len(sidecars2), len(history2))
	}
}
