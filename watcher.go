package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

func startWatcher(app *App) (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	// Watch claude data directory (sidecars, history files)
	if err := watcher.Add(app.DataDir); err != nil {
		log.Printf("watcher: cannot watch data dir: %v", err)
	}

	// Watch each plugin subdirectory (create if missing)
	pluginTypes := []string{"themes", "widgets", "pricing", "forecasters", "canvas", "vendor"}
	for _, pt := range pluginTypes {
		dir := filepath.Join(app.PluginDir, pt)
		os.MkdirAll(dir, 0755)
		if err := watcher.Add(dir); err != nil {
			log.Printf("watcher: cannot watch %s: %v", pt, err)
		}
	}

	// Event loop
	go watchLoop(app, watcher)

	return watcher, nil
}

func watchLoop(app *App, watcher *fsnotify.Watcher) {
	// Debounce: batch rapid file changes
	var debounceTimer *time.Timer
	pendingDataReload := false
	pendingPluginReloads := make(map[string]bool)

	flush := func() {
		if pendingDataReload {
			log.Println("watcher: data changed, re-importing")
			if err := importFileData(app); err != nil {
				log.Printf("watcher: import error: %v", err)
			}
			// Push fresh data to all WS clients
			data, err := buildDashboardData(app)
			if err == nil {
				app.Hub.broadcastJSON("data", data)
			}
			pendingDataReload = false
		}

		for plugin := range pendingPluginReloads {
			log.Printf("watcher: plugin changed: %s", plugin)
			app.Hub.broadcastJSON("reload", map[string]string{"plugin": plugin})
		}
		pendingPluginReloads = make(map[string]bool)
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}

			if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) && !event.Has(fsnotify.Remove) {
				continue
			}

			name := filepath.Base(event.Name)
			dir := filepath.Dir(event.Name)

			// Classify the change
			if isDataDir(dir, app.DataDir) {
				// Data file changed
				if strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".jsonl") {
					pendingDataReload = true
				}
			} else if isPluginDir(dir, app.PluginDir) {
				// Plugin file changed
				rel, _ := filepath.Rel(app.PluginDir, event.Name)
				pendingPluginReloads[rel] = true
			}

			// Debounce: wait 500ms after last change before flushing
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(500*time.Millisecond, flush)

		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("watcher error: %v", err)
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
