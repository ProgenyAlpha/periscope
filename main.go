package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/BurntSushi/toml"
	"github.com/fsnotify/fsnotify"
	"github.com/ProgenyAlpha/periscope/internal/anthropic"
	"github.com/ProgenyAlpha/periscope/internal/store"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// App holds all shared state for the periscope runtime.
type App struct {
	Config          AppConfig
	DB              *sql.DB
	Hub             *Hub
	Watcher         *fsnotify.Watcher
	AnthropicClient *anthropic.Client
	clientMu        sync.RWMutex // protects AnthropicClient
	HomeDir         string       // ~/.periscope
	ClaudeDir       string       // ~/.claude
	DataDir         string       // ~/.claude/hooks/cost-state
	PluginDir       string       // ~/.periscope/plugins
	cancel          context.CancelFunc // triggers graceful shutdown
}

type AppConfig struct {
	Server  ServerConfig `toml:"server"`
	DataDir string       `toml:"data_dir"` // override claude data dir
}

type ServerConfig struct {
	Port  int    `toml:"port"`
	Host  string `toml:"host"`
	Token string `toml:"token"`
}

func setupLogging(logPath string) {
	// Rotate if log file exceeds 5MB
	if stat, err := os.Stat(logPath); err == nil {
		if stat.Size() > 5*1024*1024 {
			os.Truncate(logPath, 0)
		}
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		slog.Warn("cannot open log file", "path", logPath, "err", err)
		return
	}

	multiWriter := io.MultiWriter(os.Stderr, logFile)
	handler := slog.NewTextHandler(multiWriter, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	slog.SetDefault(slog.New(handler))
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	slog.Info("periscope invoked", "version", Version, "command", os.Args[1])

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
		fmt.Println("periscope " + Version)
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
		slog.Info("loading config", "path", configPath)
		if _, err := toml.Decode(string(data), &app.Config); err != nil {
			slog.Warn("config parse error", "err", err)
		}
	} else {
		slog.Debug("no config.toml found, using defaults")
	}

	// Apply defaults
	if app.Config.Server.Port == 0 {
		app.Config.Server.Port = 8384
	}
	if app.Config.Server.Host == "" {
		app.Config.Server.Host = "127.0.0.1"
	}
	if app.Config.Server.Host == "0.0.0.0" || app.Config.Server.Host == "::" {
		slog.Warn("server binding to all interfaces — dashboard is network-accessible with no auth", "host", app.Config.Server.Host)
	}
	if app.Config.DataDir != "" {
		app.DataDir = app.Config.DataDir
		slog.Info("datadir override", "path", app.DataDir)
	}

	slog.Info("config loaded", "host", app.Config.Server.Host, "port", app.Config.Server.Port)
	return app, nil
}

func cmdInit() {
	app, err := newApp()
	if err != nil {
		slog.Error("init failed", "err", err)
		os.Exit(1)
	}
	if err := install(app); err != nil {
		slog.Error("install failed", "err", err)
		os.Exit(1)
	}
}

func cmdServe() {
	app, err := newApp()
	if err != nil {
		slog.Error("serve failed", "err", err)
		os.Exit(1)
	}

	// Check if already running
	resp, err := http.Get(fmt.Sprintf("http://%s:%d/api/health", app.Config.Server.Host, app.Config.Server.Port))
	if err == nil {
		resp.Body.Close()
		fmt.Printf("Periscope already running on port %d\n", app.Config.Server.Port)
		os.Exit(0)
	}

	// Set up log file with rotation
	logPath := filepath.Join(app.HomeDir, "periscope.log")
	setupLogging(logPath)
	slog.Info("logging initialized", "path", logPath)

	// Ensure initialized
	if _, err := os.Stat(app.PluginDir); os.IsNotExist(err) {
		slog.Info("first run detected, running init")
		fmt.Println("First run detected — running init...")
		if err := install(app); err != nil {
			slog.Error("install failed", "err", err)
			os.Exit(1)
		}
	}

	// Open DB
	dbPath := filepath.Join(app.HomeDir, "periscope.db")
	db, err := store.OpenDB(dbPath)
	if err != nil {
		slog.Error("could not open DB", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	app.DB = db

	// Initialize Anthropic client (optional — rate limit tracking needs OAuth)
	if client, err := anthropic.NewClientFromDisk(app.ClaudeDir); err == nil {
		app.AnthropicClient = client
		slog.Info("anthropic client initialized")
	} else {
		slog.Warn("anthropic client unavailable", "err", err)
	}

	// Import file data into DB
	slog.Info("importing file data")
	if err := store.ImportFileData(db, app.DataDir, app.ClaudeDir); err != nil {
		slog.Warn("data import error", "err", err)
	}

	// Compact limit history (tiered dedup: recent=dense, old=sparse)
	slog.Info("compacting limit history")
	store.CompactLimitHistory(db, app.DataDir)

	// Context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	app.cancel = cancel

	// Catch OS signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("received signal, initiating shutdown", "signal", sig)
		cancel()
	}()

	// Resolve auth token: env var takes priority over config
	if envToken := os.Getenv("PERISCOPE_TOKEN"); envToken != "" {
		app.Config.Server.Token = envToken
		slog.Info("auth token loaded from env var")
	} else if app.Config.Server.Token != "" {
		slog.Info("auth token loaded from config")
	}

	// Start WebSocket hub
	slog.Info("starting websocket hub")
	app.Hub = newHub()
	go app.Hub.run()

	// Start file watcher
	slog.Info("starting file watcher")
	app.Watcher, err = startWatcher(app)
	if err != nil {
		slog.Warn("file watcher error", "err", err)
	} else {
		defer app.Watcher.Close()
	}

	// Start HTTP server (blocks until shutdown)
	slog.Info("starting HTTP server", "host", app.Config.Server.Host, "port", app.Config.Server.Port)
	startServer(ctx, app)
	slog.Info("periscope stopped")
}

func cmdStatus() {
	app, err := newApp()
	if err != nil {
		slog.Error("status check failed", "err", err)
		os.Exit(1)
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
		slog.Error("uninstall failed", "err", err)
		os.Exit(1)
	}
	if err := uninstall(app); err != nil {
		slog.Error("uninstall failed", "err", err)
		os.Exit(1)
	}
}
