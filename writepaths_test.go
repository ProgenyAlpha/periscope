package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProgenyAlpha/periscope/internal/store"
)

// installEnv builds an App whose every directory lives under t.TempDir(), so
// install() can be driven for real without touching ~/.claude or ~/.periscope.
func installEnv(t *testing.T) *App {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "periscope")
	claude := filepath.Join(root, "claude")
	return &App{
		Config:    AppConfig{Server: ServerConfig{Host: "127.0.0.1", Port: 8384}},
		HomeDir:   home,
		ClaudeDir: claude,
		DataDir:   filepath.Join(claude, "hooks", "cost-state"),
		PluginDir: filepath.Join(home, "plugins"),
	}
}

// install() creates ~/.periscope and its plugin tree but never created
// ~/.claude/hooks/cost-state — the directory every telemetry writer targets.
// Until the first Stop hook happened to mkdir it, the polling loop's usage and
// profile caches, the LiteLLM pricing cache and the limit-history JSONL the
// statusline forecast reads all failed with ENOENT, mostly into a log line.
func TestInstall_CreatesTelemetryDataDir(t *testing.T) {
	app := installEnv(t)

	if err := install(app, installOptions{}); err != nil {
		t.Fatalf("install: %v", err)
	}

	st, err := os.Stat(app.DataDir)
	if err != nil {
		t.Fatalf("install left the telemetry data dir absent: %v", err)
	}
	if !st.IsDir() {
		t.Fatalf("%s is not a directory", app.DataDir)
	}
}

// install() reported "Created config.toml" from an os.WriteFile whose error it
// discarded, so a home directory it could not write to still finished as a
// clean install.
func TestInstall_ReportsConfigWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny writes")
	}
	app := installEnv(t)
	// Pre-create a home directory that cannot be written to. MkdirAll on an
	// existing directory succeeds regardless of its mode, so install() gets
	// past step 1 and fails on the config write.
	if err := os.MkdirAll(app.HomeDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Plugins live elsewhere so the sync step is not what fails.
	app.PluginDir = filepath.Join(t.TempDir(), "plugins")
	if err := os.Chmod(app.HomeDir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(app.HomeDir, 0755) })

	if err := install(app, installOptions{}); err == nil {
		t.Fatal("install reported success although config.toml could not be written")
	}
}

// setupLogging runs before the first-run install() in cmdServe, so on a fresh
// machine ~/.periscope does not exist yet. os.OpenFile does not create parent
// directories: the open failed, the process fell back to stderr for its whole
// lifetime, and nothing was ever written to periscope.log.
func TestNewRotatingWriter_CreatesMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "periscope", "periscope.log")

	w, err := newRotatingWriter(path, defaultMaxLogBytes)
	if err != nil {
		t.Fatalf("newRotatingWriter into a missing directory: %v", err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("log file not created: %v", err)
	}
	if string(got) != "hello\n" {
		t.Errorf("log contents = %q", got)
	}
}

// The polling loop's caches are read by *other processes* — the statusline and
// the display hook both open usage-api-cache.json — so they cannot be written
// with a truncate-then-write os.WriteFile into a directory that may not exist.
func TestWriteDataCache_CreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hooks", "cost-state")

	if err := writeDataCache(dir, "usage-api-cache.json", []byte(`{"pct5hr":10}`), 0644); err != nil {
		t.Fatalf("writeDataCache into a missing directory: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "usage-api-cache.json"))
	if err != nil {
		t.Fatalf("cache not written: %v", err)
	}
	if string(got) != `{"pct5hr":10}` {
		t.Errorf("cache contents = %q", got)
	}
}

func TestWriteDataCache_HonoursPermOnCreate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cost-state")
	if err := writeDataCache(dir, "profile-cache.json", []byte(`{}`), 0600); err != nil {
		t.Fatalf("writeDataCache: %v", err)
	}
	st, err := os.Stat(filepath.Join(dir, "profile-cache.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0600 {
		t.Errorf("profile cache mode = %04o, want 0600 (it holds the account email)", perm)
	}
}

// Source-level guard: the two polling caches must not go back to a bare
// os.WriteFile, which neither creates the directory nor swaps atomically.
func TestPollingCachesUseWriteDataCache(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`os.WriteFile(filepath.Join(app.DataDir, "usage-api-cache.json")`,
		`os.WriteFile(filepath.Join(app.DataDir, "profile-cache.json")`,
		"os.WriteFile(settingsPath",
		"os.WriteFile(configPath",
	} {
		if strings.Contains(string(src), bad) {
			t.Errorf("server.go still writes non-atomically into a possibly-absent dir: %s", bad)
		}
	}
}

// sendPushNotification logged sub.Endpoint[:40] on a send failure. /api/push/subscribe
// accepts any non-empty endpoint, so a short one panicked the whole handler.
func TestSendPushNotification_ShortEndpointDoesNotPanic(t *testing.T) {
	db, err := store.OpenDB(filepath.Join(t.TempDir(), "periscope.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := store.PushSubscribe(db, "x", "auth", "p256dh"); err != nil {
		t.Fatal(err)
	}
	// Sending to "x" cannot succeed; the point is that the failure path is
	// reachable without slicing a 1-byte string at [:40].
	if err := sendPushNotification(db, "Periscope", "test"); err != nil {
		t.Fatalf("sendPushNotification returned an error: %v", err)
	}
}

// KVSet swallows its error, so a handler that stored a value through it could
// only ever report success. KVSetErr gives the caller the failure back.
func TestKVSetErr_ReportsFailure(t *testing.T) {
	db, err := store.OpenDB(filepath.Join(t.TempDir(), "periscope.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := store.KVSetErr(db, "config:layout", `{"grid":[1]}`); err != nil {
		t.Fatalf("KVSetErr on a healthy DB: %v", err)
	}
	raw := store.KVGet(db, "config:layout")
	var got map[string]any
	if json.Unmarshal(raw, &got) != nil {
		t.Fatalf("value not stored, got %q", raw)
	}

	if _, err := db.Exec("DROP TABLE kv"); err != nil {
		t.Fatal(err)
	}
	if err := store.KVSetErr(db, "config:layout", `{"grid":[2]}`); err == nil {
		t.Fatal("KVSetErr reported success with no kv table")
	}
}
