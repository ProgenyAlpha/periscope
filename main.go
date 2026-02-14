package main

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/fsnotify/fsnotify"
	"github.com/shawnwakeman/periscope/internal/store"
)

// App holds all shared state for the periscope runtime.
type App struct {
	Config    AppConfig
	DB        *sql.DB
	Hub       *Hub
	Watcher   *fsnotify.Watcher
	HomeDir   string // ~/.periscope
	ClaudeDir string // ~/.claude
	DataDir   string // ~/.claude/hooks/cost-state
	PluginDir string // ~/.periscope/plugins
}

type AppConfig struct {
	Server  ServerConfig `toml:"server"`
	DataDir string       `toml:"data_dir"` // override claude data dir
}

type ServerConfig struct {
	Port int    `toml:"port"`
	Host string `toml:"host"`
}

func setupLogging(logPath string) {
	// Rotate if log file exceeds 5MB
	if stat, err := os.Stat(logPath); err == nil {
		if stat.Size() > 5*1024*1024 {
			os.Truncate(logPath, 0)
		}
	}

	// Open log file
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("warning: cannot open log file %s: %v", logPath, err)
		return
	}

	// Write to both stderr and log file
	multiWriter := io.MultiWriter(os.Stderr, logFile)
	log.SetOutput(multiWriter)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	log.Printf("[MAIN] periscope v0.1.0 invoked: %s", os.Args[1])

	switch os.Args[1] {
	case "init":
		cmdInit()
	case "serve":
		cmdServe()
	case "status":
		cmdStatus()
	case "uninstall":
		cmdUninstall()
	case "hook":
		cmdHook()
	case "statusline":
		cmdStatusline()
	case "version":
		fmt.Println("periscope v0.1.0")
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`periscope — Claude Code telemetry dashboard

Usage:
  periscope init          Set up plugins, database, and hooks
  periscope serve         Start the dashboard server
  periscope status        Check if server is running
  periscope hook stop     Process transcript (StopTurn hook)
  periscope hook display  Output telemetry context (UserPromptSubmit hook)
  periscope statusline    Render terminal statusline (reads JSON from stdin)
  periscope uninstall     Remove hooks and clean up
  periscope version       Print version`)
}

func newApp() (*App, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot find home directory: %w", err)
	}

	app := &App{
		HomeDir:   filepath.Join(home, ".periscope"),
		ClaudeDir: filepath.Join(home, ".claude"),
		DataDir:   filepath.Join(home, ".claude", "hooks", "cost-state"),
		PluginDir: filepath.Join(home, ".periscope", "plugins"),
	}

	// Load config if it exists
	configPath := filepath.Join(app.HomeDir, "config.toml")
	if data, err := os.ReadFile(configPath); err == nil {
		log.Printf("[MAIN] Loading config from %s", configPath)
		if _, err := toml.Decode(string(data), &app.Config); err != nil {
			log.Printf("[MAIN] warning: config.toml parse error: %v", err)
		}
	} else {
		log.Printf("[MAIN] No config.toml found, using defaults")
	}

	// Apply defaults
	if app.Config.Server.Port == 0 {
		app.Config.Server.Port = 8384
	}
	if app.Config.Server.Host == "" {
		app.Config.Server.Host = "localhost"
	}
	if app.Config.DataDir != "" {
		app.DataDir = app.Config.DataDir
		log.Printf("[MAIN] DataDir override: %s", app.DataDir)
	}

	log.Printf("[MAIN] Config loaded: %s:%d", app.Config.Server.Host, app.Config.Server.Port)
	return app, nil
}

func cmdInit() {
	app, err := newApp()
	if err != nil {
		log.Fatal(err)
	}
	if err := install(app); err != nil {
		log.Fatal(err)
	}
}

func cmdServe() {
	app, err := newApp()
	if err != nil {
		log.Fatal(err)
	}

	// Set up log file with rotation
	logPath := filepath.Join(app.HomeDir, "periscope.log")
	setupLogging(logPath)
	log.Printf("[MAIN] Logging to %s", logPath)

	// Ensure initialized
	if _, err := os.Stat(app.PluginDir); os.IsNotExist(err) {
		log.Printf("[MAIN] First run detected, running init")
		fmt.Println("First run detected — running init...")
		if err := install(app); err != nil {
			log.Fatal(err)
		}
	}

	// 3. Open DB
	dbPath := filepath.Join(app.HomeDir, "periscope.db") // Changed periscopeDir to app.HomeDir for syntactic correctness
	db, err := store.OpenDB(dbPath)
	if err != nil {
		log.Fatalf("Fatal: could not open DB: %v", err)
	}
	defer db.Close()

	// Import file data into DB
	log.Printf("[MAIN] Importing file data")
	if err := store.ImportFileData(db, app.DataDir, app.ClaudeDir); err != nil { // Changed app.DB to db
		log.Printf("[MAIN] warning: data import: %v", err)
	}

	// Compact limit history (tiered dedup: recent=dense, old=sparse)
	log.Printf("[MAIN] Compacting limit history")
	compactLimitHistory(app)

	// Start WebSocket hub
	log.Printf("[MAIN] Starting WebSocket hub")
	app.Hub = newHub()
	go app.Hub.run()

	// Start file watcher
	log.Printf("[MAIN] Starting file watcher")
	app.Watcher, err = startWatcher(app)
	if err != nil {
		log.Printf("[MAIN] warning: file watcher: %v", err)
	} else {
		defer app.Watcher.Close()
	}

	// Start HTTP server
	log.Printf("[MAIN] Starting HTTP server on %s:%d", app.Config.Server.Host, app.Config.Server.Port)
	startServer(app)
}

func cmdStatus() {
	app, err := newApp()
	if err != nil {
		log.Fatal(err)
	}
	addr := fmt.Sprintf("http://%s:%d", app.Config.Server.Host, app.Config.Server.Port)
	fmt.Printf("Checking %s...\n", addr)

	// Try to hit the health endpoint
	resp, err := http.Get(addr + "/api/health")
	if err != nil {
		fmt.Println("Server is not running.")
		os.Exit(1)
	}
	defer resp.Body.Close()
	fmt.Println("Server is running.")
}

func cmdHook() {
	if len(os.Args) < 3 {
		fmt.Println("Usage: periscope hook [stop|display]")
		os.Exit(1)
	}
	switch os.Args[2] {
	case "stop":
		hookStop()
	case "display":
		hookDisplay()
	default:
		fmt.Printf("Unknown hook: %s\n", os.Args[2])
		os.Exit(1)
	}
}

func cmdUninstall() {
	app, err := newApp()
	if err != nil {
		log.Fatal(err)
	}
	if err := uninstall(app); err != nil {
		log.Fatal(err)
	}
}
