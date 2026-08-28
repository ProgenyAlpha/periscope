package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ProgenyAlpha/periscope/internal/anthropic"
)

// ── UI helpers ──────────────────────────────────────────────────────────────

const (
	cDim    = "\033[90m"
	cBold   = "\033[1m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cCyan   = "\033[36m"
	cRed    = "\033[31m"
	cReset  = "\033[0m"
)

func iOK(msg string)   { fmt.Printf("  %s[OK]%s  %s\n", cGreen, cReset, msg) }
func iWarn(msg string) { fmt.Printf("  %s[!!]%s  %s\n", cYellow, cReset, msg) }
func iInfo(msg string) { fmt.Printf("  %s...%s  %s\n", cDim, cReset, msg) }
func iStep(n, total int, msg string) {
	fmt.Printf("\n  %s[%d/%d]%s %s%s%s\n", cCyan, n, total, cReset, cBold, msg, cReset)
}

// The banner and divider exist in a writer-taking form so `doctor` can render
// its whole report into a buffer for tests without duplicating the strings.
// Duplicating them is how a "human output is unchanged" test starts passing
// against a copy of the printer instead of the printer.

func iBanner() { fIBanner(os.Stdout) }

func fIBanner(w io.Writer) {
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s╔═══════════════════════════════════════════╗%s\n", cDim, cReset)
	fmt.Fprintf(w, "  %s║%s  %sP E R I S C O P E%s                       %s║%s\n", cDim, cReset, cBold, cReset, cDim, cReset)
	fmt.Fprintf(w, "  %s║%s  Claude Code Telemetry Dashboard          %s║%s\n", cDim, cReset, cDim, cReset)
	fmt.Fprintf(w, "  %s╚═══════════════════════════════════════════╝%s\n", cDim, cReset)
	fmt.Fprintln(w)
}

func iDivider() { fIDivider(os.Stdout) }

func fIDivider(w io.Writer) {
	fmt.Fprintf(w, "\n  %s───────────────────────────────────────────────%s\n", cDim, cReset)
}

func iPrompt(question string) bool {
	fmt.Printf("  %s", question)
	var answer string
	fmt.Scanln(&answer)
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer != "n" && answer != "no"
}

func printSyncSummary(r syncResult) {
	if r.written > 0 {
		iOK(fmt.Sprintf("Wrote %d files", r.written))
	}
	if r.adopted > 0 {
		iInfo(fmt.Sprintf("Started tracking %d untouched files", r.adopted))
	}
	if r.unchanged > 0 {
		iInfo(fmt.Sprintf("%d files already current", r.unchanged))
	}
	if len(r.preserved) > 0 {
		iWarn(fmt.Sprintf("Preserved %d edited files (yours, not overwritten):", len(r.preserved)))
		for _, p := range r.preserved {
			fmt.Printf("    %s%s%s\n", cDim, p, cReset)
		}
	}
}

// ── Install ─────────────────────────────────────────────────────────────────

// installOptions carries the decisions `init` is allowed to make beyond its
// defaults. It exists for exactly one of them today: whether to install the
// recurring health check.
type installOptions struct {
	Schedule   bool   // --schedule
	NoSchedule bool   // --no-schedule, and it beats --schedule
	Interval   string // --interval

	// ConfirmSchedule is set only when init is attached to a terminal, and is
	// the ONLY path by which a schedule gets installed without an explicit
	// flag. Nil means non-interactive, which means no.
	ConfirmSchedule func() bool

	// Scheduler is the hook that reaches the machine's real systemd or
	// crontab. Only `periscope init` sets it. It is nil in `serve`'s first-run
	// install and nil in every test, so no code path other than an explicit
	// `periscope init` can read or write the scheduling state of this machine.
	Scheduler func(installOptions)
}

// initOptions builds the options for one `periscope init` invocation. It is the
// single place where flags, the terminal, and the scheduler hook come together,
// so the "a plain init schedules nothing" guarantee is testable without running
// an install.
func initOptions(args []string, interactive bool) (installOptions, error) {
	opts, err := parseInitFlags(args)
	if err != nil {
		return opts, err
	}
	opts.Scheduler = offerSchedule
	// Only ask when there is a human on the other end. Gating on the terminal
	// rather than on the flags is what keeps install.sh from hanging forever
	// on a question nobody can see.
	if !opts.Schedule && !opts.NoSchedule && interactive {
		opts.ConfirmSchedule = promptSchedule
	}
	return opts, nil
}

// scheduleDecision answers "does this init install a recurring health check?".
//
// The default is NO, and the default is load-bearing: `periscope init` is run
// by install.sh and again by `periscope serve` on first run. Neither may add a
// systemd timer or a crontab line to a machine as a side effect of starting a
// dashboard — a scheduled job the user did not ask for and cannot see is its
// own kind of silent failure.
func scheduleDecision(opts installOptions) bool {
	if opts.NoSchedule {
		return false
	}
	if opts.Schedule {
		return true
	}
	if opts.ConfirmSchedule == nil {
		return false
	}
	return opts.ConfirmSchedule()
}

func parseInitFlags(args []string) (installOptions, error) {
	var opts installOptions
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--schedule":
			opts.Schedule = true
		case "--no-schedule":
			opts.NoSchedule = true
		case "--interval":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--interval needs a value (%s)", supportedIntervals())
			}
			opts.Interval = args[i+1]
			i++
		default:
			return opts, fmt.Errorf("unknown argument %q\n\nUsage: periscope init [--schedule] [--no-schedule] [--interval %s]", args[i], defaultScheduleInterval)
		}
	}
	if opts.Interval != "" {
		if _, _, err := normalizeInterval(opts.Interval); err != nil {
			return opts, err
		}
	}
	return opts, nil
}

func install(app *App, opts installOptions) error {
	// Detect if this is a first-time install or re-init
	_, existsErr := os.Stat(app.PluginDir)
	isReinstall := existsErr == nil

	iBanner()

	if isReinstall {
		fmt.Printf("  %sRe-initializing existing installation%s\n", cDim, cReset)
	}

	totalSteps := 6
	if runtime.GOOS == "windows" {
		totalSteps = 7
	}

	// ── Step 1: Directories ──
	iStep(1, totalSteps, "Creating directory structure")
	slog.Info("creating directory structure")
	dirs := []string{
		app.HomeDir,
		// Every telemetry writer targets this directory: the polling loop's
		// usage and profile caches, the LiteLLM pricing cache, the
		// limit-history JSONL the statusline forecast reads, and the file
		// watcher. init never created it, so until the first Stop hook
		// happened to mkdir it they all failed with ENOENT into a log line.
		app.DataDir,
		app.PluginDir,
		filepath.Join(app.PluginDir, "themes"),
		filepath.Join(app.PluginDir, "widgets"),
		filepath.Join(app.PluginDir, "pricing"),
		filepath.Join(app.PluginDir, "forecasters"),
		filepath.Join(app.PluginDir, "canvas"),
		filepath.Join(app.PluginDir, "vendor"),
		filepath.Join(app.PluginDir, "static"),
	}
	dirsCreated := 0
	dirsExisted := 0
	for _, d := range dirs {
		if stat, err := os.Stat(d); err == nil && stat.IsDir() {
			slog.Debug("directory exists", "path", d)
			dirsExisted++
		} else {
			if err := os.MkdirAll(d, 0755); err != nil {
				return fmt.Errorf("mkdir %s: %w", d, err)
			}
			slog.Debug("directory created", "path", d)
			dirsCreated++
		}
	}
	slog.Info("directories ready", "created", dirsCreated, "existed", dirsExisted)
	iOK(fmt.Sprintf("%d directories ready", len(dirs)))

	// ── Step 2: Sync plugins ──
	iStep(2, totalSteps, "Syncing bundled plugins")
	slog.Info("syncing bundled plugins")
	syncRes, err := syncPlugins(app.PluginDir)
	if err != nil {
		return fmt.Errorf("sync plugins: %w", err)
	}
	slog.Info("plugin sync complete", "written", syncRes.written, "adopted", syncRes.adopted, "unchanged", syncRes.unchanged, "preserved", len(syncRes.preserved))
	printSyncSummary(syncRes)

	// ── Step 3: Config ──
	iStep(3, totalSteps, "Writing configuration")
	slog.Info("writing configuration")
	configPath := filepath.Join(app.HomeDir, "config.toml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		defaultConfig := fmt.Sprintf(`# Periscope configuration

[server]
host = "localhost"
port = %d

[logging]
# debug | info | warn | error  (override with PERISCOPE_LOG_LEVEL)
level = "info"

# Override Claude data directory (usually auto-detected)
# data_dir = ""
`, app.Config.Server.Port)
		if err := os.WriteFile(configPath, []byte(defaultConfig), 0644); err != nil {
			return fmt.Errorf("write %s: %w", configPath, err)
		}
		slog.Info("config file created", "path", configPath)
		iOK(fmt.Sprintf("Created config.toml (port %d)", app.Config.Server.Port))
	} else {
		slog.Debug("config file exists, skipping", "path", configPath)
		iInfo("config.toml already exists, keeping yours")
	}

	// ── Step 4: Claude hooks ──
	iStep(4, totalSteps, "Registering Claude hooks")
	slog.Info("registering Claude hooks")
	// registerHooks logs exactly what it wrote vs. what was already there;
	// do not claim success here.
	if err := registerHooks(app); err != nil {
		slog.Warn("hook registration failed", "err", err)
		iWarn(fmt.Sprintf("Hook registration: %v", err))
	}

	// ── Step 5: OAuth ──
	iStep(5, totalSteps, "Verifying Anthropic connection")
	slog.Info("verifying OAuth token")
	if _, err := anthropic.NewClientFromDisk(app.ClaudeDir); err != nil {
		slog.Warn("OAuth token not found", "err", err)
		iWarn("No OAuth token found")
		iInfo("Rate limit tracking requires 'claude login' first")
		iInfo("Everything else works without it")
	} else {
		slog.Info("OAuth token verified")
		iOK("OAuth token verified — rate limit tracking active")
	}

	// ── Step 6: Recurring health check ──
	//
	// doctor catches the outage that motivated it, but only when something
	// runs it. This is where that something gets installed — and it is opt-in,
	// because `serve` calls install() too and must never schedule anything.
	iStep(6, totalSteps, "Recurring health check")
	if opts.Scheduler != nil {
		opts.Scheduler(opts)
	} else {
		iInfo("Not scheduled (the conservative default)")
		iInfo("Add one with `periscope schedule install`")
	}

	// ── Step 7: Autostart (Windows only) ──
	if runtime.GOOS == "windows" {
		iStep(7, totalSteps, "Background service")
		slog.Info("setting up Windows autostart")
		if err := offerAutostart(app); err != nil {
			slog.Warn("autostart setup error", "err", err)
			iWarn(fmt.Sprintf("Autostart: %v", err))
		} else {
			slog.Info("autostart setup complete")
		}
	}

	// ── Summary ──
	slog.Info("installation complete", "dirs", len(dirs), "written", syncRes.written)
	iDivider()
	addr := fmt.Sprintf("http://%s:%d", app.Config.Server.Host, app.Config.Server.Port)
	fmt.Println()
	fmt.Printf("  %s%sREADY%s\n", cBold, cGreen, cReset)
	fmt.Println()
	fmt.Printf("  %sDashboard%s   %s\n", cBold, cReset, addr)
	fmt.Printf("  %sConfig%s     %s\n", cBold, cReset, configPath)
	fmt.Printf("  %sPlugins%s    %s\n", cBold, cReset, app.PluginDir)
	fmt.Printf("  %sData%s       %s\n", cBold, cReset, app.DataDir)
	fmt.Println()
	fmt.Printf("  Run %speriscope serve%s to start the server.\n", cCyan, cReset)
	iDivider()
	fmt.Println()
	return nil
}

// offerSchedule is the init-time face of `periscope schedule install`. It
// prints what it would do before it asks, and it does nothing at all unless the
// answer is an explicit yes.
func offerSchedule(opts installOptions) {
	interval := opts.Interval
	if interval == "" {
		interval = defaultScheduleInterval
	}
	env := liveScheduleEnv(interval)

	// Already scheduled: never ask again, never add a second one, and refresh
	// the unit so its ExecStart follows a binary that moved since last time.
	if installed, detail, err := scheduleStatus(env); err == nil && installed {
		res, ierr := installSchedule(env)
		if ierr != nil {
			iWarn(fmt.Sprintf("Health check already installed but could not be refreshed: %v", ierr))
			return
		}
		slog.Info("health check already scheduled", "action", res.Action, "detail", detail)
		iOK("Health check already scheduled — " + detail)
		return
	}

	if !scheduleDecision(opts) {
		slog.Info("recurring health check not installed", "reason", "not requested")
		iInfo("Not scheduled (the conservative default)")
		iInfo("Add one later with `periscope schedule install`")
		return
	}

	res, err := installSchedule(env)
	if err != nil {
		slog.Warn("could not schedule the health check", "err", err)
		iWarn(fmt.Sprintf("Could not schedule the health check: %v", err))
		return
	}
	slog.Info("health check scheduled", "backend", res.Backend, "action", res.Action, "detail", res.Detail)
	iOK(fmt.Sprintf("Health check %s (%s)", res.Action, res.Backend))
	iInfo(res.Detail)
}

// promptSchedule is the interactive branch. It defaults to NO — iPrompt
// defaults to yes, which is right for "start at login" and wrong for anything
// that writes a unit file to a machine.
func promptSchedule() bool {
	fmt.Println()
	fmt.Printf("  %speriscope doctor checks that telemetry is actually flowing.%s\n", cDim, cReset)
	fmt.Printf("  %sNothing runs it on its own, which is how a five-day outage%s\n", cDim, cReset)
	fmt.Printf("  %sstayed invisible: the server was up the whole time.%s\n", cDim, cReset)
	fmt.Println()
	fmt.Printf("  %sA %s check would run:%s\n", cDim, defaultScheduleInterval, cReset)
	fmt.Printf("    %s%s%s\n", cDim, scheduleCommand(liveScheduleEnv(defaultScheduleInterval)), cReset)
	fmt.Printf("  %sand notify you only when something is broken.%s\n", cDim, cReset)
	fmt.Println()

	fmt.Printf("  Install a recurring health check? %s[y/N]%s ", cDim, cReset)
	var answer string
	fmt.Scanln(&answer)
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "y" || answer == "yes"
}

func offerAutostart(app *App) error {
	// Check if already registered
	slog.Debug("checking for existing autostart task")
	out, err := exec.Command("schtasks", "/Query", "/TN", "Periscope-AutoStart").CombinedOutput()
	alreadyExists := err == nil && strings.Contains(string(out), "Periscope-AutoStart")

	if alreadyExists {
		slog.Debug("autostart task already exists")
		iOK("Autostart already registered")
		return nil
	}

	// Explain the value proposition
	fmt.Println()
	fmt.Printf("  %sPeriscope runs a lightweight background server (~5MB RAM)%s\n", cDim, cReset)
	fmt.Printf("  %sthat collects Claude telemetry in real-time. It needs to%s\n", cDim, cReset)
	fmt.Printf("  %sbe running for the dashboard to have data.%s\n", cDim, cReset)
	fmt.Println()
	fmt.Printf("  %sTwo ways it stays alive:%s\n", cDim, cReset)
	fmt.Println()
	fmt.Printf("  %s>%s %sWindows Login%s — starts automatically when you sign in,\n", cCyan, cReset, cBold, cReset)
	fmt.Printf("    so the dashboard is ready before you open Claude.\n")
	fmt.Println()
	fmt.Printf("  %s>%s %sClaude Session%s — if the server ever goes down, it\n", cCyan, cReset, cBold, cReset)
	fmt.Printf("    auto-restarts the moment you open Claude.\n")
	fmt.Println()
	fmt.Printf("  The Claude hook is already configured. The question is\n")
	fmt.Printf("  whether to also start at Windows login.\n")
	fmt.Println()

	if !iPrompt(fmt.Sprintf("Start at Windows login? %s[Y/n]%s ", cDim, cReset)) {
		slog.Info("user declined autostart")
		iInfo("Skipped — Periscope will start when Claude does")
		return nil
	}

	slog.Info("creating autostart scheduled task")
	binary := periscopeBinary()
	cmd := exec.Command("schtasks", "/Create",
		"/TN", "Periscope-AutoStart",
		"/TR", fmt.Sprintf(`"%s" serve`, binary),
		"/SC", "ONLOGON",
		"/RL", "LIMITED",
		"/F",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		slog.Error("autostart task creation failed", "output", strings.TrimSpace(string(out)), "err", err)
		return fmt.Errorf("schtasks: %s: %w", strings.TrimSpace(string(out)), err)
	}
	slog.Info("autostart task registered")
	iOK("Registered autostart task (Periscope-AutoStart)")
	return nil
}

// probeHost turns a configured bind address into something the SessionStart
// launcher can actually dial.
//
// "", "0.0.0.0" and "::" are wildcards — a server bound to them is reachable,
// but they are not destinations, so the probe uses loopback instead. Bare IPv6
// literals get bracketed so they survive being pasted into a URL.
func probeHost(host string) string {
	host = strings.TrimSpace(host)
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	switch host {
	case "", "0.0.0.0", "::":
		return "localhost"
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// launcherScript builds the SessionStart auto-start script.
//
// The health probe MUST target the configured host:port. It used to be
// hardcoded to http://localhost:<port>: with `host` set to anything other than
// loopback the probe could never succeed, so every SessionStart spawned
// `periscope serve`, which then hit its own already-running check on the
// CONFIGURED host, found the live server and exited. The hook burned a process
// per session and never actually started anything.
//
// goos is passed in rather than read from runtime.GOOS so both branches are
// testable from one platform.
func launcherScript(cfg ServerConfig, binary, goos string) (name, content string) {
	port := cfg.Port
	if port == 0 {
		port = 8384 // same default newApp applies when config.toml omits it
	}
	healthURL := fmt.Sprintf("http://%s:%d/api/health", probeHost(cfg.Host), port)

	if goos == "windows" {
		return "periscope-ensure.ps1", fmt.Sprintf(`# Ensure periscope server is running
$ErrorActionPreference = 'SilentlyContinue'
try {
    $resp = Invoke-WebRequest -Uri '%s' -TimeoutSec 1 -UseBasicParsing
    if ($resp.StatusCode -eq 200) { exit 0 }
} catch {}

# Not running — start it
Start-Process -WindowStyle Hidden -FilePath '%s' -ArgumentList 'serve'
`, healthURL, binary)
	}

	return "periscope-ensure.sh", fmt.Sprintf(`#!/bin/sh
# Ensure periscope server is running
if curl -sf %s >/dev/null 2>&1; then
    exit 0
fi
nohup "%s" serve >/dev/null 2>&1 &
`, healthURL, binary)
}

func registerHooks(app *App) error {
	binary := periscopeBinary()
	slog.Debug("using binary", "path", binary)

	// Write the launcher script (health-check → auto-start)
	launcherName, launcherContent := launcherScript(app.Config.Server, binary, runtime.GOOS)

	launcherPath := filepath.Join(app.HomeDir, launcherName)
	if err := os.WriteFile(launcherPath, []byte(launcherContent), 0755); err != nil {
		return fmt.Errorf("write launcher: %w", err)
	}
	slog.Info("launcher script created", "path", launcherPath)
	iOK(fmt.Sprintf("Created %s (auto-start on Claude session)", launcherName))

	// Merge the hooks into ~/.claude/settings.json. Without the Stop hook no
	// sidecar files are ever written, so session ingestion silently dies —
	// printing the commands and hoping the user pastes them is not enough.
	settingsPath := filepath.Join(app.ClaudeDir, claudeSettingsName)
	want := desiredClaudeSettings{
		hooks: []claudeHookSpec{
			{event: "SessionStart", command: launcherPath},
			{event: "Stop", command: binary + " hook stop"},
			{event: "UserPromptSubmit", command: binary + " hook display"},
		},
		statusLine: binary + " statusline",
	}

	res, err := mergeClaudeSettings(settingsPath, want)
	if err != nil {
		// Report what the user must add by hand, since we could not.
		iWarn(fmt.Sprintf("Could not update %s: %v", settingsPath, err))
		iInfo("Add these Claude hooks manually:")
		for _, spec := range want.hooks {
			fmt.Printf("    %s%s%s: %s\n", cDim, spec.event, cReset, spec.command)
		}
		fmt.Printf("    %sstatusLine%s: %s\n", cDim, cReset, want.statusLine)
		return fmt.Errorf("update %s: %w", settingsPath, err)
	}

	if len(res.added) > 0 {
		slog.Info("claude hooks written", "path", settingsPath, "added", res.added)
		iOK(fmt.Sprintf("Registered in settings.json: %s", strings.Join(res.added, ", ")))
	}
	if len(res.updated) > 0 {
		slog.Info("claude hooks repointed at this binary", "path", settingsPath, "updated", res.updated)
		iOK(fmt.Sprintf("Updated in settings.json (repointed at this binary): %s", strings.Join(res.updated, ", ")))
	}
	if len(res.existing) > 0 {
		slog.Info("claude hooks already present", "path", settingsPath, "existing", res.existing)
		iInfo(fmt.Sprintf("Already configured: %s", strings.Join(res.existing, ", ")))
	}
	for _, s := range res.skipped {
		slog.Warn("claude setting owned by another tool, left alone", "path", settingsPath, "key", s)
		iWarn(fmt.Sprintf("%s already points elsewhere — left it alone", s))
	}

	return nil
}

func periscopeBinary() string {
	exe, err := os.Executable()
	if err != nil {
		if runtime.GOOS == "windows" {
			return "periscope.exe"
		}
		return "periscope"
	}
	return exe
}

// ── Uninstall ───────────────────────────────────────────────────────────────

func uninstall(app *App) error {
	iBanner()
	fmt.Printf("  %sUninstalling Periscope%s\n", cBold, cReset)
	fmt.Println()

	// Try to shut down running server
	addr := fmt.Sprintf("http://%s:%d", app.Config.Server.Host, app.Config.Server.Port)
	resp, err := httpGet(addr + "/api/health")
	if err == nil {
		resp.Body.Close()
		http.Post(addr+"/api/shutdown", "application/json", nil)
		iOK("Stopped running server")
	} else {
		iInfo("Server not running")
	}

	// Remove the recurring health check. This runs before the home directory
	// is deleted and unconditionally, on every platform: a systemd timer left
	// behind after an uninstall keeps firing a binary that is gone, and its
	// failures then look exactly like the telemetry failures it was watching
	// for. removeSchedule sweeps systemd and cron both.
	if removed, err := removeSchedule(liveScheduleEnv(defaultScheduleInterval)); err != nil {
		slog.Warn("could not fully remove the scheduled health check", "err", err)
		iWarn(fmt.Sprintf("Scheduled health check: %v", err))
	} else if len(removed) > 0 {
		slog.Info("scheduled health check removed", "removed", removed)
		iOK("Removed scheduled health check: " + strings.Join(removed, ", "))
	} else {
		iInfo("No scheduled health check found")
	}

	// Remove scheduled task
	if runtime.GOOS == "windows" {
		if err := exec.Command("schtasks", "/Delete", "/TN", "Periscope-AutoStart", "/F").Run(); err == nil {
			iOK("Removed autostart task")
		} else {
			iInfo("No autostart task found")
		}
	}

	// Remove periscope home directory
	if _, err := os.Stat(app.HomeDir); err == nil {
		fmt.Println()
		fmt.Printf("  %sRemove %s?%s\n", cBold, app.HomeDir, cReset)
		fmt.Printf("  %sThis deletes all plugins, themes, and the database.%s\n", cDim, cReset)
		fmt.Printf("  %sClaude hooks and session data are NOT affected.%s\n", cDim, cReset)
		fmt.Println()
		if iPrompt(fmt.Sprintf("Delete? %s[y/N]%s ", cDim, cReset)) {
			os.RemoveAll(app.HomeDir)
			iOK("Removed " + app.HomeDir)
		} else {
			iInfo("Kept " + app.HomeDir)
		}
	}

	iDivider()
	fmt.Println()
	fmt.Printf("  %sPeriscope removed.%s\n", cBold, cReset)
	fmt.Printf("  %sClaude hooks and cost-state data are preserved.%s\n", cDim, cReset)
	fmt.Printf("  %sRun 'periscope init' anytime to reinstall.%s\n", cDim, cReset)
	iDivider()
	fmt.Println()
	return nil
}
