package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/ProgenyAlpha/periscope/internal/store"
)

func startWatcher(app *App) (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Error("failed to create watcher", "err", err)
		return nil, err
	}

	slog.Info("watcher started")

	// Watch claude data directory (sidecars, history files)
	if err := watcher.Add(app.DataDir); err != nil {
		slog.Warn("cannot watch data dir", "path", app.DataDir, "err", err)
	} else {
		slog.Info("watching data dir", "path", app.DataDir)
	}

	// Watch each plugin subdirectory (create if missing)
	pluginTypes := []string{"themes", "widgets", "pricing", "forecasters", "canvas", "vendor"}
	for _, pt := range pluginTypes {
		dir := filepath.Join(app.PluginDir, pt)
		os.MkdirAll(dir, 0755)
		if err := watcher.Add(dir); err != nil {
			slog.Warn("cannot watch plugin dir", "type", pt, "err", err)
		} else {
			slog.Debug("watching plugin dir", "type", pt)
		}
	}

	// Watch teams directory for agent tracker
	teamsDir := filepath.Join(app.ClaudeDir, "teams")
	if err := watcher.Add(teamsDir); err != nil {
		slog.Warn("cannot watch teams dir", "path", teamsDir, "err", err)
	} else {
		slog.Info("watching teams dir", "path", teamsDir)
		// Also watch each existing team subdir for config.json changes
		if entries, err := os.ReadDir(teamsDir); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					subDir := filepath.Join(teamsDir, e.Name())
					if err := watcher.Add(subDir); err != nil {
						slog.Warn("cannot watch team subdir", "path", subDir, "err", err)
					}
				}
			}
		}
	}

	// Event loop
	go watchLoop(app, watcher)

	return watcher, nil
}

func watchLoop(app *App, watcher *fsnotify.Watcher) {
	// Debounce: batch rapid file changes using a channel to avoid cross-goroutine access
	var debounceTimer *time.Timer
	pendingDataReload := false
	pendingPluginReloads := make(map[string]bool)
	flushCh := make(chan struct{}, 1)

	flush := func() {
		if pendingDataReload {
			slog.Debug("debounced data reload triggered")
			if err := store.ImportFileData(app.DB, app.DataDir, app.ClaudeDir); err != nil {
				slog.Warn("import error", "err", err)
			} else {
				slog.Debug("data reimport successful")
			}
			// Push fresh data to all WS clients
			data, err := store.BuildDashboardData(app.DB)
			if err == nil {
				app.Hub.broadcastJSON("data", data)
				slog.Debug("broadcasted updated data to ws clients")
			} else {
				slog.Error("failed to build dashboard data", "err", err)
			}
			pendingDataReload = false
		}

		for plugin := range pendingPluginReloads {
			slog.Debug("debounced plugin reload", "plugin", plugin)
			app.Hub.broadcastJSON("reload", map[string]string{"plugin": plugin})
		}
		pendingPluginReloads = make(map[string]bool)
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				slog.Info("event channel closed, stopping watcher")
				return
			}

			if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) && !event.Has(fsnotify.Remove) {
				continue
			}

			name := filepath.Base(event.Name)
			dir := filepath.Dir(event.Name)
			eventType := ""
			if event.Has(fsnotify.Write) {
				eventType = "WRITE"
			} else if event.Has(fsnotify.Create) {
				eventType = "CREATE"
			} else if event.Has(fsnotify.Remove) {
				eventType = "REMOVE"
			}

			// Classify the change
			if isDataDir(dir, app.DataDir) {
				// Data file changed
				if strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".jsonl") {
					slog.Debug("data file change", "file", name, "event", eventType)
					pendingDataReload = true
				}
			} else if isTeamsDir(dir, app.ClaudeDir) {
				// Team config changed — also watch newly created subdirs
				slog.Debug("teams dir change", "file", name, "event", eventType)
				if event.Has(fsnotify.Create) {
					newPath := filepath.Join(dir, name)
					if info, err := os.Stat(newPath); err == nil && info.IsDir() {
						watcher.Add(newPath)
					}
				}
				pendingDataReload = true
			} else if isPluginDir(dir, app.PluginDir) {
				// Plugin file changed
				rel, _ := filepath.Rel(app.PluginDir, event.Name)
				slog.Debug("plugin file change", "file", rel, "event", eventType)
				pendingPluginReloads[rel] = true
			}

			// Debounce: wait 500ms after last change, then signal the event loop
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(500*time.Millisecond, func() {
				select {
				case flushCh <- struct{}{}:
				default:
				}
			})

		case <-flushCh:
			flush()

		case err, ok := <-watcher.Errors:
			if !ok {
				slog.Info("error channel closed, stopping watcher")
				return
			}
			slog.Error("watcher error", "err", err)
		}
	}
}

func isDataDir(dir, dataDir string) bool {
	abs1, _ := filepath.Abs(dir)
	abs2, _ := filepath.Abs(dataDir)
	return strings.EqualFold(abs1, abs2)
}

func isPluginDir(dir, pluginDir string) bool {
	abs1, _ := filepath.Abs(dir)
	abs2, _ := filepath.Abs(pluginDir)
	return strings.HasPrefix(strings.ToLower(abs1), strings.ToLower(abs2))
}

func isTeamsDir(dir, claudeDir string) bool {
	abs1, _ := filepath.Abs(dir)
	abs2, _ := filepath.Abs(filepath.Join(claudeDir, "teams"))
	return strings.HasPrefix(strings.ToLower(abs1), strings.ToLower(abs2))
}
