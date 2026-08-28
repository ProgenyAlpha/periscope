package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- log level resolution ---

func TestResolveLogLevelDefaultsToInfo(t *testing.T) {
	t.Setenv(logLevelEnvVar, "")
	if got := resolveLogLevel(""); got != slog.LevelInfo {
		t.Fatalf("empty config with no env: got %v, want %v", got, slog.LevelInfo)
	}
}

func TestResolveLogLevelFromConfig(t *testing.T) {
	t.Setenv(logLevelEnvVar, "")
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		" info ":  slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
	}
	for in, want := range cases {
		if got := resolveLogLevel(in); got != want {
			t.Errorf("resolveLogLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestResolveLogLevelEnvOverridesConfig(t *testing.T) {
	t.Setenv(logLevelEnvVar, "error")
	if got := resolveLogLevel("debug"); got != slog.LevelError {
		t.Fatalf("env should win: got %v, want %v", got, slog.LevelError)
	}
}

func TestResolveLogLevelIgnoresGarbage(t *testing.T) {
	t.Setenv(logLevelEnvVar, "")
	if got := resolveLogLevel("verbose"); got != slog.LevelInfo {
		t.Errorf("bad config value: got %v, want %v", got, slog.LevelInfo)
	}
	// A bad env value must not discard a valid config value.
	t.Setenv(logLevelEnvVar, "loud")
	if got := resolveLogLevel("warn"); got != slog.LevelWarn {
		t.Errorf("bad env value: got %v, want %v", got, slog.LevelWarn)
	}
}

// --- rotation ---

func TestRotatingWriterRotatesWhileWriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "periscope.log")

	w, err := newRotatingWriter(path, 64)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer w.Close()

	line := strings.Repeat("a", 40) + "\n"
	for i := 0; i < 3; i++ {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat live log: %v", err)
	}
	if st.Size() > 64 {
		t.Fatalf("live log not rotated: size %d exceeds max 64", st.Size())
	}
	if _, err := os.Stat(path + rotatedSuffix); err != nil {
		t.Fatalf("expected rotated file %s: %v", path+rotatedSuffix, err)
	}
}

func TestRotatingWriterRotatesOversizedFileOnOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "periscope.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 500)), 0644); err != nil {
		t.Fatal(err)
	}

	w, err := newRotatingWriter(path, 100)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer w.Close()

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat live log: %v", err)
	}
	if st.Size() != 0 {
		t.Fatalf("oversized log should have been rotated away, size is %d", st.Size())
	}
	old, err := os.ReadFile(path + rotatedSuffix)
	if err != nil {
		t.Fatalf("read rotated file: %v", err)
	}
	if len(old) != 500 {
		t.Fatalf("rotated file lost data: %d bytes, want 500", len(old))
	}
}

// Rotation must never truncate a file another process still holds open: the
// old finding was os.Truncate on a live path, which destroyed the other
// process's log in place. Renaming keeps that process writing to the rotated
// file instead.
func TestRotatingWriterDoesNotTruncateAnotherProcessesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "periscope.log")

	// Stand in for the other process: an append handle held across rotation.
	other, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.WriteString(strings.Repeat("o", 200) + "\n"); err != nil {
		t.Fatal(err)
	}

	w, err := newRotatingWriter(path, 100)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer w.Close()

	if _, err := other.WriteString("still here\n"); err != nil {
		t.Fatalf("other process write after rotation: %v", err)
	}

	rotated, err := os.ReadFile(path + rotatedSuffix)
	if err != nil {
		t.Fatalf("read rotated file: %v", err)
	}
	if !strings.Contains(string(rotated), "still here") {
		t.Error("the other process's post-rotation write was lost")
	}
	if !strings.Contains(string(rotated), strings.Repeat("o", 200)) {
		t.Error("the other process's earlier log lines were destroyed")
	}
	live, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read live log: %v", err)
	}
	if strings.Contains(string(live), "still here") {
		t.Error("live log should be a fresh file, not the other process's")
	}
}

func TestRotatingWriterOpenErrorIsReported(t *testing.T) {
	dir := t.TempDir()
	// A directory can never be opened for writing.
	if _, err := newRotatingWriter(dir, 100); err == nil {
		t.Fatal("expected an error opening a directory as a log file")
	}
}

func TestRotatingWriterWatchRotatesPeriodically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "periscope.log")

	w, err := newRotatingWriter(path, 1024)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.watch(ctx, 5*time.Millisecond)
	}()

	// Growth that the writer itself did not perform still has to be noticed.
	if err := os.WriteFile(path, []byte(strings.Repeat("z", 4096)), 0644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		if st, err := os.Stat(path); err == nil && st.Size() <= 1024 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watch did not rotate an oversized log file")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not stop when its context was cancelled")
	}
}

// --- setupLogging wiring ---

func TestSetupLoggingHonoursLevel(t *testing.T) {
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })

	dir := t.TempDir()
	path := filepath.Join(dir, "periscope.log")

	w := setupLogging(path, slog.LevelWarn)
	if w == nil {
		t.Fatal("setupLogging returned no writer")
	}
	defer w.Close()

	slog.Debug("debug-line")
	slog.Info("info-line")
	slog.Warn("warn-line")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	got := string(data)
	if strings.Contains(got, "debug-line") {
		t.Error("debug output written at warn level")
	}
	if strings.Contains(got, "info-line") {
		t.Error("info output written at warn level")
	}
	if !strings.Contains(got, "warn-line") {
		t.Error("warn output missing from log file")
	}
}

func TestSetupLoggingFallsBackWhenFileUnusable(t *testing.T) {
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })

	// A path whose parent does not exist cannot be opened.
	bad := filepath.Join(t.TempDir(), "missing", "periscope.log")
	if w := setupLogging(bad, slog.LevelInfo); w != nil {
		w.Close()
		t.Fatal("expected no writer when the log file cannot be opened")
	}
	// Logging must still work (stderr only) rather than panic.
	slog.Info("still logging")
}

func TestRotatingWriterWriteAfterCloseIsInert(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "periscope.log")

	w, err := newRotatingWriter(path, 10)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := w.Write([]byte(strings.Repeat("q", 100))); err != nil {
		t.Fatalf("write after close: %v", err)
	}
	if err := w.checkRotate(); err != nil {
		t.Fatalf("checkRotate after close: %v", err)
	}
	if _, err := os.Stat(path + rotatedSuffix); !os.IsNotExist(err) {
		t.Fatal("a closed writer must not rotate or reopen the log")
	}
}

// Config files written before the [logging] key existed must still load, and
// the new key must reach the level resolver when present.
func TestNewAppLoadsLoggingLevelFromConfig(t *testing.T) {
	t.Setenv(logLevelEnvVar, "")

	write := func(body string) *App {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home) // windows
		if err := os.MkdirAll(filepath.Join(home, ".periscope"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".periscope", "config.toml"), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		app, err := newApp()
		if err != nil {
			t.Fatalf("newApp: %v", err)
		}
		return app
	}

	legacy := write("[server]\nport = 8384\n")
	if legacy.Config.Logging.Level != "" {
		t.Errorf("legacy config: level = %q, want empty", legacy.Config.Logging.Level)
	}
	if got := resolveLogLevel(legacy.Config.Logging.Level); got != slog.LevelInfo {
		t.Errorf("legacy config resolves to %v, want %v", got, slog.LevelInfo)
	}

	current := write("[server]\nport = 8384\n\n[logging]\nlevel = \"debug\"\n")
	if got := resolveLogLevel(current.Config.Logging.Level); got != slog.LevelDebug {
		t.Errorf("config level not honoured: got %v, want %v", got, slog.LevelDebug)
	}
}
