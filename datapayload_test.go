package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Finding 4 (reader half), end to end: a corrupt sidecar row must not be able to
// take down the /api/data response. The old handler streamed the payload
// straight into the ResponseWriter, so an unencodable json.RawMessage("") failed
// mid-body — after the 200 status line had already been written — and the client
// got a truncated document with no error to go on.
func TestHandleData_CorruptRowDoesNotTruncateTheResponse(t *testing.T) {
	app := newTestApp(t, "")
	handler := newTestHandler(app)

	// A row that predates the validating importer.
	if _, err := app.DB.Exec(
		"INSERT INTO sessions(id, data, updated_at) VALUES(?, ?, ?)",
		"11111111-1111-4111-8111-111111111111", "", "2026-01-02T00:00:00Z"); err != nil {
		t.Fatalf("seed corrupt row: %v", err)
	}
	if _, err := app.DB.Exec(
		"INSERT INTO sessions(id, data, updated_at) VALUES(?, ?, ?)",
		"22222222-2222-4222-8222-222222222222", `{"cumulative":{"cost":1.0}}`, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed good row: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/data", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var payload struct {
		Sidecars []struct {
			ID string `json:"id"`
		} `json:"sidecars"`
		History      []json.RawMessage `json:"history"`
		LimitHistory []json.RawMessage `json:"limitHistory"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("response body is not complete JSON (%d bytes): %v", len(body), err)
	}
	if len(payload.Sidecars) != 1 {
		t.Fatalf("sidecars = %d, want 1 (the corrupt row must be dropped, not fatal)", len(payload.Sidecars))
	}
	if payload.Sidecars[0].ID != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("surviving sidecar = %q", payload.Sidecars[0].ID)
	}
}

// Finding 4 + 14: a truncated sidecar file and a non-session JSON file both stay
// out of the sessions table, so the served payload stays encodable.
func TestHandleData_TruncatedSidecarFileNeverReachesThePayload(t *testing.T) {
	app := newTestApp(t, "")
	handler := newTestHandler(app)

	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(app.DataDir, name), []byte(body), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("33333333-3333-4333-8333-333333333333.json", `{"cumulative":{"cost":2.0}}`)
	write("44444444-4444-4444-8444-444444444444.json", `{"cumulative":{"cost":`)
	write("some-other-tool-state.json", `{"cumulative":{"cost":999.0}}`)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/data", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var payload struct {
		Sidecars []struct {
			ID string `json:"id"`
		} `json:"sidecars"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response body is not complete JSON: %v", err)
	}
	if len(payload.Sidecars) != 1 {
		var ids []string
		for _, s := range payload.Sidecars {
			ids = append(ids, s.ID)
		}
		t.Fatalf("sidecars = %v, want only the one valid session sidecar", ids)
	}
}

// Finding 11: /api/data used to re-write every sidecar row on every request.
func TestHandleData_UnchangedSidecarsAreNotRewritten(t *testing.T) {
	app := newTestApp(t, "")
	handler := newTestHandler(app)

	if err := os.WriteFile(
		filepath.Join(app.DataDir, "55555555-5555-4555-8555-555555555555.json"),
		[]byte(`{"cumulative":{"cost":3.0}}`), 0644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	get := func() {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/data", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	}
	get()

	// Stamp the row so a rewrite is visible.
	if _, err := app.DB.Exec("UPDATE sessions SET updated_at = 'SENTINEL'"); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	get()

	var stamp string
	if err := app.DB.QueryRow("SELECT updated_at FROM sessions").Scan(&stamp); err != nil {
		t.Fatalf("read stamp: %v", err)
	}
	if stamp != "SENTINEL" {
		t.Fatalf("unchanged sidecar was rewritten on the second request (updated_at=%q)", stamp)
	}
}
