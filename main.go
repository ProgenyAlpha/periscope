package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/fsnotify/fsnotify"
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

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

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
		if _, err := toml.Decode(string(data), &app.Config); err != nil {
			log.Printf("warning: config.toml parse error: %v", err)
		}
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
	}

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
	fmt.Println("Periscope initialized. Run 'periscope serve' to start.")
}

func cmdServe() {
	app, err := newApp()
	if err != nil {
		log.Fatal(err)
	}

	// Ensure initialized
	if _, err := os.Stat(app.PluginDir); os.IsNotExist(err) {
		fmt.Println("First run detected — running init...")
		if err := install(app); err != nil {
			log.Fatal(err)
		}
	}

	// Open database
	dbPath := filepath.Join(app.HomeDir, "periscope.db")
	app.DB, err = openDB(dbPath)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer app.DB.Close()

	// Import file data into DB
	if err := importFileData(app); err != nil {
		log.Printf("warning: data import: %v", err)
	}

	// Start WebSocket hub
	app.Hub = newHub()
	go app.Hub.run()

	// Start file watcher
	app.Watcher, err = startWatcher(app)
	if err != nil {
		log.Printf("warning: file watcher: %v", err)
	} else {
		defer app.Watcher.Close()
	}

	// Start HTTP server
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
	resp, err := httpGet(addr + "/api/health")
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
	fmt.Println("Periscope uninstalled.")
}
