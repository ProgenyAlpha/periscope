package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// doctor.go — one command that answers "is telemetry actually flowing?"
//
// This exists because of a five-day silent outage. `periscope init` printed its
// hook commands instead of writing them into ~/.claude/settings.json, so the
// Stop hook never ran, no sidecar was ever written, ImportSidecars reported
// count=0 as success, and the sessions table stopped updating. Nothing in the
// dashboard changed: the server was up, /api/health returned 200, and
// limit_history kept polling every 60s because that path needs no hooks at all.
// The only way to see the gap was to open SQLite by hand.
//
// So doctor deliberately checks the *chain* — hook registered → sidecar written
// → row imported → server serving it — instead of asking each link whether it
// is alive. A link that is up but has moved no data is exactly the failure that
// hid for five days.
//
// doctor is read-only. It opens the database read-only, never rewrites
// settings.json, and never restarts anything.

// ── Thresholds ──────────────────────────────────────────────────────────────

const (
	// sidecarWarnAge / sidecarFailAge bound how long the newest session sidecar
	// may be. A sidecar is written by the Stop hook at the end of every
	// assistant turn, so an actively-used machine writes many per day. Six
	// hours is a long lunch; twenty-four hours is either a genuine day off or a
	// dead hook, and the cost of being wrong about a day off (one noisy line in
	// a cron log) is nothing against five days of silent data loss.
	sidecarWarnAge = 6 * time.Hour
	sidecarFailAge = 24 * time.Hour

	// hookLagLimit is the sharp version of the same check, and the one that
	// would have caught the outage on day one rather than day two. Claude
	// appends to ~/.claude/projects/**/*.jsonl whether or not any hook is
	// registered, so a transcript much newer than the newest sidecar proves
	// Claude ran and the Stop hook did not. The hour of slack is for a single
	// long-running turn, which streams to the transcript throughout but only
	// writes its sidecar when it stops.
	hookLagLimit = time.Hour

	// ingestLagWarn / ingestLagFail bound how far sessions.updated_at may trail
	// the sidecar files it is built from. The fsnotify watcher re-imports
	// within 500ms (watcher.go), and the poll loop re-imports every cycle
	// regardless of what else it does (server.go). That cycle backs off to at
	// most 60 minutes when the usage API rate-limits us, so 90 minutes is past
	// anything normal operation can explain.
	ingestLagWarn = 15 * time.Minute
	ingestLagFail = 90 * time.Minute

	// walWarnBytes mirrors store's journal_size_limit and walFailBytes its
	// forced-checkpoint threshold: past the first, checkpoints are not
	// truncating; past the second, the maintenance pass is not running at all.
	walWarnBytes = 4 << 20
	walFailBytes = 8 << 20

	// logWarnBytes is the rotation threshold itself (logging.go). A log sitting
	// above it means rotation is failing, and four times over means it has been
	// failing for a while — this machine reached 21 MB.
	logWarnBytes = defaultMaxLogBytes
	logFailBytes = 4 * defaultMaxLogBytes

	// diskWarnBytes / diskFailBytes: the database is ~13 MB and its WAL can
	// reach 8 MB, but the real risk is the hooks. A sidecar write that fails
	// for ENOSPC is as silent as a hook that never runs.
	diskWarnBytes = 1 << 30
	diskFailBytes = 100 << 20

	// diskWarnPercent catches a large disk that is nonetheless nearly full.
	diskWarnPercent = 5

	// expectedSchemaVersion tracks currentSchemaVersion in
	// internal/store/db.go, which is unexported. If a migration is added there,
	// bump this too.
	expectedSchemaVersion = 4
)

// Check names. Constants so the printer, the tests and the summary cannot drift.
const (
	checkHooks      = "claude hooks"
	checkStatusLine = "claude statusline"
	checkSidecars   = "sidecar freshness"
	checkIngest     = "ingestion freshness"
	checkServer     = "server reachable"
	checkDBFile     = "database file"
	checkDBSchema   = "database schema"
	checkDBWAL      = "database wal"
	checkLogFile    = "log file"
	checkLogLevel   = "log level"
	checkDiskHome   = "disk (periscope)"
	checkDiskData   = "disk (cost-state)"
)

// ── Result model ────────────────────────────────────────────────────────────

type checkStatus string

const (
	ckOK   checkStatus = "OK"
	ckWarn checkStatus = "WARN"
	ckFail checkStatus = "FAIL"
)

// checkResult is one line of the report. Remedy is mandatory for anything that
// is not OK: a diagnostic that says "broken" without saying "run this" just
// moves the investigation somewhere else.
type checkResult struct {
	Name   string
	Status checkStatus
	Detail string
	Remedy string
}

// doctorEnv is everything the checks read. It is a value, not global state, so
// every test can point it at t.TempDir() and the live machine is never touched.
type doctorEnv struct {
	Now       time.Time
	HomeDir   string // ~/.periscope
	ClaudeDir string // ~/.claude
	DataDir   string // ~/.claude/hooks/cost-state
	Binary    string // the periscope executable running this command
	Config    AppConfig

	// probeHealth is injected so tests never open a socket.
	probeHealth func(url string) (int, error)
}

func (e doctorEnv) dbPath() string       { return filepath.Join(e.HomeDir, "periscope.db") }
func (e doctorEnv) walPath() string      { return e.dbPath() + "-wal" }
func (e doctorEnv) logPath() string      { return filepath.Join(e.HomeDir, "periscope.log") }
func (e doctorEnv) settingsPath() string { return filepath.Join(e.ClaudeDir, claudeSettingsName) }

func (e doctorEnv) healthURL() string {
	return fmt.Sprintf("http://%s:%d/api/health", e.Config.Server.Host, e.Config.Server.Port)
}

// ── Command ─────────────────────────────────────────────────────────────────

func cmdDoctor() {
	app, err := newApp()
	if err != nil {
		slog.Error("doctor failed", "err", err)
		os.Exit(1)
	}

	env := doctorEnv{
		Now:         time.Now(),
		HomeDir:     app.HomeDir,
		ClaudeDir:   app.ClaudeDir,
		DataDir:     app.DataDir,
		Binary:      periscopeBinary(),
		Config:      app.Config,
		probeHealth: httpProbeHealth,
	}

	iBanner()
	fmt.Printf("  %sDiagnostics%s  %s\n\n", cBold, cReset, env.HomeDir)

	results := runDoctorChecks(env)
	printDoctorResults(results)

	iDivider()
	summary := doctorSummary(results)
	code := doctorExitCode(results)
	if code == 0 {
		fmt.Printf("\n  %s%sHEALTHY%s  %s\n\n", cBold, cGreen, cReset, summary)
	} else {
		fmt.Printf("\n  %s%sUNHEALTHY%s  %s\n\n", cBold, cRed, cReset, summary)
	}
	slog.Info("doctor complete", "summary", summary, "exit", code)
	os.Exit(code)
}

// runDoctorChecks runs every check in chain order: registration, then the file
// the registration produces, then the row that file becomes, then the server
// that serves the row, then the resources all of it needs.
func runDoctorChecks(env doctorEnv) []checkResult {
	var out []checkResult
	out = append(out, checkClaudeHooks(env)...)
	out = append(out, checkSidecarFreshness(env))
	out = append(out, checkIngestion(env))
	out = append(out, checkServerReachable(env))
	out = append(out, checkDatabase(env)...)
	out = append(out, checkLogHealth(env)...)
	out = append(out, checkDiskSpace(env)...)
	return out
}

func doctorExitCode(results []checkResult) int {
	for _, r := range results {
		if r.Status == ckFail {
			return 1
		}
	}
	return 0
}

func doctorSummary(results []checkResult) string {
	var ok, warn, fail int
	for _, r := range results {
		switch r.Status {
		case ckOK:
			ok++
		case ckWarn:
			warn++
		case ckFail:
			fail++
		}
	}
	return fmt.Sprintf("%d checks — %d ok, %s, %s",
		len(results), ok, plural(warn, "warning"), plural(fail, "failure"))
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func printDoctorResults(results []checkResult) {
	width := 0
	for _, r := range results {
		if len(r.Name) > width {
			width = len(r.Name)
		}
	}
	for _, r := range results {
		var tag string
		switch r.Status {
		case ckOK:
			tag = cGreen + "[OK]" + cReset
		case ckWarn:
			tag = cYellow + "[!!]" + cReset
		default:
			tag = cRed + "[XX]" + cReset
		}
		fmt.Printf("  %s  %-*s  %s\n", tag, width, r.Name, r.Detail)
		if r.Status != ckOK && r.Remedy != "" {
			fmt.Printf("        %*s  %s→ %s%s\n", width, "", cDim, r.Remedy, cReset)
		}
	}
}

func httpProbeHealth(url string) (int, error) {
	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// ── 1. Claude hooks ─────────────────────────────────────────────────────────

// doctorLauncherName mirrors the launcher filename chosen in registerHooks
// (installer.go). Kept in step by name, not by import, because registerHooks
// builds it inline while writing the file.
func doctorLauncherName() string {
	if runtime.GOOS == "windows" {
		return "periscope-ensure.ps1"
	}
	return "periscope-ensure.sh"
}

// desiredDoctorSettings is the same desiredClaudeSettings that registerHooks
// merges, rebuilt from the environment so doctor asks for exactly what init
// writes rather than a second opinion about it.
func desiredDoctorSettings(env doctorEnv) desiredClaudeSettings {
	return desiredClaudeSettings{
		hooks: []claudeHookSpec{
			{event: "SessionStart", command: filepath.Join(env.HomeDir, doctorLauncherName())},
			{event: "Stop", command: env.Binary + " hook stop"},
			{event: "UserPromptSubmit", command: env.Binary + " hook display"},
		},
		statusLine: env.Binary + " statusline",
	}
}

// claudeSettingsView is a read-only projection of settings.json, decoded with
// the same hookGroup/hookEntry types mergeClaudeSettings writes with.
type claudeSettingsView struct {
	exists     bool
	hooks      map[string][]string
	statusLine string
}

func readClaudeSettingsView(path string) (claudeSettingsView, error) {
	view := claudeSettingsView{hooks: map[string][]string{}}

	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return view, nil
		}
		return view, fmt.Errorf("stat %s: %w", path, err)
	}
	view.exists = true

	raw, _, err := readSettingsFile(path)
	if err != nil {
		return view, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return view, nil
	}

	settings := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return view, fmt.Errorf("parse %s: %w", path, err)
	}

	if rawHooks, ok := settings["hooks"]; ok && isJSONValue(rawHooks) {
		events := map[string][]json.RawMessage{}
		if err := json.Unmarshal(rawHooks, &events); err != nil {
			return view, fmt.Errorf(`parse "hooks" in %s: %w`, path, err)
		}
		for event, groups := range events {
			for _, rawGroup := range groups {
				var g hookGroup
				if json.Unmarshal(rawGroup, &g) != nil {
					continue // a group shaped by another tool; not ours to read
				}
				for _, e := range g.Hooks {
					if e.Command != "" {
						view.hooks[event] = append(view.hooks[event], e.Command)
					}
				}
			}
		}
	}

	if rawLine, ok := settings["statusLine"]; ok && isJSONValue(rawLine) {
		var entry hookEntry
		if json.Unmarshal(rawLine, &entry) == nil && entry.Command != "" {
			view.statusLine = entry.Command
		} else {
			// Claude also accepts a bare command string here.
			var s string
			if json.Unmarshal(rawLine, &s) == nil {
				view.statusLine = s
			}
		}
	}

	return view, nil
}

func isJSONValue(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
}

// hookTarget is the executable a hook command runs; hookArgs is the rest.
func hookTarget(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return ""
	}
	return strings.Trim(fields[0], `"'`)
}

func hookArgs(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) <= 1 {
		return ""
	}
	return strings.Join(fields[1:], " ")
}

// looksLikePeriscope lets doctor recognise a hook registered by a copy of
// periscope living somewhere else (a different install path, a renamed
// binary), which is a warning rather than the failure that "not registered" is.
func looksLikePeriscope(target string) bool {
	base := strings.ToLower(filepath.Base(target))
	for _, ext := range []string{".exe", ".sh", ".ps1", ".bat", ".cmd"} {
		base = strings.TrimSuffix(base, ext)
	}
	return strings.HasPrefix(base, "periscope")
}

// resolveHookTarget resolves the executable a hook command runs.
//
// Claude runs hook commands through a shell, so a bare name like `periscope` is
// resolved on PATH. Stat-ing it relative to the working directory instead would
// let a stray ./periscope in whatever directory doctor happens to run from
// vouch for a hook that is really broken — which it did, once, in this file.
func resolveHookTarget(target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("empty command")
	}
	if strings.HasPrefix(target, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			target = filepath.Join(home, target[2:])
		}
	}
	if strings.ContainsAny(target, `/\`) {
		if _, err := os.Stat(target); err != nil {
			return "", err
		}
		return target, nil
	}
	return exec.LookPath(target)
}

// sameBinary compares two paths by identity rather than by string, so a hook
// registered as a bare name on PATH, or through a symlink, is recognised as
// this binary instead of being reported as a stranger.
func sameBinary(a, b string) bool {
	if a == b {
		return true
	}
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(sa, sb)
}

// verifyHook decides whether one desired hook is actually wired up, returning
// the resolved target it found. A hook pointing at a binary that no longer
// exists runs nothing, so it is as bad as no hook at all and is reported as a
// failure, not a warning.
func verifyHook(registered []string, spec claudeHookSpec) (status checkStatus, note, resolved string) {
	wantArgs := hookArgs(spec.command)

	candidate := ""
	for _, cmd := range registered {
		if looksLikePeriscope(hookTarget(cmd)) && hookArgs(cmd) == wantArgs {
			candidate = cmd
			break
		}
	}
	for _, cmd := range registered { // an exact match always wins
		if cmd == spec.command {
			candidate = cmd
			break
		}
	}
	if candidate == "" {
		return ckFail, spec.event + " not registered", ""
	}

	target := hookTarget(candidate)
	resolved, err := resolveHookTarget(target)
	if err != nil {
		return ckFail, fmt.Sprintf("%s → %s does not exist", spec.event, target), ""
	}
	if sameBinary(resolved, hookTarget(spec.command)) {
		return ckOK, spec.event, resolved
	}
	return ckWarn, spec.event, resolved
}

func checkClaudeHooks(env doctorEnv) []checkResult {
	path := env.settingsPath()
	want := desiredDoctorSettings(env)
	reinit := fmt.Sprintf("Run `periscope init` — it merges the missing entries into %s and never overwrites hooks owned by other tools.", path)

	view, err := readClaudeSettingsView(path)
	if err != nil {
		return []checkResult{
			{checkHooks, ckFail, err.Error(), "Fix the JSON in " + path + ", then run `periscope init`."},
			{checkStatusLine, ckWarn, "not checked: " + path + " did not parse", "Fix the JSON in " + path + "."},
		}
	}
	if !view.exists {
		return []checkResult{
			{checkHooks, ckFail, path + " does not exist — no periscope hook can ever run", reinit},
			{checkStatusLine, ckWarn, path + " does not exist", reinit},
		}
	}

	worst := ckOK
	var okEvents, otherEvents, failNotes []string
	otherTargets := map[string]bool{}
	for _, spec := range want.hooks {
		status, note, resolved := verifyHook(view.hooks[spec.event], spec)
		worst = worseStatus(worst, status)
		switch status {
		case ckOK:
			okEvents = append(okEvents, spec.event)
		case ckWarn:
			otherEvents = append(otherEvents, spec.event)
			otherTargets[resolved] = true
		default:
			failNotes = append(failNotes, note)
		}
	}

	hooksResult := checkResult{Name: checkHooks, Status: worst}
	switch worst {
	case ckOK:
		hooksResult.Detail = strings.Join(okEvents, ", ") + " → this binary"
	case ckWarn:
		hooksResult.Detail = fmt.Sprintf("all registered, but %s → %s rather than this binary (%s)",
			strings.Join(otherEvents, ", "), strings.Join(sortedTargets(otherTargets), ", "), env.Binary)
		hooksResult.Remedy = "Harmless if that is the copy you want driving telemetry; otherwise run `periscope init` from this one."
	default:
		hooksResult.Detail = strings.Join(failNotes, "; ")
		if len(okEvents)+len(otherEvents) > 0 {
			hooksResult.Detail += fmt.Sprintf(" (ok: %s)", strings.Join(append(okEvents, otherEvents...), ", "))
		}
		hooksResult.Remedy = reinit
	}

	return []checkResult{hooksResult, checkClaudeStatusLine(env, view, want, reinit)}
}

// checkClaudeStatusLine is a warning, never a failure: the terminal statusline
// is cosmetic, and no ingestion depends on it.
func checkClaudeStatusLine(env doctorEnv, view claudeSettingsView, want desiredClaudeSettings, reinit string) checkResult {
	if view.statusLine == "" {
		return checkResult{checkStatusLine, ckWarn, "statusLine not set", reinit}
	}
	target := hookTarget(view.statusLine)
	if !looksLikePeriscope(target) {
		return checkResult{checkStatusLine, ckWarn,
			fmt.Sprintf("statusLine is owned by another tool (%s) — periscope left it alone", target),
			"Point statusLine at `" + want.statusLine + "` by hand if you want periscope's statusline."}
	}
	resolved, err := resolveHookTarget(target)
	if err != nil {
		return checkResult{checkStatusLine, ckWarn,
			fmt.Sprintf("statusLine → %s does not exist", target), reinit}
	}
	if hookArgs(view.statusLine) != hookArgs(want.statusLine) {
		return checkResult{checkStatusLine, ckWarn,
			fmt.Sprintf("statusLine runs `%s`, not `statusline`", view.statusLine), reinit}
	}
	if sameBinary(resolved, env.Binary) {
		return checkResult{checkStatusLine, ckOK, "statusLine → this binary", ""}
	}
	return checkResult{checkStatusLine, ckWarn,
		fmt.Sprintf("statusLine → %s rather than this binary (%s)", resolved, env.Binary), ""}
}

func sortedTargets(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func worseStatus(a, b checkStatus) checkStatus {
	rank := map[checkStatus]int{ckOK: 0, ckWarn: 1, ckFail: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// ── 2. Sidecar freshness ────────────────────────────────────────────────────

// doctorSessionIDRe mirrors sessionIDRe in internal/store/db.go: a sidecar is
// named after a session, and a session id is a UUID. Matching on *.json instead
// would let limit-history's neighbours (profile-cache.json,
// litellm-pricing-cache.json) masquerade as fresh telemetry.
var doctorSessionIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\.json$`)

func newestSidecar(dir string) (name string, mod time.Time, count int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}, 0, err
	}
	for _, e := range entries {
		if e.IsDir() || !doctorSessionIDRe.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		count++
		if info.ModTime().After(mod) {
			name, mod = e.Name(), info.ModTime()
		}
	}
	return name, mod, count, nil
}

// newestTranscript finds the most recently written Claude transcript. Claude
// writes these whether or not any hook is registered, which is what makes them
// a usable control for "did Claude run?".
func newestTranscript(claudeDir string) (path string, mod time.Time) {
	root := filepath.Join(claudeDir, "projects")
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil //nolint:nilerr // an unreadable subtree is not a finding
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(mod) {
			path, mod = p, info.ModTime()
		}
		return nil
	})
	return path, mod
}

func checkSidecarFreshness(env doctorEnv) checkResult {
	reinit := fmt.Sprintf("Run `periscope init` to re-register the Stop hook in %s, then start a Claude session and re-run doctor.", env.settingsPath())

	name, mod, count, err := newestSidecar(env.DataDir)
	if err != nil {
		return checkResult{checkSidecars, ckFail,
			fmt.Sprintf("cannot read %s: %v", env.DataDir, err), reinit}
	}

	_, tMod := newestTranscript(env.ClaudeDir)

	if count == 0 {
		if !tMod.IsZero() {
			return checkResult{checkSidecars, ckFail,
				fmt.Sprintf("no session sidecars in %s, but Claude wrote a transcript %s ago — the Stop hook is not running",
					env.DataDir, humanAge(env.Now.Sub(tMod))), reinit}
		}
		return checkResult{checkSidecars, ckWarn,
			fmt.Sprintf("no session sidecars in %s and no Claude transcripts either — nothing has run yet", env.DataDir),
			"Start a Claude Code session, then re-run `periscope doctor`."}
	}

	age := env.Now.Sub(mod)

	// The sharp check, and the one the outage needed: Claude is demonstrably
	// running but the hook that writes sidecars is not.
	if !tMod.IsZero() && tMod.Sub(mod) > hookLagLimit {
		return checkResult{checkSidecars, ckFail,
			fmt.Sprintf("newest sidecar is %s old (%s) but Claude wrote a transcript %s ago — the Stop hook is not running",
				humanAge(age), name, humanAge(env.Now.Sub(tMod))), reinit}
	}
	if age > sidecarFailAge {
		return checkResult{checkSidecars, ckFail,
			fmt.Sprintf("newest of %d sidecars is %s old (%s), past the %s limit",
				count, humanAge(age), name, humanAge(sidecarFailAge)), reinit}
	}
	if age > sidecarWarnAge {
		return checkResult{checkSidecars, ckWarn,
			fmt.Sprintf("newest of %d sidecars is %s old (%s)", count, humanAge(age), name),
			"Expected if you have not used Claude Code today; otherwise run `periscope init` to check the Stop hook."}
	}
	return checkResult{checkSidecars, ckOK,
		fmt.Sprintf("%d sidecars, newest %s ago (%s)", count, humanAge(age), name), ""}
}

// ── 3. Ingestion freshness ──────────────────────────────────────────────────

// openDoctorDB opens the database read-only. doctor runs against a live
// installation whose server holds the only writable handle; it must not create
// a file, migrate a schema, or take the writer lock.
func openDoctorDB(path string) (*sql.DB, error) {
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout(3000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// parseSessionStamp reads sessions.updated_at. Rows written by periscope use
// stampLayout; a row that fell back to the CURRENT_TIMESTAMP column default
// uses a space instead of a T.
func parseSessionStamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02T15:04:05Z", time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, strings.Replace(s, " ", "T", 1)); err == nil {
			return t.UTC(), true
		}
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func checkIngestion(env doctorEnv) checkResult {
	path := env.dbPath()
	serve := "Start the server with `periscope serve` — it imports on startup, on every file change, and on every poll cycle."

	if _, err := os.Stat(path); err != nil {
		return checkResult{checkIngest, ckFail, fmt.Sprintf("no database at %s", path),
			"Run `periscope init` and then `periscope serve`."}
	}

	db, err := openDoctorDB(path)
	if err != nil {
		return checkResult{checkIngest, ckFail, fmt.Sprintf("cannot open %s read-only: %v", path, err),
			"Check the file is a readable SQLite database and that ~/.periscope is not on a read-only mount."}
	}
	defer db.Close()

	var newest sql.NullString
	if err := db.QueryRow(`SELECT max(replace(updated_at, ' ', 'T')) FROM sessions`).Scan(&newest); err != nil {
		return checkResult{checkIngest, ckFail, fmt.Sprintf("cannot read sessions: %v", err), serve}
	}
	if !newest.Valid || newest.String == "" {
		return checkResult{checkIngest, ckFail, "the sessions table is empty — no sidecar has ever been imported", serve}
	}
	stamp, ok := parseSessionStamp(newest.String)
	if !ok {
		return checkResult{checkIngest, ckFail,
			fmt.Sprintf("newest sessions.updated_at is unparseable (%q)", newest.String), serve}
	}

	age := env.Now.Sub(stamp)
	_, sideMod, sideCount, _ := newestSidecar(env.DataDir)

	// Hooks writing but nothing importing: this is the shape ImportSidecars
	// hid by logging count=0 as a success.
	if sideCount > 0 {
		lag := sideMod.Sub(stamp)
		if lag > ingestLagFail {
			return checkResult{checkIngest, ckFail,
				fmt.Sprintf("newest sidecar is %s newer than the newest sessions row (row: %s ago) — sidecars are being written but not imported",
					humanAge(lag), humanAge(age)), serve}
		}
		if lag > ingestLagWarn {
			return checkResult{checkIngest, ckWarn,
				fmt.Sprintf("newest sidecar is %s newer than the newest sessions row", humanAge(lag)),
				"Check the file watcher and poll loop in the server log (" + env.logPath() + ")."}
		}
	}

	if age > sidecarFailAge {
		return checkResult{checkIngest, ckFail,
			fmt.Sprintf("newest sessions row is %s old, past the %s limit — the dashboard is rendering stale data",
				humanAge(age), humanAge(sidecarFailAge)), serve}
	}
	if age > sidecarWarnAge {
		return checkResult{checkIngest, ckWarn,
			fmt.Sprintf("newest sessions row is %s old", humanAge(age)),
			"Expected if you have not used Claude Code today; otherwise check the Stop hook and the server log."}
	}
	return checkResult{checkIngest, ckOK,
		fmt.Sprintf("newest sessions row %s ago (%s)", humanAge(age), stamp.Format(time.RFC3339)), ""}
}

// ── 4. Server reachability ──────────────────────────────────────────────────

func checkServerReachable(env doctorEnv) checkResult {
	url := env.healthURL()
	code, err := env.probeHealth(url)
	if err != nil {
		return checkResult{checkServer, ckFail, fmt.Sprintf("GET %s: %v", url, err),
			"Start it with `periscope serve`, or fix [server] host/port in " + filepath.Join(env.HomeDir, "config.toml") + "."}
	}
	if code != http.StatusOK {
		return checkResult{checkServer, ckFail, fmt.Sprintf("GET %s returned %d", url, code),
			"Check the server log at " + env.logPath() + "."}
	}
	return checkResult{checkServer, ckOK, url + " → 200", ""}
}

// ── 5. Database health ──────────────────────────────────────────────────────

func checkDatabase(env doctorEnv) []checkResult {
	path := env.dbPath()
	out := []checkResult{}

	st, err := os.Stat(path)
	if err != nil {
		miss := checkResult{checkDBFile, ckFail, fmt.Sprintf("no database at %s", path),
			"Run `periscope init` and then `periscope serve`."}
		return []checkResult{
			miss,
			{checkDBSchema, ckFail, "not checked: no database", miss.Remedy},
			{checkDBWAL, ckOK, "no database, so no WAL", ""},
		}
	}

	// The database holds the VAPID private key, so its mode is a security
	// property, not housekeeping. OpenDB pre-creates the file at 0600 for
	// exactly this reason; anything looser was widened after the fact.
	perm := st.Mode().Perm()
	switch {
	case runtime.GOOS == "windows":
		out = append(out, checkResult{checkDBFile, ckOK,
			fmt.Sprintf("%s (unix modes not enforced on windows)", humanBytes(st.Size())), ""})
	case perm != 0o600:
		out = append(out, checkResult{checkDBFile, ckFail,
			fmt.Sprintf("%s, mode %04o — it holds the VAPID private key and must be 0600", humanBytes(st.Size()), perm),
			fmt.Sprintf("chmod 600 %s", path)})
	default:
		out = append(out, checkResult{checkDBFile, ckOK,
			fmt.Sprintf("%s, mode 0600", humanBytes(st.Size())), ""})
	}

	out = append(out, checkSchemaVersion(env))
	out = append(out, checkWALSize(env))
	return out
}

func checkSchemaVersion(env doctorEnv) checkResult {
	db, err := openDoctorDB(env.dbPath())
	if err != nil {
		return checkResult{checkDBSchema, ckFail, fmt.Sprintf("cannot open read-only: %v", err),
			"Check that " + env.dbPath() + " is a readable SQLite database."}
	}
	defer db.Close()

	var version int
	if err := db.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&version); err != nil {
		return checkResult{checkDBSchema, ckFail, fmt.Sprintf("cannot read schema_version: %v", err),
			"Run `periscope serve` once — OpenDB creates and migrates the schema on open."}
	}
	switch {
	case version < expectedSchemaVersion:
		return checkResult{checkDBSchema, ckFail,
			fmt.Sprintf("schema is v%d, this binary expects v%d", version, expectedSchemaVersion),
			"Run `periscope serve` once — OpenDB applies the pending migrations on open."}
	case version > expectedSchemaVersion:
		return checkResult{checkDBSchema, ckWarn,
			fmt.Sprintf("schema is v%d but this binary only knows v%d — it was written by a newer periscope", version, expectedSchemaVersion),
			"Upgrade this periscope binary, or point it at a different data directory."}
	}
	return checkResult{checkDBSchema, ckOK, fmt.Sprintf("v%d (current)", version), ""}
}

func checkWALSize(env doctorEnv) checkResult {
	st, err := os.Stat(env.walPath())
	if err != nil {
		return checkResult{checkDBWAL, ckOK, "no WAL file (checkpointed)", ""}
	}
	size := st.Size()
	switch {
	case size >= walFailBytes:
		return checkResult{checkDBWAL, ckFail,
			fmt.Sprintf("%s, past the %s forced-checkpoint threshold — maintenance is not running",
				humanBytes(size), humanBytes(walFailBytes)),
			"Restart the server with `periscope serve`; StartMaintenance truncates the WAL once it is past threshold."}
	case size >= walWarnBytes:
		return checkResult{checkDBWAL, ckWarn,
			fmt.Sprintf("%s, past the %s journal_size_limit — checkpoints are not truncating",
				humanBytes(size), humanBytes(walWarnBytes)),
			"Usually transient under load; if it persists, restart the server."}
	}
	return checkResult{checkDBWAL, ckOK, humanBytes(size), ""}
}

// ── 6. Log health ───────────────────────────────────────────────────────────

func checkLogHealth(env doctorEnv) []checkResult {
	path := env.logPath()
	var fileResult checkResult

	st, err := os.Stat(path)
	switch {
	case err != nil:
		fileResult = checkResult{checkLogFile, ckWarn, fmt.Sprintf("no log file at %s", path),
			"Start the server with `periscope serve`; it creates the log on startup."}
	case st.Size() >= logFailBytes:
		fileResult = checkResult{checkLogFile, ckFail,
			fmt.Sprintf("%s, far past the %s rotation threshold — rotation is failing", humanBytes(st.Size()), humanBytes(logWarnBytes)),
			"Check stderr for `log rotation failed`, then restart the server."}
	case st.Size() >= logWarnBytes:
		fileResult = checkResult{checkLogFile, ckWarn,
			fmt.Sprintf("%s, past the %s rotation threshold", humanBytes(st.Size()), humanBytes(logWarnBytes)),
			"A serving process re-checks every minute; if it stays oversize, rotation is failing."}
	default:
		detail := humanBytes(st.Size())
		if rot, rerr := os.Stat(path + rotatedSuffix); rerr == nil {
			detail += fmt.Sprintf(" (+ %s rotated)", humanBytes(rot.Size()))
		}
		fileResult = checkResult{checkLogFile, ckOK, detail, ""}
	}

	// Debug was 72% of log volume back when it was the hardcoded level.
	level := resolveLogLevel(env.Config.Logging.Level)
	levelResult := checkResult{checkLogLevel, ckOK, level.String(), ""}
	if level <= slog.LevelDebug {
		levelResult = checkResult{checkLogLevel, ckWarn,
			"debug — roughly 72% of log volume is debug lines",
			`Set level = "info" under [logging] in ` + filepath.Join(env.HomeDir, "config.toml") + ` (or unset ` + logLevelEnvVar + `).`}
	}

	return []checkResult{fileResult, levelResult}
}

// ── 7. Disk space ───────────────────────────────────────────────────────────

func checkDiskSpace(env doctorEnv) []checkResult {
	return []checkResult{
		diskCheck(checkDiskHome, env.HomeDir),
		diskCheck(checkDiskData, env.DataDir),
	}
}

func diskCheck(name, dir string) checkResult {
	free, total, err := diskFree(dir)
	if err != nil {
		return checkResult{name, ckWarn, fmt.Sprintf("cannot stat the filesystem holding %s: %v", dir, err),
			"Confirm " + dir + " exists and is readable."}
	}

	pct := 100.0
	if total > 0 {
		pct = float64(free) / float64(total) * 100
	}
	detail := fmt.Sprintf("%s free of %s (%.0f%%)", humanBytes(int64(free)), humanBytes(int64(total)), pct)

	switch {
	case free < diskFailBytes:
		return checkResult{name, ckFail, detail,
			"Free space now — a sidecar or database write that fails for ENOSPC is as silent as a hook that never runs."}
	case free < diskWarnBytes || pct < diskWarnPercent:
		return checkResult{name, ckWarn, detail, "Free space before telemetry writes start failing."}
	}
	return checkResult{name, ckOK, detail, ""}
}

// ── Formatting ──────────────────────────────────────────────────────────────

func humanAge(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}
