package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// seedLimitHistory fills limit_history with enough rows that the /api/data
// payload comfortably clears the compression threshold, so these tests are
// exercising the real gzip path and not the small-body bypass.
func seedLimitHistory(t *testing.T, app *App, n int) {
	t.Helper()
	base := time.Now().UTC().Add(-time.Duration(n) * time.Minute)
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		data := fmt.Sprintf(`{"ts":%q,"pct5hr":%d,"pctWeekly":%d,"pctSonnet":%d,"reset5hr":"2026-08-28T12:00:00Z","resetWeekly":"2026-09-01T00:00:00Z"}`,
			ts, i%100, i%50, i%25)
		if _, err := app.DB.Exec("INSERT INTO limit_history(ts, data) VALUES(?, ?)", ts, data); err != nil {
			t.Fatalf("seed limit_history: %v", err)
		}
	}
}

// noAutoCompressClient returns a client that does NOT add its own
// Accept-Encoding: gzip and does NOT transparently decompress, so the test
// controls the header and sees the wire bytes.
func noAutoCompressClient() *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{DisableCompression: true},
	}
}

func TestAPIData_GzipsWhenClientAcceptsIt(t *testing.T) {
	app := newTestApp(t, "")
	seedLimitHistory(t, app, 400)
	srv := httptest.NewServer(newTestHandler(app))
	defer srv.Close()

	client := noAutoCompressClient()

	// 1. Identity request — the reference bytes.
	plainReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/data", nil)
	plainResp, err := client.Do(plainReq)
	if err != nil {
		t.Fatalf("identity request: %v", err)
	}
	plainBody, err := io.ReadAll(plainResp.Body)
	plainResp.Body.Close()
	if err != nil {
		t.Fatalf("read identity body: %v", err)
	}
	if plainResp.StatusCode != http.StatusOK {
		t.Fatalf("identity status = %d, want 200", plainResp.StatusCode)
	}
	if ce := plainResp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("Content-Encoding = %q on a request that did not ask for gzip", ce)
	}
	if v := plainResp.Header.Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
		t.Fatalf("Vary = %q, want it to contain Accept-Encoding even on the identity response", v)
	}

	// 2. gzip request.
	gzReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/data", nil)
	gzReq.Header.Set("Accept-Encoding", "gzip")
	gzResp, err := client.Do(gzReq)
	if err != nil {
		t.Fatalf("gzip request: %v", err)
	}
	wire, err := io.ReadAll(gzResp.Body)
	gzResp.Body.Close()
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	if gzResp.StatusCode != http.StatusOK {
		t.Fatalf("gzip status = %d, want 200", gzResp.StatusCode)
	}
	if ce := gzResp.Header.Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", ce)
	}
	if ct := gzResp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if v := gzResp.Header.Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
		t.Fatalf("Vary = %q, want it to contain Accept-Encoding", v)
	}

	zr, err := gzip.NewReader(bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("body is not valid gzip: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if err := zr.Close(); err != nil {
		t.Fatalf("gzip reader close: %v", err)
	}

	// /api/data embeds generatedAt, so the two bodies differ by that field
	// alone. Compare lengths loosely and structure exactly.
	if len(got) == 0 {
		t.Fatal("decompressed body is empty")
	}
	if !bytes.HasPrefix(got, []byte("{")) || !bytes.HasSuffix(bytes.TrimSpace(got), []byte("}")) {
		t.Fatalf("decompressed body is not a complete JSON object (%d bytes)", len(got))
	}
	if d := len(got) - len(plainBody); d < -256 || d > 256 {
		t.Fatalf("decompressed length %d differs from identity length %d by more than the timestamp field",
			len(got), len(plainBody))
	}
	if len(wire) >= len(got) {
		t.Fatalf("compressed %d bytes is not smaller than raw %d bytes", len(wire), len(got))
	}
	t.Logf("identity=%d bytes gzip=%d bytes ratio=%.2fx", len(got), len(wire), float64(len(got))/float64(len(wire)))
}

// A round trip through the real client, which negotiates gzip itself, must
// produce byte-identical JSON to what the encoder wrote.
func TestAPIData_GzipRoundTripIsByteIdentical(t *testing.T) {
	app := newTestApp(t, "")
	seedLimitHistory(t, app, 400)
	srv := httptest.NewServer(newTestHandler(app))
	defer srv.Close()

	client := noAutoCompressClient()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/data", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	wire, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	zr, err := gzip.NewReader(bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}

	// Recompress the decoded bytes with the same settings and confirm the
	// server produced exactly that — no truncation, no double-encoding.
	var recompressed bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&recompressed, gzipLevel)
	zw.Write(decoded)
	zw.Close()
	if !bytes.Equal(recompressed.Bytes(), wire) {
		t.Fatalf("wire bytes are not a plain gzip stream of the payload (wire=%d recompressed=%d)",
			len(wire), recompressed.Len())
	}
}

func TestAcceptsGzip(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"gzip", true},
		{"GZIP", true},
		{"deflate, gzip;q=1.0, *;q=0.5", true},
		{"gzip;q=0", false},
		{"gzip;q=0.0", false},
		{"gzip; q=0", false},
		{"deflate", false},
		{"identity", false},
		{"br", false},
		{"notgzip", false},
		{"x-gzip", false},
		{"deflate, gzip", true},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/data", nil)
		if c.header != "" {
			r.Header.Set("Accept-Encoding", c.header)
		}
		if got := acceptsGzip(r); got != c.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", c.header, got, c.want)
		}
	}
}

// A payload below the threshold is not worth a gzip header + trailer.
func TestAPIData_SmallPayloadIsNotCompressed(t *testing.T) {
	app := newTestApp(t, "")
	srv := httptest.NewServer(newTestHandler(app))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/data", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := noAutoCompressClient().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if len(body) >= gzipMinSize {
		t.Skipf("empty-DB payload is %d bytes, at or above the %d-byte threshold", len(body), gzipMinSize)
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("Content-Encoding = %q on a %d-byte payload, want no compression below %d bytes",
			ce, len(body), gzipMinSize)
	}
}
