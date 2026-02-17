package main

import (
	"fmt"
	"io/fs"
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

func iBanner() {
	fmt.Println()
	fmt.Printf("  %s╔═══════════════════════════════════════════╗%s\n", cDim, cReset)
	fmt.Printf("  %s║%s  %sP E R I S C O P E%s                       %s║%s\n", cDim, cReset, cBold, cReset, cDim, cReset)
	fmt.Printf("  %s║%s  Claude Code Telemetry Dashboard          %s║%s\n", cDim, cReset, cDim, cReset)
	fmt.Printf("  %s╚═══════════════════════════════════════════╝%s\n", cDim, cReset)
	fmt.Println()
}

func iDivider() {
	fmt.Printf("\n  %s───────────────────────────────────────────────%s\n", cDim, cReset)
}

func iPrompt(question string) bool {
	fmt.Printf("  %s", question)
	var answer string
	fmt.Scanln(&answer)
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer != "n" && answer != "no"
}

// ── Install ─────────────────────────────────────────────────────────────────

func install(app *App) error {
	// Detect if this is a first-time install or re-init
	_, existsErr := os.Stat(app.PluginDir)
	isReinstall := existsErr == nil

	iBanner()

	if isReinstall {
		fmt.Printf("  %sRe-initializing existing installation%s\n", cDim, cReset)
	}

	totalSteps := 5
	if runtime.GOOS == "windows" {
		totalSteps = 6
	}

	// ── Step 1: Directories ──
	iStep(1, totalSteps, "Creating directory structure")
	slog.Info("creating directory structure")
	dirs := []string{
		app.HomeDir,
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

	// ── Step 2: Extract plugins ──
	iStep(2, totalSteps, "Extracting bundled plugins")
	slog.Info("extracting bundled plugins")
	extracted := 0
	skipped := 0
	fs.WalkDir(defaultPlugins, "defaults", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel("defaults", path)
		dest := filepath.Join(app.PluginDir, rel)

		if _, err := os.Stat(dest); err == nil {
			slog.Debug("file exists, skipping", "file", rel)
			skipped++
			return nil // Don't clobber user edits
		}

		data, err := defaultPlugins.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dest, data, 0644); err != nil {
			return err
		}
		slog.Debug("file extracted", "file", rel)
		extracted++
		return nil
	})
	slog.Info("plugin extraction complete", "extracted", extracted, "skipped", skipped)
	if extracted > 0 {
		iOK(fmt.Sprintf("Extracted %d files", extracted))
	}
	if skipped > 0 {
		iInfo(fmt.Sprintf("Skipped %d existing files (preserving your edits)", skipped))
	}

	// ── Step 3: Config ──
	iStep(3, totalSteps, "Writing configuration")
	slog.Info("writing configuration")
	configPath := filepath.Join(app.HomeDir, "config.toml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		defaultConfig := fmt.Sprintf(`# Periscope configuration

[server]
host = "localhost"
port = %d

# Override Claude data directory (usually auto-detected)
# data_dir = ""
`, app.Config.Server.Port)
		os.WriteFile(configPath, []byte(defaultConfig), 0644)
		slog.Info("config file created", "path", configPath)
		iOK(fmt.Sprintf("Created config.toml (port %d)", app.Config.Server.Port))
	} else {
		slog.Debug("config file exists, skipping", "path", configPath)
		iInfo("config.toml already exists, keeping yours")
	}

	// ── Step 4: Claude hooks ──
	iStep(4, totalSteps, "Registering Claude hooks")
	slog.Info("registering Claude hooks")
	if err := registerHooks(app); err != nil {
		slog.Warn("hook registration error", "err", err)
		iWarn(fmt.Sprintf("Hook registration: %v", err))
	} else {
		slog.Info("hooks registered")
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

	// ── Step 6: Autostart (Windows only) ──
	if runtime.GOOS == "windows" {
		iStep(6, totalSteps, "Background service")
		slog.Info("setting up Windows autostart")
		if err := offerAutostart(app); err != nil {
			slog.Warn("autostart setup error", "err", err)
			iWarn(fmt.Sprintf("Autostart: %v", err))
		} else {
			slog.Info("autostart setup complete")
		}
	}

	// ── Summary ──
	slog.Info("installation complete", "dirs", len(dirs), "extracted", extracted)
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

func registerHooks(app *App) error {
	binary := periscopeBinary()
	slog.Debug("using binary", "path", binary)

	// Write the launcher script (health-check → auto-start)
	var launcherContent string
	var launcherName string

	if runtime.GOOS == "windows" {
		launcherName = "periscope-ensure.ps1"
		launcherContent = fmt.Sprintf(`# Ensure periscope server is running
$ErrorActionPreference = 'SilentlyContinue'
try {
    $resp = Invoke-WebRequest -Uri 'http://localhost:%d/api/health' -TimeoutSec 1 -UseBasicParsing
    if ($resp.StatusCode -eq 200) { exit 0 }
} catch {}

# Not running — start it
Start-Process -WindowStyle Hidden -FilePath '%s' -ArgumentList 'serve'
`, app.Config.Server.Port, binary)
	} else {
		launcherName = "periscope-ensure.sh"
		launcherContent = fmt.Sprintf(`#!/bin/sh
# Ensure periscope server is running
if curl -sf http://localhost:%d/api/health >/dev/null 2>&1; then
    exit 0
fi
nohup "%s" serve >/dev/null 2>&1 &
`, app.Config.Server.Port, binary)
	}

	launcherPath := filepath.Join(app.HomeDir, launcherName)
	if err := os.WriteFile(launcherPath, []byte(launcherContent), 0755); err != nil {
		return fmt.Errorf("write launcher: %w", err)
	}
	slog.Info("launcher script created", "path", launcherPath)
	iOK(fmt.Sprintf("Created %s (auto-start on Claude session)", launcherName))

	// Show hook commands for manual setup
	iInfo("Claude hook commands (if not already configured):")
	fmt.Printf("    %sSessionStart%s:       %s%s\n", cDim, cReset, launcherPath, cReset)
	fmt.Printf("    %sStopTurn%s:           %s hook stop\n", cDim, cReset, binary)
	fmt.Printf("    %sUserPromptSubmit%s:   %s hook display\n", cDim, cReset, binary)
	fmt.Printf("    %sStatusline%s:         %s statusline\n", cDim, cReset, binary)

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
