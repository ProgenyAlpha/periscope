package main

import (
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

func install(app *App) error {
	fmt.Println("Setting up Periscope...")

	// Create directory structure
	dirs := []string{
		app.HomeDir,
		app.PluginDir,
		filepath.Join(app.PluginDir, "themes"),
		filepath.Join(app.PluginDir, "widgets"),
		filepath.Join(app.PluginDir, "pricing"),
		filepath.Join(app.PluginDir, "forecasters"),
		filepath.Join(app.PluginDir, "canvas"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	fmt.Println("  Created plugin directories")

	// Extract default plugins (only if not already present — don't clobber user edits)
	extracted := 0
	fs.WalkDir(defaultPlugins, "defaults", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		// defaults/themes/foo.toml → plugins/themes/foo.toml
		rel, _ := filepath.Rel("defaults", path)
		dest := filepath.Join(app.PluginDir, rel)

		if _, err := os.Stat(dest); err == nil {
			return nil // Already exists, don't overwrite
		}

		data, err := defaultPlugins.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dest, data, 0644); err != nil {
			return err
		}
		extracted++
		return nil
	})
	fmt.Printf("  Extracted %d default plugins\n", extracted)

	// Write default config if missing
	configPath := filepath.Join(app.HomeDir, "config.toml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		defaultConfig := `# Periscope configuration

[server]
host = "localhost"
port = 8384

# Override Claude data directory (usually auto-detected)
# data_dir = ""
`
		os.WriteFile(configPath, []byte(defaultConfig), 0644)
		fmt.Println("  Created config.toml")
	}

	// Register Claude hooks
	if err := registerHooks(app); err != nil {
		log.Printf("warning: hook registration: %v", err)
	}

	// Verify OAuth token
	if _, err := getOAuthToken(app); err != nil {
		fmt.Printf("  Warning: %v\n", err)
		fmt.Println("  Rate limit tracking requires Claude OAuth login")
	} else {
		fmt.Println("  OAuth token verified")
	}

	fmt.Println("Done.")
	return nil
}

func registerHooks(app *App) error {
	// Read existing hooks config
	hooksPath := filepath.Join(app.ClaudeDir, "hooks.json")

	// The hooks need to know where periscope binary is
	binary := periscopeBinary()

	// Create/update hooks that start periscope if not running
	// The existing hooks (cost-tracker-stop.ps1, cost-tracker-display.ps1) already
	// write sidecar files. We just need periscope serve running to pick them up.

	// Write a small launcher script
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
	fmt.Printf("  Created %s\n", launcherName)

	// Print hook configuration instructions
	fmt.Println("  Hook commands for ~/.claude/hooks.json:")
	fmt.Println("")
	fmt.Printf("    StopTurn:          %s hook stop\n", binary)
	fmt.Printf("    UserPromptSubmit:  %s hook display\n", binary)
	fmt.Println("")
	fmt.Println("  Example hooks.json:")
	fmt.Printf(`    {
      "hooks": {
        "StopTurn": [{"command": "%s hook stop"}],
        "UserPromptSubmit": [{"command": "%s hook display"}]
      }
    }
`, binary, binary)

	_ = hooksPath // acknowledge we read but don't modify
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

func uninstall(app *App) error {
	fmt.Println("Removing Periscope...")

	// Try to shut down running server
	addr := fmt.Sprintf("http://%s:%d", app.Config.Server.Host, app.Config.Server.Port)
	resp, err := httpGet(addr + "/api/health")
	if err == nil {
		resp.Body.Close()
		// Server is running, shut it down
		http.Post(addr+"/api/shutdown", "application/json", nil)
		fmt.Println("  Stopped running server")
	}

	// Remove periscope home directory
	if _, err := os.Stat(app.HomeDir); err == nil {
		fmt.Printf("  Remove %s? This deletes all plugins and data. [y/N] ", app.HomeDir)
		var answer string
		fmt.Scanln(&answer)
		if answer == "y" || answer == "Y" {
			os.RemoveAll(app.HomeDir)
			fmt.Println("  Removed")
		} else {
			fmt.Println("  Kept")
		}
	}

	fmt.Println("Note: Claude hooks and cost-state data are preserved.")
	fmt.Println("Done.")
	return nil
}
