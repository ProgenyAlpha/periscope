package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ProgenyAlpha/periscope/internal/store"
	"github.com/gorilla/websocket"
)

func dialTestWSPath(t *testing.T, srvURL, path string) *websocket.Conn {
	t.Helper()
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, err := d.Dial("ws"+strings.TrimPrefix(srvURL, "http")+path, nil)
	if err != nil {
		t.Fatalf("ws dial %s: %v", path, err)
	}
	if resp != nil {
		resp.Body.Close()
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func waitClients(t *testing.T, app *App, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for app.Hub.clientCount() < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if app.Hub.clientCount() < n {
		t.Fatalf("only %d of %d clients registered", app.Hub.clientCount(), n)
	}
}

func readDataHistory(t *testing.T, conn *websocket.Conn) []json.RawMessage {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var msg struct {
		Type    string `json:"type"`
		Payload struct {
			History []json.RawMessage `json:"history"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("ws frame is not the data envelope: %v", err)
	}
	if msg.Type != "data" {
		t.Fatalf("frame type = %q, want data", msg.Type)
	}
	return msg.Payload.History
}

// The push path is where the bytes actually recur — a legacy client must still
// get the whole array, and a client that asked for the rollup must get the
// thinned one, out of the SAME broadcast.
func TestHubBroadcastData_PerClientHistoryShape(t *testing.T) {
	app := newTestApp(t, "")
	srv := httptest.NewServer(newTestHandler(app))
	defer srv.Close()

	seedHistoryRows(t, app, "oldsess1", time.Now().Add(-90*24*time.Hour), 400, time.Hour)

	legacy := dialTestWSPath(t, srv.URL, "/ws")
	rolled := dialTestWSPath(t, srv.URL, "/ws?history=rollup")
	waitClients(t, app, 2)

	if !app.Hub.wantsRollup() {
		t.Fatal("hub does not report that a client asked for the rollup")
	}

	full, err := store.BuildDashboardData(app.DB, app.DataDir)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	broadcastDashboardData(app, full)

	gotLegacy := readDataHistory(t, legacy)
	gotRolled := readDataHistory(t, rolled)

	if len(gotLegacy) != 400 {
		t.Fatalf("legacy ws client got %d history rows, want all 400", len(gotLegacy))
	}
	if len(gotRolled) >= len(gotLegacy) {
		t.Fatalf("rollup ws client got %d rows, legacy got %d; want a reduction",
			len(gotRolled), len(gotLegacy))
	}
}

// With no rollup client connected the hub must not pay to build the variant.
func TestHubBroadcastData_NoRollupClientMeansNoVariant(t *testing.T) {
	app := newTestApp(t, "")
	srv := httptest.NewServer(newTestHandler(app))
	defer srv.Close()

	seedHistoryRows(t, app, "oldsess1", time.Now().Add(-90*24*time.Hour), 100, time.Hour)
	legacy := dialTestWSPath(t, srv.URL, "/ws")
	waitClients(t, app, 1)

	if app.Hub.wantsRollup() {
		t.Fatal("hub reports a rollup client where there is none")
	}
	full, err := store.BuildDashboardData(app.DB, app.DataDir)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	broadcastDashboardData(app, full)
	if n := len(readDataHistory(t, legacy)); n != 100 {
		t.Fatalf("legacy client got %d rows, want 100", n)
	}
}

// An unknown ?history= value on the websocket must not break the connection;
// it falls back to the full shape rather than 400-ing a long-lived socket.
func TestServeWS_UnknownHistoryParamFallsBackToFull(t *testing.T) {
	app := newTestApp(t, "")
	srv := httptest.NewServer(newTestHandler(app))
	defer srv.Close()

	seedHistoryRows(t, app, "oldsess1", time.Now().Add(-90*24*time.Hour), 100, time.Hour)
	conn := dialTestWSPath(t, srv.URL, "/ws?history=banana")
	waitClients(t, app, 1)
	if app.Hub.wantsRollup() {
		t.Fatal("an unrecognised history param was treated as a rollup request")
	}
	full, err := store.BuildDashboardData(app.DB, app.DataDir)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	broadcastDashboardData(app, full)
	if n := len(readDataHistory(t, conn)); n != 100 {
		t.Fatalf("fallback client got %d rows, want the full 100", n)
	}
}
