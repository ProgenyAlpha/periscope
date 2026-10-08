package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeRemoveSettings(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readRemoveSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("settings no longer parse: %v\n%s", err, raw)
	}
	return out
}

func uninstallWant() desiredClaudeSettings {
	return desiredClaudeSettings{
		hooks: []claudeHookSpec{
			{event: "Stop", command: "/opt/periscope hook stop"},
			{event: "UserPromptSubmit", command: "/opt/periscope hook display"},
		},
		statusLine: "/opt/periscope statusline",
	}
}

// A hook registered by a periscope living somewhere else is still ours, and
// uninstall has to take it out or it keeps firing a binary that is gone.
func TestRemoveClaudeSettingsRemovesOurEntries(t *testing.T) {
	path := writeRemoveSettings(t, `{
	  "theme": "dark",
	  "statusLine": {"type": "command", "command": "/home/u/.local/bin/periscope statusline"},
	  "hooks": {
	    "Stop": [{"hooks": [{"type": "command", "command": "/home/u/.local/bin/periscope hook stop"}]}],
	    "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "/opt/periscope hook display"}]}]
	  }
	}`)

	res, err := removeClaudeSettings(path, uninstallWant())
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(res.removed) != 3 {
		t.Fatalf("removed %v, want Stop, UserPromptSubmit and statusLine", res.removed)
	}

	got := readRemoveSettings(t, path)
	if _, ok := got["statusLine"]; ok {
		t.Error("statusLine survived the uninstall")
	}
	if _, ok := got["hooks"]; ok {
		t.Errorf("hooks survived the uninstall: %v", got["hooks"])
	}
	if got["theme"] != "dark" {
		t.Errorf("theme = %v, want the untouched user setting", got["theme"])
	}
}

// settings.json belongs to the user. Another tool's hooks, and another tool's
// status line, have to come out the far side unchanged.
func TestRemoveClaudeSettingsLeavesForeignEntries(t *testing.T) {
	path := writeRemoveSettings(t, `{
	  "statusLine": {"type": "command", "command": "starship prompt"},
	  "hooks": {
	    "Stop": [
	      {"hooks": [{"type": "command", "command": "/opt/periscope hook stop"}]},
	      {"matcher": "*", "hooks": [{"type": "command", "command": "/usr/bin/notify-send done"}]}
	    ],
	    "PreToolUse": [{"hooks": [{"type": "command", "command": "/usr/bin/audit"}]}]
	  }
	}`)

	res, err := removeClaudeSettings(path, uninstallWant())
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(res.skipped) != 1 || res.skipped[0] != "statusLine" {
		t.Errorf("skipped = %v, want the foreign statusLine reported", res.skipped)
	}

	got := readRemoveSettings(t, path)
	sl, _ := got["statusLine"].(map[string]any)
	if sl["command"] != "starship prompt" {
		t.Errorf("clobbered another tool's status line: %v", got["statusLine"])
	}
	hooks, _ := got["hooks"].(map[string]any)
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Error("dropped an unrelated hook event")
	}
	stop, _ := hooks["Stop"].([]any)
	if len(stop) != 1 {
		t.Fatalf("Stop groups = %d, want only the foreign one left", len(stop))
	}
	group, _ := stop[0].(map[string]any)
	if group["matcher"] != "*" {
		t.Errorf("kept the wrong Stop group: %v", group)
	}
}

// Removing what is not there is a no-op, not an error: uninstall is re-runnable
// and must not rewrite a settings file it has nothing to change.
func TestRemoveClaudeSettingsIsIdempotent(t *testing.T) {
	path := writeRemoveSettings(t, `{"theme": "dark"}`)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	res, err := removeClaudeSettings(path, uninstallWant())
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(res.removed) != 0 {
		t.Errorf("removed = %v, want nothing", res.removed)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("rewrote a settings file it had nothing to change")
	}
}

// A settings file we cannot parse is reported, never overwritten.
func TestRemoveClaudeSettingsRefusesUnparseableFile(t *testing.T) {
	const body = `{"hooks": {broken`
	path := writeRemoveSettings(t, body)

	if _, err := removeClaudeSettings(path, uninstallWant()); err == nil {
		t.Fatal("expected an error for an unparseable settings file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != body {
		t.Errorf("file was modified: %q", raw)
	}
}
