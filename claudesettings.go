package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Merging Periscope's hooks into ~/.claude/settings.json.
//
// settings.json belongs to the user, not to us: it holds permissions, theme,
// plugin state and hooks registered by other tools. So every write here is a
// read-modify-write that keeps unknown keys byte-for-byte identical, appends
// rather than replaces inside a hook event, and refuses to touch a file it
// cannot parse.
//
// Claude's hook schema:
//
//	{"hooks": {"Stop": [ {"matcher": "...", "hooks": [{"type":"command","command":"..."}]} ]}}

const claudeSettingsName = "settings.json"

// claudeHookSpec is one hook Periscope wants registered.
type claudeHookSpec struct {
	event   string // SessionStart, Stop, UserPromptSubmit, ...
	command string
}

// desiredClaudeSettings is everything `periscope init` wants present in
// settings.json. An empty statusLine means "don't manage the status line".
type desiredClaudeSettings struct {
	hooks      []claudeHookSpec
	statusLine string
}

// settingsMergeResult reports what the merge actually did, so callers can log
// the truth instead of an unconditional "registered".
type settingsMergeResult struct {
	path     string
	added    []string // newly written (e.g. "Stop", "statusLine")
	existing []string // already pointed at our command
	skipped  []string // present but owned by something else; left alone
}

func (r settingsMergeResult) changed() bool { return len(r.added) > 0 }

// hookEntry is the leaf of the hook schema. Groups are kept as RawMessage so
// that fields we don't know about (matcher, timeout, ...) survive a rewrite.
type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

type hookGroup struct {
	Hooks []hookEntry `json:"hooks"`
}

// mergeClaudeSettings idempotently merges want into the settings file at path.
// A missing file (or a blank one) is created; a file that does not parse as a
// JSON object is left untouched and reported as an error.
func mergeClaudeSettings(path string, want desiredClaudeSettings) (settingsMergeResult, error) {
	res := settingsMergeResult{path: path}

	raw, mode, err := readSettingsFile(path)
	if err != nil {
		return res, err
	}

	// Top level stays as RawMessage: keys we don't touch are re-emitted verbatim.
	settings := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return res, fmt.Errorf("parse %s (leaving it untouched): %w", path, err)
		}
	}

	hooks := map[string]json.RawMessage{}
	if rawHooks, ok := settings["hooks"]; ok && len(bytes.TrimSpace(rawHooks)) > 0 &&
		!bytes.Equal(bytes.TrimSpace(rawHooks), []byte("null")) {
		if err := json.Unmarshal(rawHooks, &hooks); err != nil {
			return res, fmt.Errorf(`parse "hooks" in %s (leaving it untouched): %w`, path, err)
		}
	}

	for _, spec := range want.hooks {
		var groups []json.RawMessage
		if rawEvent, ok := hooks[spec.event]; ok && len(bytes.TrimSpace(rawEvent)) > 0 &&
			!bytes.Equal(bytes.TrimSpace(rawEvent), []byte("null")) {
			if err := json.Unmarshal(rawEvent, &groups); err != nil {
				return res, fmt.Errorf(`parse hooks.%s in %s (leaving it untouched): %w`, spec.event, path, err)
			}
		}

		if hookGroupsContain(groups, spec.command) {
			res.existing = append(res.existing, spec.event)
			continue
		}

		group, err := json.Marshal(hookGroup{Hooks: []hookEntry{{Type: "command", Command: spec.command}}})
		if err != nil {
			return res, fmt.Errorf("encode %s hook: %w", spec.event, err)
		}
		groups = append(groups, group)

		encoded, err := json.Marshal(groups)
		if err != nil {
			return res, fmt.Errorf("encode hooks.%s: %w", spec.event, err)
		}
		hooks[spec.event] = encoded
		res.added = append(res.added, spec.event)
	}

	if len(hooks) > 0 {
		encoded, err := json.Marshal(hooks)
		if err != nil {
			return res, fmt.Errorf("encode hooks: %w", err)
		}
		settings["hooks"] = encoded
	}

	if want.statusLine != "" {
		switch cur, ok := settings["statusLine"]; {
		case !ok || len(bytes.TrimSpace(cur)) == 0 || bytes.Equal(bytes.TrimSpace(cur), []byte("null")):
			encoded, err := json.Marshal(hookEntry{Type: "command", Command: want.statusLine})
			if err != nil {
				return res, fmt.Errorf("encode statusLine: %w", err)
			}
			settings["statusLine"] = encoded
			res.added = append(res.added, "statusLine")
		default:
			var entry hookEntry
			if json.Unmarshal(cur, &entry) == nil && entry.Command == want.statusLine {
				res.existing = append(res.existing, "statusLine")
			} else {
				// Someone else's status line. Never overwrite it.
				res.skipped = append(res.skipped, "statusLine")
			}
		}
	}

	if !res.changed() {
		return res, nil
	}
	if err := writeSettingsFile(path, settings, mode); err != nil {
		return res, err
	}
	return res, nil
}

// hookGroupsContain reports whether any entry in any group already runs command.
// Groups that don't parse are treated as foreign and skipped, never dropped.
func hookGroupsContain(groups []json.RawMessage, command string) bool {
	for _, raw := range groups {
		var g hookGroup
		if json.Unmarshal(raw, &g) != nil {
			continue
		}
		for _, e := range g.Hooks {
			if e.Command == command {
				return true
			}
		}
	}
	return false
}

// readSettingsFile returns the file's contents and the mode to preserve on
// rewrite. A missing file yields empty contents and the default mode.
func readSettingsFile(path string) ([]byte, os.FileMode, error) {
	const defaultMode os.FileMode = 0644
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, defaultMode, nil
	}
	if err != nil {
		return nil, defaultMode, fmt.Errorf("read %s: %w", path, err)
	}
	mode := defaultMode
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	return raw, mode, nil
}

// writeSettingsFile writes settings atomically (temp file + rename) so a crash
// mid-write cannot leave the user without a settings.json.
func writeSettingsFile(path string, settings map[string]json.RawMessage, mode os.FileMode) error {
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	out = append(out, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.json")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename onto %s: %w", path, err)
	}
	return nil
}
