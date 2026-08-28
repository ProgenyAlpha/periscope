package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ProgenyAlpha/periscope/internal/store"
)

// Every test here runs against t.TempDir(). Nothing in this file may read or
// write ~/.periscope, ~/.claude, or the live server: doctor is a diagnostic for
// a machine that is already in trouble, and its own tests must not be able to
// make that worse.

func testDoctorEnv(t *testing.T) doctorEnv {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "periscope")
	claude := filepath.Join(root, "claude")
	data := filepath.Join(claude, "hooks", "cost-state")
	bin := filepath.Join(root, "bin", "periscope")
	for _, d := range []string{home, claude, data, filepath.Dir(bin)} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return doctorEnv{
		Now:         time.Now(),
		HomeDir:     home,
		ClaudeDir:   claude,
		DataDir:     data,
		Binary:      bin,
		Config:      AppConfig{Server: ServerConfig{Host: "127.0.0.1", Port: 8384}},
		probeHealth: func(string) (int, error) { return 200, nil },
	}
}

func pick(t *testing.T, results []checkResult, name string) checkResult {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no check named %q in %v", name, checkNames(results))
	return checkResult{}
}

func checkNames(results []checkResult) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.Name)
	}
	return out
}

func wantStatus(t *testing.T, r checkResult, want checkStatus) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("check %q: status = %s (%s), want %s", r.Name, r.Status, r.Detail, want)
	}
	if want != ckOK && strings.TrimSpace(r.Remedy) == "" {
		t.Fatalf("check %q is %s but carries no remediation line", r.Name, r.Status)
	}
}

// writeSettings writes a ~/.claude/settings.json with the given hook commands
// per event plus an optional statusLine command.
func writeSettings(t *testing.T, env doctorEnv, hooks map[string][]string, statusLine string) {
	t.Helper()
	settings := map[string]any{"theme": "dark"} // a key we must never disturb
	if len(hooks) > 0 {
		h := map[string]any{}
		for event, cmds := range hooks {
			groups := []any{}
			for _, c := range cmds {
				groups = append(groups, map[string]any{
					"hooks": []any{map[string]string{"type": "command", "command": c}},
				})
			}
			h[event] = groups
		}
		settings["hooks"] = h
	}
	if statusLine != "" {
		settings["statusLine"] = map[string]string{"type": "command", "command": statusLine}
	}
	raw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	if err := os.WriteFile(filepath.Join(env.ClaudeDir, claudeSettingsName), raw, 0644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

// healthySettings is what `periscope init` leaves behind when it works.
func healthySettings(t *testing.T, env doctorEnv) {
	t.Helper()
	launcher := filepath.Join(env.HomeDir, doctorLauncherName())
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write launcher: %v", err)
	}
	writeSettings(t, env, map[string][]string{
		"SessionStart":     {launcher},
		"Stop":             {env.Binary + " hook stop"},
		"UserPromptSubmit": {env.Binary + " hook display"},
	}, env.Binary+" statusline")
}

func writeDoctorSidecar(t *testing.T, env doctorEnv, id string, age time.Duration) {
	t.Helper()
	path := filepath.Join(env.DataDir, id+".json")
	if err := os.WriteFile(path, []byte(`{"cumulative":{"cost":1}}`), 0644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	mt := env.Now.Add(-age)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatalf("chtimes sidecar: %v", err)
	}
}

func writeTranscript(t *testing.T, env doctorEnv, age time.Duration) {
	t.Helper()
	dir := filepath.Join(env.ClaudeDir, "projects", "-home-user-proj")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	mt := env.Now.Add(-age)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatalf("chtimes transcript: %v", err)
	}
}

// makeDB creates a real periscope database in the temp home and stamps the
// given session ages into sessions.updated_at.
func makeDB(t *testing.T, env doctorEnv, sessionAges ...time.Duration) {
	t.Helper()
	db, err := store.OpenDB(env.dbPath())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	for i, age := range sessionAges {
		stamp := env.Now.Add(-age).UTC().Format("2006-01-02T15:04:05Z")
		_, err := db.Exec(`INSERT OR REPLACE INTO sessions(id, data, updated_at) VALUES(?,?,?)`,
			fmt.Sprintf("00000000-0000-0000-0000-%012d", i), `{"cumulative":{"cost":1}}`, stamp)
		if err != nil {
			t.Fatalf("insert session: %v", err)
		}
	}
	if err := store.CheckpointWAL(db); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
}

// ── 1. Claude hooks ─────────────────────────────────────────────────────────

func TestCheckClaudeHooks_AllRegistered(t *testing.T) {
	env := testDoctorEnv(t)
	healthySettings(t, env)

	results := checkClaudeHooks(env)
	wantStatus(t, pick(t, results, checkHooks), ckOK)
	wantStatus(t, pick(t, results, checkStatusLine), ckOK)
}

// The five-day outage: init printed the commands instead of writing them, so
// settings.json had no Stop hook and no sidecar was ever written again.
func TestCheckClaudeHooks_StopHookMissing(t *testing.T) {
	env := testDoctorEnv(t)
	launcher := filepath.Join(env.HomeDir, doctorLauncherName())
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, env, map[string][]string{
		"SessionStart":     {launcher},
		"UserPromptSubmit": {env.Binary + " hook display"},
	}, env.Binary+" statusline")

	r := pick(t, checkClaudeHooks(env), checkHooks)
	wantStatus(t, r, ckFail)
	if !strings.Contains(r.Detail, "Stop") {
		t.Fatalf("detail should name the missing Stop hook, got %q", r.Detail)
	}
}

func TestCheckClaudeHooks_NoHooksAtAll(t *testing.T) {
	env := testDoctorEnv(t)
	writeSettings(t, env, nil, "")

	wantStatus(t, pick(t, checkClaudeHooks(env), checkHooks), ckFail)
	wantStatus(t, pick(t, checkClaudeHooks(env), checkStatusLine), ckWarn)
}

func TestCheckClaudeHooks_MissingSettingsFile(t *testing.T) {
	env := testDoctorEnv(t)
	wantStatus(t, pick(t, checkClaudeHooks(env), checkHooks), ckFail)
}

// A hook registered against a binary that has since been deleted or moved runs
// nothing at all — exactly as silent as no hook.
func TestCheckClaudeHooks_PointsAtMissingBinary(t *testing.T) {
	env := testDoctorEnv(t)
	healthySettings(t, env)
	if err := os.Remove(env.Binary); err != nil {
		t.Fatal(err)
	}

	r := pick(t, checkClaudeHooks(env), checkHooks)
	wantStatus(t, r, ckFail)
	if !strings.Contains(r.Detail, "does not exist") {
		t.Fatalf("detail should say the target is gone, got %q", r.Detail)
	}
}

func TestCheckClaudeHooks_StatusLineOwnedByAnotherTool(t *testing.T) {
	env := testDoctorEnv(t)
	launcher := filepath.Join(env.HomeDir, doctorLauncherName())
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, env, map[string][]string{
		"SessionStart":     {launcher},
		"Stop":             {env.Binary + " hook stop"},
		"UserPromptSubmit": {env.Binary + " hook display"},
	}, "/usr/bin/some-other-statusline")

	wantStatus(t, pick(t, checkClaudeHooks(env), checkHooks), ckOK)
	wantStatus(t, pick(t, checkClaudeHooks(env), checkStatusLine), ckWarn)
}

// ── 2. Sidecar freshness ────────────────────────────────────────────────────

func TestCheckSidecarFreshness_OK(t *testing.T) {
	env := testDoctorEnv(t)
	writeDoctorSidecar(t, env, "55ad72ab-803a-4749-9cfa-c201d754cefc", 3*time.Minute)
	writeTranscript(t, env, time.Minute)

	wantStatus(t, checkSidecarFreshness(env), ckOK)
}

func TestCheckSidecarFreshness_FiveDayOutage(t *testing.T) {
	env := testDoctorEnv(t)
	writeDoctorSidecar(t, env, "55ad72ab-803a-4749-9cfa-c201d754cefc", 5*24*time.Hour)

	r := checkSidecarFreshness(env)
	wantStatus(t, r, ckFail)
	if !strings.Contains(r.Remedy, "init") {
		t.Fatalf("remedy should point at `periscope init`, got %q", r.Remedy)
	}
}

// The sharp version of the same detection: Claude is clearly running (it just
// appended to a transcript) but the Stop hook has written nothing. This fires
// on day one of an outage instead of day two.
func TestCheckSidecarFreshness_HookDeadWhileClaudeRuns(t *testing.T) {
	env := testDoctorEnv(t)
	writeDoctorSidecar(t, env, "55ad72ab-803a-4749-9cfa-c201d754cefc", 4*time.Hour)
	writeTranscript(t, env, time.Minute)

	r := checkSidecarFreshness(env)
	wantStatus(t, r, ckFail)
	if !strings.Contains(r.Detail, "transcript") {
		t.Fatalf("detail should contrast sidecar age with transcript age, got %q", r.Detail)
	}
}

// A long-running turn writes to the transcript continuously and only writes a
// sidecar when it stops, so a modest lag must not be called a failure.
func TestCheckSidecarFreshness_LongTurnIsNotAFailure(t *testing.T) {
	env := testDoctorEnv(t)
	writeDoctorSidecar(t, env, "55ad72ab-803a-4749-9cfa-c201d754cefc", 20*time.Minute)
	writeTranscript(t, env, time.Minute)

	wantStatus(t, checkSidecarFreshness(env), ckOK)
}

func TestCheckSidecarFreshness_EmptyDirWithActiveClaude(t *testing.T) {
	env := testDoctorEnv(t)
	writeTranscript(t, env, time.Minute)

	wantStatus(t, checkSidecarFreshness(env), ckFail)
}

func TestCheckSidecarFreshness_IgnoresNonSessionFiles(t *testing.T) {
	env := testDoctorEnv(t)
	// These live in cost-state too and are written by other code paths; a
	// freshly-polled limit-history.jsonl must never mask a dead Stop hook.
	for _, name := range []string{"limit-history.jsonl", "profile-cache.json", "litellm-pricing-cache.json"} {
		if err := os.WriteFile(filepath.Join(env.DataDir, name), []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeTranscript(t, env, time.Minute)

	wantStatus(t, checkSidecarFreshness(env), ckFail)
}

func TestCheckSidecarFreshness_MissingDirectory(t *testing.T) {
	env := testDoctorEnv(t)
	if err := os.RemoveAll(env.DataDir); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, checkSidecarFreshness(env), ckFail)
}

// ── 3. Ingestion freshness ──────────────────────────────────────────────────

func TestCheckIngestion_OK(t *testing.T) {
	env := testDoctorEnv(t)
	writeDoctorSidecar(t, env, "55ad72ab-803a-4749-9cfa-c201d754cefc", 2*time.Minute)
	makeDB(t, env, 2*time.Minute)

	wantStatus(t, checkIngestion(env), ckOK)
}

// Hooks work, sidecars are fresh, but nothing imports them: ImportSidecars
// logging count=0 as success is what made this invisible.
func TestCheckIngestion_DatabaseBehindSidecars(t *testing.T) {
	env := testDoctorEnv(t)
	writeDoctorSidecar(t, env, "55ad72ab-803a-4749-9cfa-c201d754cefc", time.Minute)
	makeDB(t, env, 3*time.Hour)

	r := checkIngestion(env)
	wantStatus(t, r, ckFail)
	if !strings.Contains(r.Detail, "sidecar") {
		t.Fatalf("detail should compare the DB against the sidecars, got %q", r.Detail)
	}
}

func TestCheckIngestion_StaleSessionsTable(t *testing.T) {
	env := testDoctorEnv(t)
	writeDoctorSidecar(t, env, "55ad72ab-803a-4749-9cfa-c201d754cefc", 5*24*time.Hour)
	makeDB(t, env, 5*24*time.Hour)

	wantStatus(t, checkIngestion(env), ckFail)
}

func TestCheckIngestion_NoDatabase(t *testing.T) {
	env := testDoctorEnv(t)
	wantStatus(t, checkIngestion(env), ckFail)
}

// ── 5. Database health ──────────────────────────────────────────────────────

func TestCheckDatabase_MissingFile(t *testing.T) {
	env := testDoctorEnv(t)
	wantStatus(t, pick(t, checkDatabase(env), checkDBFile), ckFail)
}

func TestCheckDatabase_HealthyFile(t *testing.T) {
	env := testDoctorEnv(t)
	makeDB(t, env, time.Minute)

	results := checkDatabase(env)
	wantStatus(t, pick(t, results, checkDBFile), ckOK)
	wantStatus(t, pick(t, results, checkDBSchema), ckOK)
	wantStatus(t, pick(t, results, checkDBWAL), ckOK)
}

// The database holds the VAPID private key, so 0644 is a real finding.
func TestCheckDatabase_WorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not meaningful on windows")
	}
	env := testDoctorEnv(t)
	makeDB(t, env, time.Minute)
	if err := os.Chmod(env.dbPath(), 0644); err != nil {
		t.Fatal(err)
	}

	r := pick(t, checkDatabase(env), checkDBFile)
	wantStatus(t, r, ckFail)
	if !strings.Contains(r.Remedy, "chmod") {
		t.Fatalf("remedy should be a chmod, got %q", r.Remedy)
	}
}

func TestCheckDatabase_StaleSchemaVersion(t *testing.T) {
	env := testDoctorEnv(t)
	makeDB(t, env, time.Minute)

	db, err := sql.Open("sqlite", env.dbPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE schema_version SET version = 1"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	wantStatus(t, pick(t, checkDatabase(env), checkDBSchema), ckFail)
}

func TestCheckDatabase_OversizeWAL(t *testing.T) {
	env := testDoctorEnv(t)
	makeDB(t, env, time.Minute)
	if err := os.WriteFile(env.dbPath()+"-wal", make([]byte, walFailBytes+1), 0600); err != nil {
		t.Fatal(err)
	}

	wantStatus(t, pick(t, checkDatabase(env), checkDBWAL), ckFail)
}

// ── 6. Log health ───────────────────────────────────────────────────────────

func TestCheckLog_HealthyAndDefaultLevel(t *testing.T) {
	env := testDoctorEnv(t)
	if err := os.WriteFile(env.logPath(), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}

	results := checkLogHealth(env)
	wantStatus(t, pick(t, results, checkLogFile), ckOK)
	wantStatus(t, pick(t, results, checkLogLevel), ckOK)
}

func TestCheckLog_OversizeLog(t *testing.T) {
	env := testDoctorEnv(t)
	if err := os.WriteFile(env.logPath(), make([]byte, logFailBytes+1), 0644); err != nil {
		t.Fatal(err)
	}

	wantStatus(t, pick(t, checkLogHealth(env), checkLogFile), ckFail)
}

// Debug was 72% of log volume when it was the hardcoded level (logging.go).
func TestCheckLog_DebugLevelIsCalledOut(t *testing.T) {
	env := testDoctorEnv(t)
	env.Config.Logging.Level = "debug"
	if err := os.WriteFile(env.logPath(), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}

	wantStatus(t, pick(t, checkLogHealth(env), checkLogLevel), ckWarn)
}

// ── 4. Server reachability ──────────────────────────────────────────────────

func TestCheckServer_UsesConfiguredHostNotLocalhost(t *testing.T) {
	env := testDoctorEnv(t)
	env.Config.Server.Host = "100.115.109.120"
	env.Config.Server.Port = 9999
	var got string
	env.probeHealth = func(url string) (int, error) { got = url; return 200, nil }

	wantStatus(t, checkServerReachable(env), ckOK)
	if !strings.Contains(got, "100.115.109.120:9999") {
		t.Fatalf("probed %q, want the configured host:port", got)
	}
}

func TestCheckServer_Unreachable(t *testing.T) {
	env := testDoctorEnv(t)
	env.probeHealth = func(string) (int, error) { return 0, fmt.Errorf("connection refused") }

	wantStatus(t, checkServerReachable(env), ckFail)
}

func TestCheckServer_BadStatus(t *testing.T) {
	env := testDoctorEnv(t)
	env.probeHealth = func(string) (int, error) { return 503, nil }

	wantStatus(t, checkServerReachable(env), ckFail)
}

// ── 7. Disk ─────────────────────────────────────────────────────────────────

func TestCheckDisk_ReportsBothDirectories(t *testing.T) {
	env := testDoctorEnv(t)
	names := checkNames(checkDiskSpace(env))
	for _, want := range []string{checkDiskHome, checkDiskData} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing disk check %q in %v", want, names)
		}
	}
}

// ── Aggregate ───────────────────────────────────────────────────────────────

func TestRunDoctorChecks_ExitsNonZeroOnAnyFailure(t *testing.T) {
	env := testDoctorEnv(t)
	// Nothing set up at all: hooks, sidecars, DB and log are all absent.
	results := runDoctorChecks(env)
	if doctorExitCode(results) == 0 {
		t.Fatalf("expected non-zero exit for %v", results)
	}
}

func TestDoctorExitCode_ZeroWhenOnlyWarnings(t *testing.T) {
	results := []checkResult{
		{Name: "a", Status: ckOK},
		{Name: "b", Status: ckWarn, Remedy: "do a thing"},
	}
	if code := doctorExitCode(results); code != 0 {
		t.Fatalf("warnings must not fail the run, got exit %d", code)
	}
}

func TestDoctorSummaryCountsEveryStatus(t *testing.T) {
	results := []checkResult{
		{Name: "a", Status: ckOK},
		{Name: "b", Status: ckWarn},
		{Name: "c", Status: ckFail},
		{Name: "d", Status: ckOK},
	}
	got := doctorSummary(results)
	for _, want := range []string{"4 checks", "2 ok", "1 warning", "1 failure"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary %q missing %q", got, want)
		}
	}
}

func TestRunDoctorChecks_HealthyMachineIsAllOK(t *testing.T) {
	env := testDoctorEnv(t)
	healthySettings(t, env)
	writeDoctorSidecar(t, env, "55ad72ab-803a-4749-9cfa-c201d754cefc", 2*time.Minute)
	writeTranscript(t, env, time.Minute)
	makeDB(t, env, 2*time.Minute)
	if err := os.WriteFile(env.logPath(), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}

	results := runDoctorChecks(env)
	for _, r := range results {
		if r.Status == ckFail {
			t.Fatalf("healthy machine reported FAIL on %q: %s", r.Name, r.Detail)
		}
	}
	if doctorExitCode(results) != 0 {
		t.Fatalf("healthy machine must exit 0")
	}
}
