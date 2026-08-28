package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// dialTestWS opens a websocket against the test server, optionally asking for
// permessage-deflate, and returns the connection plus the handshake response.
func dialTestWS(t *testing.T, srvURL string, compress bool) (*websocket.Conn, string) {
	t.Helper()
	d := websocket.Dialer{
		HandshakeTimeout:  10 * time.Second,
		EnableCompression: compress,
	}
	wsURL := "ws" + strings.TrimPrefix(srvURL, "http") + "/ws"
	conn, resp, err := d.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	ext := ""
	if resp != nil {
		ext = resp.Header.Get("Sec-WebSocket-Extensions")
		resp.Body.Close()
	}
	return conn, ext
}

// A client that offers permessage-deflate must get it negotiated, and the
// payload it reads back must be the exact JSON the hub broadcast.
func TestHubBroadcast_NegotiatesPermessageDeflate(t *testing.T) {
	app := newTestApp(t, "")
	srv := httptest.NewServer(newTestHandler(app))
	defer srv.Close()

	conn, ext := dialTestWS(t, srv.URL, true)
	if !strings.Contains(ext, "permessage-deflate") {
		t.Fatalf("Sec-WebSocket-Extensions = %q, want permessage-deflate negotiated", ext)
	}

	// Give the hub's register channel a moment to land the client.
	deadline := time.Now().Add(3 * time.Second)
	for app.Hub.clientCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if app.Hub.clientCount() == 0 {
		t.Fatal("client never registered with the hub")
	}

	payload := map[string]any{"hello": strings.Repeat("compressible ", 500)}
	app.Hub.broadcastJSON("data", payload)

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	typ, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	if typ != websocket.TextMessage {
		t.Fatalf("message type = %d, want TextMessage", typ)
	}
	var got struct {
		Type    string `json:"type"`
		Payload struct {
			Hello string `json:"hello"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("broadcast payload is not valid JSON after deflate round trip: %v", err)
	}
	if got.Type != "data" {
		t.Fatalf("type = %q, want data", got.Type)
	}
	if got.Payload.Hello != strings.Repeat("compressible ", 500) {
		t.Fatalf("payload corrupted through the deflate round trip (%d bytes)", len(got.Payload.Hello))
	}
}

// A client that does NOT offer permessage-deflate must still get an
// uncompressed, readable frame — the prepared-message cache holds both forms.
func TestHubBroadcast_UncompressedClientStillWorks(t *testing.T) {
	app := newTestApp(t, "")
	srv := httptest.NewServer(newTestHandler(app))
	defer srv.Close()

	conn, ext := dialTestWS(t, srv.URL, false)
	if strings.Contains(ext, "permessage-deflate") {
		t.Fatalf("Sec-WebSocket-Extensions = %q, want no extension for a client that did not offer one", ext)
	}

	deadline := time.Now().Add(3 * time.Second)
	for app.Hub.clientCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	app.Hub.broadcastJSON("reload", map[string]string{"plugin": "limit-timeline"})

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var got struct {
		Type    string            `json:"type"`
		Payload map[string]string `json:"payload"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("uncompressed broadcast is not valid JSON: %v", err)
	}
	if got.Type != "reload" || got.Payload["plugin"] != "limit-timeline" {
		t.Fatalf("unexpected broadcast: %s", raw)
	}
}

// Both client kinds share one PreparedMessage; each must decode its own form.
func TestHubBroadcast_MixedClientsBothDecode(t *testing.T) {
	app := newTestApp(t, "")
	srv := httptest.NewServer(newTestHandler(app))
	defer srv.Close()

	cz, _ := dialTestWS(t, srv.URL, true)
	cp, _ := dialTestWS(t, srv.URL, false)

	deadline := time.Now().Add(3 * time.Second)
	for app.Hub.clientCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if app.Hub.clientCount() < 2 {
		t.Fatalf("clients registered = %d, want 2", app.Hub.clientCount())
	}

	body := strings.Repeat("x", 20000)
	app.Hub.broadcastJSON("data", map[string]string{"blob": body})

	for name, c := range map[string]*websocket.Conn{"deflate": cz, "plain": cp} {
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, raw, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("%s read: %v", name, err)
		}
		var got struct {
			Payload map[string]string `json:"payload"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s payload invalid: %v", name, err)
		}
		if got.Payload["blob"] != body {
			t.Fatalf("%s payload corrupted", name)
		}
	}
}
