package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hooksFor extracts the list of commands registered for a given Claude hook
// event from a settings.json blob.
func hooksFor(t *testing.T, settings map[string]any, event string) []string {
	t.Helper()
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	groups, ok := hooks[event].([]any)
	if !ok {
		return nil
	}
	var cmds []string
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		entries, ok := gm["hooks"].([]any)
		if !ok {
			continue
		}
		for _, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if c, ok := em["command"].(string); ok {
				cmds = append(cmds, c)
			}
		}
	}
	return cmds
}

func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v (content: %s)", path, err, data)
	}
	return m
}

// testDesired is the standard set of Periscope hooks used by the tests.
func testDesired() desiredClaudeSettings {
	return desiredClaudeSettings{
		hooks: []claudeHookSpec{
			{event: "SessionStart", command: "/home/u/.periscope/periscope-ensure.sh"},
			{event: "Stop", command: "/bin/periscope hook stop"},
			{event: "UserPromptSubmit", command: "/bin/periscope hook display"},
		},
		statusLine: "/bin/periscope statusline",
	}
}

func TestMergeClaudeSettings_MissingFileWritesAllHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")

	res, err := mergeClaudeSettings(path, testDesired())
	if err != nil {
		t.Fatalf("mergeClaudeSettings: %v", err)
	}
	if len(res.added) != 4 {
		t.Errorf("added = %v, want 4 entries (3 hooks + statusLine)", res.added)
	}
	if len(res.existing) != 0 {
		t.Errorf("existing = %v, want none", res.existing)
	}
	if !res.changed() {
		t.Errorf("changed() = false, want true")
	}

	settings := readSettings(t, path)
	for _, spec := range testDesired().hooks {
		cmds := hooksFor(t, settings, spec.event)
		if len(cmds) != 1 || cmds[0] != spec.command {
			t.Errorf("%s hooks = %v, want [%s]", spec.event, cmds, spec.command)
		}
	}
	sl, ok := settings["statusLine"].(map[string]any)
	if !ok {
		t.Fatalf("statusLine = %v, want an object", settings["statusLine"])
	}
	if sl["command"] != "/bin/periscope statusline" || sl["type"] != "command" {
		t.Errorf("statusLine = %v, want type=command command=/bin/periscope statusline", sl)
	}
}

func TestMergeClaudeSettings_PreservesUnrelatedKeysAndHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := `{
  "permissions": {"allow": ["Bash(ls:*)"]},
  "theme": "dark",
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/other/tool guard"}]}
    ],
    "Stop": [
      {"hooks": [{"type": "command", "command": "/other/tool notify"}]}
    ]
  }
}`
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := mergeClaudeSettings(path, testDesired()); err != nil {
		t.Fatalf("mergeClaudeSettings: %v", err)
	}

	settings := readSettings(t, path)

	// Unrelated top-level keys survive untouched.
	if settings["theme"] != "dark" {
		t.Errorf("theme = %v, want dark", settings["theme"])
	}
	perms, ok := settings["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions lost: %v", settings["permissions"])
	}
	allow, _ := perms["allow"].([]any)
	if len(allow) != 1 || allow[0] != "Bash(ls:*)" {
		t.Errorf("permissions.allow = %v, want [Bash(ls:*)]", allow)
	}

	// Another tool's PreToolUse hook survives.
	if cmds := hooksFor(t, settings, "PreToolUse"); len(cmds) != 1 || cmds[0] != "/other/tool guard" {
		t.Errorf("PreToolUse = %v, want [/other/tool guard]", cmds)
	}

	// Another tool's Stop hook survives alongside ours.
	stop := hooksFor(t, settings, "Stop")
	if len(stop) != 2 {
		t.Fatalf("Stop = %v, want 2 commands", stop)
	}
	if stop[0] != "/other/tool notify" {
		t.Errorf("Stop[0] = %q, want the pre-existing /other/tool notify", stop[0])
	}
	if stop[1] != "/bin/periscope hook stop" {
		t.Errorf("Stop[1] = %q, want /bin/periscope hook stop", stop[1])
	}
}

func TestMergeClaudeSettings_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	first, err := mergeClaudeSettings(path, testDesired())
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}
	if len(first.added) != 4 {
		t.Fatalf("first merge added = %v, want 4", first.added)
	}
	afterFirst := mustReadFile(t, path)

	second, err := mergeClaudeSettings(path, testDesired())
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	if len(second.added) != 0 {
		t.Errorf("second merge added = %v, want none", second.added)
	}
	if len(second.existing) != 4 {
		t.Errorf("second merge existing = %v, want 4", second.existing)
	}
	if second.changed() {
		t.Errorf("second merge changed() = true, want false")
	}
	if got := mustReadFile(t, path); string(got) != string(afterFirst) {
		t.Errorf("second merge rewrote the file:\nbefore: %s\nafter:  %s", afterFirst, got)
	}

	settings := readSettings(t, path)
	for _, spec := range testDesired().hooks {
		if cmds := hooksFor(t, settings, spec.event); len(cmds) != 1 {
			t.Errorf("%s = %v, want exactly 1 entry (no duplicates)", spec.event, cmds)
		}
	}
}

func TestMergeClaudeSettings_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("   \n"), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := mergeClaudeSettings(path, testDesired())
	if err != nil {
		t.Fatalf("mergeClaudeSettings on empty file: %v", err)
	}
	if len(res.added) != 4 {
		t.Errorf("added = %v, want 4", res.added)
	}
	settings := readSettings(t, path)
	if cmds := hooksFor(t, settings, "Stop"); len(cmds) != 1 {
		t.Errorf("Stop = %v, want 1", cmds)
	}
}

func TestMergeClaudeSettings_MalformedFileIsNotClobbered(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	broken := `{"theme": "dark", "hooks": {`
	if err := os.WriteFile(path, []byte(broken), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := mergeClaudeSettings(path, testDesired()); err == nil {
		t.Fatalf("mergeClaudeSettings on malformed JSON = nil error, want an error")
	}
	if got := mustReadFile(t, path); string(got) != broken {
		t.Errorf("malformed file was modified: %s", got)
	}
}

func TestMergeClaudeSettings_ExistingStatusLinePreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"statusLine": {"type": "command", "command": "/other/statusline"}}`), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := mergeClaudeSettings(path, testDesired())
	if err != nil {
		t.Fatalf("mergeClaudeSettings: %v", err)
	}
	if len(res.skipped) != 1 {
		t.Errorf("skipped = %v, want 1 (foreign statusLine)", res.skipped)
	}

	settings := readSettings(t, path)
	sl, _ := settings["statusLine"].(map[string]any)
	if sl["command"] != "/other/statusline" {
		t.Errorf("statusLine = %v, want the user's own /other/statusline untouched", sl)
	}
	// Hooks still installed.
	if cmds := hooksFor(t, settings, "SessionStart"); len(cmds) != 1 {
		t.Errorf("SessionStart = %v, want 1", cmds)
	}
}

func TestMergeClaudeSettings_HooksKeyWrongTypeErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	bad := `{"hooks": "nope"}`
	if err := os.WriteFile(path, []byte(bad), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := mergeClaudeSettings(path, testDesired()); err == nil {
		t.Fatalf("mergeClaudeSettings with non-object hooks = nil error, want an error")
	}
	if got := mustReadFile(t, path); string(got) != bad {
		t.Errorf("file was modified: %s", got)
	}
}

func TestMergeClaudeSettings_PartiallyRegistered(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	// Stop already points at our command; the rest are missing.
	if err := os.WriteFile(path, []byte(`{
  "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "/bin/periscope hook stop"}]}]}
}`), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := mergeClaudeSettings(path, testDesired())
	if err != nil {
		t.Fatalf("mergeClaudeSettings: %v", err)
	}
	if len(res.added) != 3 {
		t.Errorf("added = %v, want 3 (SessionStart, UserPromptSubmit, statusLine)", res.added)
	}
	if len(res.existing) != 1 {
		t.Errorf("existing = %v, want 1 (Stop)", res.existing)
	}
	if cmds := hooksFor(t, readSettings(t, path), "Stop"); len(cmds) != 1 {
		t.Errorf("Stop = %v, want 1 (must not duplicate)", cmds)
	}
}

func TestMergeClaudeSettings_PreservesFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"theme":"dark"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := mergeClaudeSettings(path, testDesired()); err != nil {
		t.Fatalf("mergeClaudeSettings: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
}

// TestRegisterHooks_WritesSettings is the end-to-end guard: `periscope init`'s
// hook registration must actually write the hooks, not just print them.
func TestRegisterHooks_WritesSettings(t *testing.T) {
	home := t.TempDir()
	claude := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claude, 0755); err != nil {
		t.Fatal(err)
	}
	app := &App{
		HomeDir:   filepath.Join(home, ".periscope"),
		ClaudeDir: claude,
		Config:    AppConfig{Server: ServerConfig{Host: "localhost", Port: 7788}},
	}
	if err := os.MkdirAll(app.HomeDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := registerHooks(app); err != nil {
		t.Fatalf("registerHooks: %v", err)
	}

	settingsPath := filepath.Join(claude, "settings.json")
	settings := readSettings(t, settingsPath)
	for _, event := range []string{"SessionStart", "Stop", "UserPromptSubmit"} {
		if cmds := hooksFor(t, settings, event); len(cmds) != 1 {
			t.Errorf("%s = %v, want exactly 1 registered command", event, cmds)
		}
	}
	if _, ok := settings["statusLine"]; !ok {
		t.Errorf("statusLine not written")
	}

	// Second run must be a no-op, not a duplicator.
	before := mustReadFile(t, settingsPath)
	if err := registerHooks(app); err != nil {
		t.Fatalf("registerHooks (second run): %v", err)
	}
	if got := mustReadFile(t, settingsPath); string(got) != string(before) {
		t.Errorf("re-running registerHooks changed settings.json:\n%s", got)
	}
}

// A status line registered as a bare name on PATH is ours, not a stranger.
// Regression: init reported "statusLine already points elsewhere" against its
// own status line because it compared command strings literally.
func TestSameCommandMatchesBareNameOnPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "periscope")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	if !sameCommand("periscope statusline", bin+" statusline") {
		t.Errorf("bare name on PATH not recognised as the same binary")
	}
	if sameCommand("periscope statusline", bin+" substatusline") {
		t.Errorf("differing args must not match")
	}
	if sameCommand("some-other-tool statusline", bin+" statusline") {
		t.Errorf("a genuinely foreign command must not match")
	}
}

// The same identity comparison must apply to hooks, not just statusLine.
func TestMergeRecognisesBareNameHook(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "periscope")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	path := filepath.Join(dir, "settings.json")
	seed := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"periscope hook stop"}]}]}}`
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := mergeClaudeSettings(path, desiredClaudeSettings{
		hooks: []claudeHookSpec{{event: "Stop", command: bin + " hook stop"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.added) != 0 {
		t.Errorf("added = %v, want none — the hook is already registered", res.added)
	}
	if len(res.existing) != 1 || res.existing[0] != "Stop" {
		t.Errorf("existing = %v, want [Stop]", res.existing)
	}
}

// ── Defect: `periscope init` appended a duplicate hook ───────────────────────
//
// Reproduction of the live failure: settings.json already registered the Stop
// hook against the installed binary, and running a *different* periscope build
// (`/tmp/x/psc init`) appended a second Stop group instead of updating the
// first. Two Stop hooks means every turn writes the sidecar twice and cost
// accounting is counted twice — and it fails silently.
func TestMergeClaudeSettings_ReplacesStalePeriscopeHook(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	seed := `{
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "/home/progenyalpha/.local/bin/periscope hook stop"}]}]
  }
}`
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	want := desiredClaudeSettings{
		hooks: []claudeHookSpec{{event: "Stop", command: "/tmp/x/psc hook stop"}},
	}
	res, err := mergeClaudeSettings(path, want)
	if err != nil {
		t.Fatalf("mergeClaudeSettings: %v", err)
	}

	if len(res.added) != 0 {
		t.Errorf("added = %v, want none — a periscope Stop hook was already registered", res.added)
	}
	if len(res.updated) != 1 || res.updated[0] != "Stop" {
		t.Errorf("updated = %v, want [Stop]", res.updated)
	}

	cmds := hooksFor(t, readSettings(t, path), "Stop")
	if len(cmds) != 1 {
		t.Fatalf("Stop = %v, want exactly 1 hook (the duplicate is the defect)", cmds)
	}
	if cmds[0] != "/tmp/x/psc hook stop" {
		t.Errorf("Stop[0] = %q, want it repointed at /tmp/x/psc hook stop", cmds[0])
	}
}

// Repairing the already-corrupted state: several periscope Stop hooks must
// collapse to exactly one, and another tool's hook must survive in place.
//
// The renamed build (/tmp/x/psc) is recognised as ours because it is the binary
// running init; the install-path one is recognised by name. Re-running init
// from either of the two duplicated binaries therefore repairs the file.
func TestMergeClaudeSettings_CollapsesDuplicatePeriscopeHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	seed := `{
  "hooks": {
    "Stop": [
      {"hooks": [{"type": "command", "command": "/other/tool notify"}]},
      {"hooks": [{"type": "command", "command": "/home/progenyalpha/.local/bin/periscope hook stop"}]},
      {"hooks": [{"type": "command", "command": "/tmp/x/psc hook stop"}]}
    ]
  }
}`
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := mergeClaudeSettings(path, desiredClaudeSettings{
		hooks: []claudeHookSpec{{event: "Stop", command: "/tmp/x/psc hook stop"}},
	})
	if err != nil {
		t.Fatalf("mergeClaudeSettings: %v", err)
	}
	if len(res.updated) != 1 || res.updated[0] != "Stop" {
		t.Errorf("updated = %v, want [Stop]", res.updated)
	}

	cmds := hooksFor(t, readSettings(t, path), "Stop")
	want := []string{"/other/tool notify", "/tmp/x/psc hook stop"}
	if len(cmds) != len(want) {
		t.Fatalf("Stop = %v, want %v", cmds, want)
	}
	for i := range want {
		if cmds[i] != want[i] {
			t.Errorf("Stop[%d] = %q, want %q", i, cmds[i], want[i])
		}
	}
}

// The same collapse, from a third binary: two entries that both *look* like
// periscope are both ours no matter which binary is doing the installing.
func TestMergeClaudeSettings_CollapsesDuplicatesFromAThirdBinary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	seed := `{
  "hooks": {
    "Stop": [
      {"hooks": [
        {"type": "command", "command": "/home/u/.local/bin/periscope hook stop"},
        {"type": "command", "command": "/other/tool notify"}
      ]},
      {"hooks": [{"type": "command", "command": "/opt/periscope-dev/periscope hook stop"}]}
    ]
  }
}`
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := mergeClaudeSettings(path, desiredClaudeSettings{
		hooks: []claudeHookSpec{{event: "Stop", command: "/usr/local/bin/periscope hook stop"}},
	}); err != nil {
		t.Fatalf("mergeClaudeSettings: %v", err)
	}

	cmds := hooksFor(t, readSettings(t, path), "Stop")
	want := []string{"/usr/local/bin/periscope hook stop", "/other/tool notify"}
	if len(cmds) != len(want) {
		t.Fatalf("Stop = %v, want %v", cmds, want)
	}
	for i := range want {
		if cmds[i] != want[i] {
			t.Errorf("Stop[%d] = %q, want %q", i, cmds[i], want[i])
		}
	}
}

// A foreign hook sharing a group with ours must be left byte-for-byte alone,
// and the group's unknown fields (matcher, timeout, ...) must survive.
func TestMergeClaudeSettings_UpdatePreservesForeignEntriesAndFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	seed := `{
  "hooks": {
    "Stop": [
      {"matcher": "*", "timeout": 30, "hooks": [
        {"type": "command", "command": "/other/tool notify"},
        {"type": "command", "command": "/old/periscope hook stop", "timeout": 5}
      ]}
    ]
  }
}`
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := mergeClaudeSettings(path, desiredClaudeSettings{
		hooks: []claudeHookSpec{{event: "Stop", command: "/new/periscope hook stop"}},
	}); err != nil {
		t.Fatalf("mergeClaudeSettings: %v", err)
	}

	settings := readSettings(t, path)
	cmds := hooksFor(t, settings, "Stop")
	want := []string{"/other/tool notify", "/new/periscope hook stop"}
	if len(cmds) != 2 || cmds[0] != want[0] || cmds[1] != want[1] {
		t.Fatalf("Stop = %v, want %v", cmds, want)
	}

	groups := settings["hooks"].(map[string]any)["Stop"].([]any)
	g := groups[0].(map[string]any)
	if g["matcher"] != "*" {
		t.Errorf("group matcher = %v, want * (unknown fields must survive)", g["matcher"])
	}
	if g["timeout"] != float64(30) {
		t.Errorf("group timeout = %v, want 30", g["timeout"])
	}
	entry := g["hooks"].([]any)[1].(map[string]any)
	if entry["timeout"] != float64(5) {
		t.Errorf("rewritten entry lost its timeout: %v", entry)
	}
}

// After repointing a stale hook, a second run must be a pure no-op.
func TestMergeClaudeSettings_IdempotentAfterUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	seed := `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "/old/periscope hook stop"}]}]}}`
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}
	want := desiredClaudeSettings{
		hooks: []claudeHookSpec{{event: "Stop", command: "/new/periscope hook stop"}},
	}

	if _, err := mergeClaudeSettings(path, want); err != nil {
		t.Fatalf("first merge: %v", err)
	}
	afterFirst := mustReadFile(t, path)

	second, err := mergeClaudeSettings(path, want)
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	if len(second.added) != 0 || len(second.updated) != 0 {
		t.Errorf("second merge added=%v updated=%v, want both empty", second.added, second.updated)
	}
	if len(second.existing) != 1 || second.existing[0] != "Stop" {
		t.Errorf("second merge existing = %v, want [Stop]", second.existing)
	}
	if second.changed() {
		t.Errorf("second merge changed() = true, want false")
	}
	if got := mustReadFile(t, path); string(got) != string(afterFirst) {
		t.Errorf("second merge rewrote the file:\nbefore: %s\nafter:  %s", afterFirst, got)
	}
}

// The SessionStart entry is the launcher script, not the binary; a stale one
// from a previous install location must be repointed, not duplicated.
func TestMergeClaudeSettings_ReplacesStaleLauncherScript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	seed := `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "/old/home/.periscope/periscope-ensure.sh"}]}]}}`
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := mergeClaudeSettings(path, desiredClaudeSettings{
		hooks: []claudeHookSpec{{event: "SessionStart", command: "/new/home/.periscope/periscope-ensure.sh"}},
	})
	if err != nil {
		t.Fatalf("mergeClaudeSettings: %v", err)
	}
	if len(res.updated) != 1 || len(res.added) != 0 {
		t.Errorf("updated=%v added=%v, want updated=[SessionStart] added=[]", res.updated, res.added)
	}
	cmds := hooksFor(t, readSettings(t, path), "SessionStart")
	if len(cmds) != 1 || cmds[0] != "/new/home/.periscope/periscope-ensure.sh" {
		t.Errorf("SessionStart = %v, want the single new launcher path", cmds)
	}
}

// A statusLine pointing at a stale periscope binary is ours to repoint. A
// statusLine owned by another tool still must never be touched (covered by
// TestMergeClaudeSettings_ExistingStatusLinePreserved).
func TestMergeClaudeSettings_RepointsStalePeriscopeStatusLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	seed := `{"statusLine": {"type": "command", "command": "/old/periscope statusline", "padding": 0}}`
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := mergeClaudeSettings(path, desiredClaudeSettings{statusLine: "/new/periscope statusline"})
	if err != nil {
		t.Fatalf("mergeClaudeSettings: %v", err)
	}
	if len(res.skipped) != 0 {
		t.Errorf("skipped = %v, want none — that status line is ours", res.skipped)
	}
	if len(res.updated) != 1 || res.updated[0] != "statusLine" {
		t.Errorf("updated = %v, want [statusLine]", res.updated)
	}

	sl := readSettings(t, path)["statusLine"].(map[string]any)
	if sl["command"] != "/new/periscope statusline" {
		t.Errorf("statusLine command = %v, want /new/periscope statusline", sl["command"])
	}
	if sl["padding"] != float64(0) {
		t.Errorf("statusLine lost its padding field: %v", sl)
	}
}

// registerHooks is what `periscope init` actually runs: a second init from a
// different binary path must repoint every entry, never append.
func TestRegisterHooks_SecondBinaryDoesNotDuplicate(t *testing.T) {
	home := t.TempDir()
	claude := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claude, 0755); err != nil {
		t.Fatal(err)
	}
	app := &App{
		HomeDir:   filepath.Join(home, ".periscope"),
		ClaudeDir: claude,
		Config:    AppConfig{Server: ServerConfig{Host: "localhost", Port: 7788}},
	}
	if err := os.MkdirAll(app.HomeDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := registerHooks(app); err != nil {
		t.Fatalf("registerHooks: %v", err)
	}

	// Simulate the hooks having been registered by a periscope binary that
	// lives somewhere else, exactly as the live failure did.
	settingsPath := filepath.Join(claude, claudeSettingsName)
	raw := mustReadFile(t, settingsPath)
	stale := strings.ReplaceAll(string(raw), periscopeBinary(), "/home/other/.local/bin/periscope")
	if stale == string(raw) {
		t.Fatalf("test setup did not rewrite the binary path in %s", settingsPath)
	}
	if err := os.WriteFile(settingsPath, []byte(stale), 0644); err != nil {
		t.Fatal(err)
	}

	if err := registerHooks(app); err != nil {
		t.Fatalf("registerHooks (second binary): %v", err)
	}
	settings := readSettings(t, settingsPath)
	for _, event := range []string{"SessionStart", "Stop", "UserPromptSubmit"} {
		if cmds := hooksFor(t, settings, event); len(cmds) != 1 {
			t.Errorf("%s = %v, want exactly 1 registered command", event, cmds)
		}
	}
}
