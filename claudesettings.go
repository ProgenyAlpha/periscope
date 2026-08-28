package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	updated  []string // a stale periscope entry repointed at this binary
	existing []string // already pointed at our command
	skipped  []string // present but owned by something else; left alone
}

func (r settingsMergeResult) changed() bool { return len(r.added)+len(r.updated) > 0 }

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

		groups, outcome, err := reconcileHookGroups(groups, spec.command)
		if err != nil {
			return res, fmt.Errorf("rewrite hooks.%s in %s: %w", spec.event, path, err)
		}

		switch outcome {
		case hookUnchanged:
			res.existing = append(res.existing, spec.event)
			continue
		case hookRepointed:
			res.updated = append(res.updated, spec.event)
		case hookAbsent:
			group, err := json.Marshal(hookGroup{Hooks: []hookEntry{{Type: "command", Command: spec.command}}})
			if err != nil {
				return res, fmt.Errorf("encode %s hook: %w", spec.event, err)
			}
			groups = append(groups, group)
			res.added = append(res.added, spec.event)
		}

		encoded, err := json.Marshal(groups)
		if err != nil {
			return res, fmt.Errorf("encode hooks.%s: %w", spec.event, err)
		}
		hooks[spec.event] = encoded
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
			encoded, outcome, err := reconcileStatusLine(cur, want.statusLine)
			if err != nil {
				return res, fmt.Errorf("rewrite statusLine in %s: %w", path, err)
			}
			switch outcome {
			case hookUnchanged:
				res.existing = append(res.existing, "statusLine")
			case hookRepointed:
				settings["statusLine"] = encoded
				res.updated = append(res.updated, "statusLine")
			default:
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

// hookMergeOutcome is what reconcileHookGroups found for one event.
type hookMergeOutcome int

const (
	hookAbsent    hookMergeOutcome = iota // no periscope entry — the caller appends one
	hookUnchanged                         // exactly one periscope entry, already correct
	hookRepointed                         // a stale/duplicated periscope entry was rewritten
)

// reconcileHookGroups makes the event's groups hold exactly one Periscope entry
// for this subcommand, pointing at this binary, and returns the rewritten groups.
//
// Deduping by command identity is not enough. A second periscope build is a
// genuinely different file, so `psc init` used to append a second `Stop` group
// beside the one the installed binary had registered — and Claude then ran the
// Stop hook twice per turn, double-writing sidecars and double-counting cost,
// silently. Ownership, not file identity, is the question to ask: an entry
// whose program looks like periscope (looksLikePeriscope) and whose arguments
// are the ones we register is ours to repoint, wherever it points now.
//
// Everything else is left exactly as found — foreign tools' entries, groups
// that do not parse, and unknown fields on the groups and entries we do rewrite
// (matcher, timeout, ...). A hook registered under a name that does not look
// like periscope and is not this very file cannot be recognised as ours and
// will be appended beside; that is the safe direction to fail.
func reconcileHookGroups(groups []json.RawMessage, command string) ([]json.RawMessage, hookMergeOutcome, error) {
	wantArgs := hookArgs(command)
	found, rewrote := false, false
	out := make([]json.RawMessage, 0, len(groups))

	for _, rawGroup := range groups {
		var group map[string]json.RawMessage
		if json.Unmarshal(rawGroup, &group) != nil {
			out = append(out, rawGroup) // not a group we understand: never touch it
			continue
		}
		var entries []json.RawMessage
		if rawEntries, ok := group["hooks"]; !ok || json.Unmarshal(rawEntries, &entries) != nil {
			out = append(out, rawGroup)
			continue
		}

		kept := make([]json.RawMessage, 0, len(entries))
		groupChanged := false
		for _, rawEntry := range entries {
			var entry map[string]json.RawMessage
			var cmd string
			if json.Unmarshal(rawEntry, &entry) != nil ||
				json.Unmarshal(entry["command"], &cmd) != nil ||
				!ownedByPeriscope(cmd, command, wantArgs) {
				kept = append(kept, rawEntry)
				continue
			}
			if found { // a second periscope entry: the corrupted state, collapsed
				groupChanged = true
				continue
			}
			found = true
			if sameCommand(cmd, command) {
				// Already this binary — a bare name on PATH or a symlink is a
				// deliberate registration, not something to hard-code away.
				kept = append(kept, rawEntry)
				continue
			}
			encoded, err := json.Marshal(command)
			if err != nil {
				return nil, hookAbsent, fmt.Errorf("encode command: %w", err)
			}
			entry["command"] = encoded
			rewritten, err := json.Marshal(entry)
			if err != nil {
				return nil, hookAbsent, fmt.Errorf("encode hook entry: %w", err)
			}
			kept = append(kept, rewritten)
			groupChanged = true
		}

		if !groupChanged {
			out = append(out, rawGroup)
			continue
		}
		rewrote = true
		if len(kept) == 0 {
			continue // the group held nothing but a duplicate of ours
		}
		encodedEntries, err := json.Marshal(kept)
		if err != nil {
			return nil, hookAbsent, fmt.Errorf("encode hook entries: %w", err)
		}
		group["hooks"] = encodedEntries
		rewritten, err := json.Marshal(group)
		if err != nil {
			return nil, hookAbsent, fmt.Errorf("encode hook group: %w", err)
		}
		out = append(out, rewritten)
	}

	switch {
	case rewrote:
		return out, hookRepointed, nil
	case found:
		return out, hookUnchanged, nil
	default:
		return out, hookAbsent, nil
	}
}

// reconcileStatusLine applies the same ownership rule to the statusLine object,
// preserving any fields on it we do not know about.
func reconcileStatusLine(cur json.RawMessage, want string) (json.RawMessage, hookMergeOutcome, error) {
	var entry map[string]json.RawMessage
	var cmd string
	if json.Unmarshal(cur, &entry) != nil || json.Unmarshal(entry["command"], &cmd) != nil {
		return nil, hookAbsent, nil
	}
	if sameCommand(cmd, want) {
		return cur, hookUnchanged, nil
	}
	if !ownedByPeriscope(cmd, want, hookArgs(want)) {
		return nil, hookAbsent, nil
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		return nil, hookAbsent, fmt.Errorf("encode command: %w", err)
	}
	entry["command"] = encoded
	rewritten, err := json.Marshal(entry)
	if err != nil {
		return nil, hookAbsent, fmt.Errorf("encode statusLine: %w", err)
	}
	return rewritten, hookRepointed, nil
}

// ownedByPeriscope reports whether a registered command line is one of ours:
// either literally this binary (a hard link or a bare name on PATH included),
// or some other periscope build invoked with the same subcommand.
func ownedByPeriscope(registered, want, wantArgs string) bool {
	if sameCommand(registered, want) {
		return true
	}
	return looksLikePeriscope(hookTarget(registered)) && hookArgs(registered) == wantArgs
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

// sameCommand reports whether two hook command lines invoke the same program
// with the same arguments.
//
// A command registered as a bare name ("periscope statusline") and the same
// command written absolutely ("/home/u/.local/bin/periscope statusline") are
// the same thing, but compare unequal as strings — which made init report our
// own status line as "owned by another tool" and refuse to manage it. Resolve
// the executable the way a shell would and compare it by identity, so a bare
// name on PATH or a symlink is recognised rather than treated as a stranger.
func sameCommand(a, b string) bool {
	if a == b {
		return true
	}
	fa, fb := strings.Fields(a), strings.Fields(b)
	if len(fa) != len(fb) || len(fa) == 0 {
		return false
	}
	for i := 1; i < len(fa); i++ {
		if fa[i] != fb[i] {
			return false
		}
	}
	ra, err := resolveHookTarget(fa[0])
	if err != nil {
		return false
	}
	rb, err := resolveHookTarget(fb[0])
	if err != nil {
		return false
	}
	return sameBinary(ra, rb)
}
