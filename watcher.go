package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/shawnwakeman/periscope/internal/store"
)

func startWatcher(app *App) (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[WATCH] Failed to create watcher: %v", err)
		return nil, err
	}

	log.Printf("[WATCH] Watcher started")

	// Watch claude data directory (sidecars, history files)
	if err := watcher.Add(app.DataDir); err != nil {
		log.Printf("[WATCH] Cannot watch data dir %s: %v", app.DataDir, err)
	} else {
		log.Printf("[WATCH] Watching data dir: %s", app.DataDir)
	}

	// Watch each plugin subdirectory (create if missing)
	pluginTypes := []string{"themes", "widgets", "pricing", "forecasters", "canvas", "vendor"}
	for _, pt := range pluginTypes {
		dir := filepath.Join(app.PluginDir, pt)
		os.MkdirAll(dir, 0755)
		if err := watcher.Add(dir); err != nil {
			log.Printf("[WATCH] Cannot watch plugin dir %s: %v", pt, err)
		} else {
			log.Printf("[WATCH] Watching plugin dir: %s", pt)
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
			log.Println("[WATCH] Debounced reload triggered for data files")
			if err := store.ImportFileData(app.DB, app.DataDir, app.ClaudeDir); err != nil {
				log.Printf("[WATCH] Import error: %v", err)
			} else {
				log.Println("[WATCH] Data reimport successful")
			}
			// Push fresh data to all WS clients
			data, err := store.BuildDashboardData(app.DB)
			if err == nil {
				app.Hub.broadcastJSON("data", data)
				log.Println("[WATCH] Broadcasted updated data to WebSocket clients")
			} else {
				log.Printf("[WATCH] Failed to build dashboard data: %v", err)
			}
			pendingDataReload = false
		}

		for plugin := range pendingPluginReloads {
			log.Printf("[WATCH] Debounced reload triggered for plugin: %s", plugin)
			app.Hub.broadcastJSON("reload", map[string]string{"plugin": plugin})
		}
		pendingPluginReloads = make(map[string]bool)
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				log.Println("[WATCH] Event channel closed, stopping watcher")
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
					log.Printf("[WATCH] File change detected: %s [%s] -> queued data reload", name, eventType)
					pendingDataReload = true
				}
			} else if isPluginDir(dir, app.PluginDir) {
				// Plugin file changed
				rel, _ := filepath.Rel(app.PluginDir, event.Name)
				log.Printf("[WATCH] File change detected: %s [%s] -> queued plugin reload", rel, eventType)
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
				log.Println("[WATCH] Error channel closed, stopping watcher")
				return
			}
			log.Printf("[WATCH] Watcher error: %v", err)
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
