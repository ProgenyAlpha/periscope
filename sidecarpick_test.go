package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeSidecar(t *testing.T, dir, sid string, turns int, mod time.Time) {
	t.Helper()
	body := `{"cumulative":{"input":1,"cache_read":1,"chat_calls":` + itoa(turns) + `,"tools":{}},"lastTurn":{"type":"chat"}}`
	p := filepath.Join(dir, sid+".json")
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, mod, mod)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestSidecarPicksOwnSessionNotNewest(t *testing.T) {
	dir := t.TempDir()
	writeSidecar(t, dir, "mine", 420, time.Now().Add(-time.Hour))
	writeSidecar(t, dir, "other", 2286, time.Now())

	got := loadSidecarForStatuslineFor(dir, "mine")
	if got.Turns != 420 {
		t.Errorf("Turns = %d, want 420 — picked another session's sidecar", got.Turns)
	}
}

func TestSidecarFallsBackToNewestWhenSessionUnknown(t *testing.T) {
	dir := t.TempDir()
	writeSidecar(t, dir, "other", 2286, time.Now())
	got := loadSidecarForStatuslineFor(dir, "")
	if got.Turns != 2286 {
		t.Errorf("Turns = %d, want 2286 from newest when no session id", got.Turns)
	}
}
