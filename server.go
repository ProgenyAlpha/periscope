package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ProgenyAlpha/periscope/internal/anthropic"
	"github.com/ProgenyAlpha/periscope/internal/pricing"
	"github.com/ProgenyAlpha/periscope/internal/store"
	"github.com/gorilla/websocket"
)

// --- WebSocket Hub ---

type Hub struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	mu         sync.RWMutex
}

type Client struct {
	hub      *Hub
	conn     *websocket.Conn
	send     chan []byte
	closeOne sync.Once
}

func (c *Client) closeSend() {
	c.closeOne.Do(func() { close(c.send) })
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte, 64),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			count := len(h.clients)
			h.mu.Unlock()
			slog.Info("ws client connected", "total", count)
		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				client.closeSend()
			}
			count := len(h.clients)
			h.mu.Unlock()
			slog.Info("ws client disconnected", "total", count)
		case message := <-h.broadcast:
			h.mu.Lock()
			clientCount := len(h.clients)
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					client.closeSend()
					delete(h.clients, client)
				}
			}
			h.mu.Unlock()
			slog.Debug("hub broadcast sent", "clients", clientCount, "bytes", len(message))
		}
	}
}

func (h *Hub) broadcastJSON(msgType string, payload any) {
	msg := map[string]any{"type": msgType, "payload": payload}
	data, err := json.Marshal(msg)
	if err != nil {
		slog.Error("hub marshal failed", "type", msgType, "err", err)
		return
	}
	h.mu.RLock()
	clientCount := len(h.clients)
	h.mu.RUnlock()
	slog.Debug("hub broadcasting", "type", msgType, "clients", clientCount)
	h.broadcast <- data
}

func (h *Hub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// originAllowed permits same-origin requests plus loopback, so the dashboard
// works on whatever address the server is bound to without widening
// cross-site access. An absent Origin (non-browser client) is allowed.
func originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Host == r.Host {
		return true
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		allowed := originAllowed(r)
		if !allowed {
			slog.Warn("ws upgrade rejected", "origin", r.Header.Get("Origin"), "host", r.Host)
		}
		return allowed
	},
}

func serveWS(app *App, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("ws upgrade failed", "err", err)
		return
	}

	client := &Client{hub: app.Hub, conn: conn, send: make(chan []byte, 64)}
	app.Hub.register <- client

	// Writer goroutine
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer func() {
			ticker.Stop()
			conn.Close()
		}()
		for {
			select {
			case message, ok := <-client.send:
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if !ok {
					conn.WriteMessage(websocket.CloseMessage, []byte{})
					return
				}
				if err := conn.WriteMessage(websocket.TextMessage, message); err != nil {
					slog.Debug("ws write error", "err", err)
					return
				}
			case <-ticker.C:
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					slog.Debug("ws ping failed", "err", err)
					return
				}
			}
		}
	}()

	// Reader goroutine (handles close + incoming messages)
	go func() {
		defer func() {
			app.Hub.unregister <- client
			conn.Close()
		}()
		conn.SetReadLimit(4096)
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			return nil
		})
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					slog.Debug("ws client closed normally")
				} else {
					slog.Debug("ws read error", "err", err)
				}
				break
			}
		}
	}()
}

// --- HTTP Server ---

func buildMux(app *App) *http.ServeMux {
	mux := http.NewServeMux()

	// Dashboard HTML
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("http request", "method", r.Method, "path", r.URL.Path)
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		serveDashboard(app, w, r)
	})

	// API routes
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("http request", "method", r.Method, "path", r.URL.Path)
		writeJSON(w, map[string]any{"ok": true, "clients": app.Hub.clientCount()})
	})

	mux.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("http request", "method", r.Method, "path", r.URL.Path)
		handleData(app, w, r)
	})

	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("http request", "method", r.Method, "path", r.URL.Path)
		handleConfig(app, w, r)
	})

	mux.HandleFunc("/api/usage", func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("http request", "method", r.Method, "path", r.URL.Path)
		handleUsage(app, w, r)
	})

	mux.HandleFunc("/api/pricing", func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("http request", "method", r.Method, "path", r.URL.Path)
		handlePricing(app, w, r)
	})

	mux.HandleFunc("/api/layout", func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("http request", "method", r.Method, "path", r.URL.Path)
		handleLayout(app, w, r)
	})

	mux.HandleFunc("/api/statusline", func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("http request", "method", r.Method, "path", r.URL.Path)
		handleStatuslineToggle(app, w, r)
	})

	mux.HandleFunc("/api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("http request", "method", r.Method, "path", r.URL.Path)
		if r.Method != "POST" {
			http.Error(w, "method not allowed", 405)
			return
		}
		slog.Info("shutdown requested via API")
		writeJSON(w, map[string]bool{"ok": true})
		// Trigger graceful shutdown via context cancellation
		if app.cancel != nil {
			app.cancel()
		}
	})

	// Push notification endpoints
	mux.HandleFunc("/api/push/public-key", func(w http.ResponseWriter, r *http.Request) {
		pub, _, err := ensureVAPIDKeys(app.DB)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, map[string]string{"publicKey": pub})
	})
	mux.HandleFunc("/api/push/subscribe", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", 405)
			return
		}
		var req struct {
			Endpoint string `json:"endpoint"`
			Keys     struct {
				Auth   string `json:"auth"`
				P256dh string `json:"p256dh"`
			} `json:"keys"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Endpoint == "" {
			writeError(w, 400, "invalid subscription")
			return
		}
		if err := store.PushSubscribe(app.DB, req.Endpoint, req.Keys.Auth, req.Keys.P256dh); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		slog.Info("push subscription added", "endpoint", req.Endpoint[:min(40, len(req.Endpoint))])
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/push/test", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", 405)
			return
		}
		if err := sendPushNotification(app.DB, "Periscope", "Test notification"); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	// PWA routes
	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		http.ServeFile(w, r, filepath.Join(app.PluginDir, "static", "manifest.json"))
	})
	mux.HandleFunc("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Service-Worker-Allowed", "/")
		http.ServeFile(w, r, filepath.Join(app.PluginDir, "static", "sw.js"))
	})
	mux.HandleFunc("/static/", func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		if strings.Contains(name, "..") || name == "." {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(app.PluginDir, "static", name))
	})

	// Plugin routes
	mux.HandleFunc("/api/plugins/", func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("http request", "method", r.Method, "path", r.URL.Path)
		handlePlugins(app, w, r)
	})

	// WebSocket
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("http request", "method", r.Method, "path", r.URL.Path, "upgrade", "websocket")
		serveWS(app, w, r)
	})

	return mux
}

func startServer(ctx context.Context, app *App) {
	mux := buildMux(app)

	// Background session-title backfill (off the Stop-hook critical path)
	go titleBackfill(ctx, app)

	// Background usage refresh with exponential backoff
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("polling goroutine panicked", "err", r)
			}
		}()

		const (
			// Headers-based polling can run aggressively because /v1/messages
			// is the path Claude Code itself uses — no special rate-limit budget.
			// Cost: ~$0.00001 per ping × cadence × devices, so even worst-case
			// 30s flat is ~$0.86/month/device. Graph fidelity wins.
			fastInterval    = 30 * time.Second
			defaultInterval = 60 * time.Second
			maxInterval     = 60 * time.Minute
		)
		backoff := defaultInterval

		slog.Info("polling started", "interval", defaultInterval)

		// Initial fetch on startup
		if result, err := fetchAndCacheUsage(app); err == nil {
			app.Hub.broadcastJSON("usage", json.RawMessage(result))
			store.AppendLimitSnapshot(app.DB, app.DataDir, result)
			slog.Info("initial usage fetch ok")
		} else {
			slog.Warn("initial usage fetch failed", "err", err)
		}

		var consecutiveErrors int
		var cycleCount int
		for {
			select {
			case <-ctx.Done():
				slog.Info("polling shutdown")
				return
			case <-time.After(backoff):
			}

			cycleCount++

			// Fetch Anthropic API usage — failure here must NOT block local work
			if result, err := fetchAndCacheUsage(app); err == nil {
				app.Hub.broadcastJSON("usage", json.RawMessage(result))
				store.AppendLimitSnapshot(app.DB, app.DataDir, result)
				// Check push notification thresholds
				var usage map[string]any
				if json.Unmarshal(result, &usage) == nil {
					checkAndNotify(app, usage)
				}
				if consecutiveErrors > 0 {
					slog.Info("usage fetch recovered", "previousErrors", consecutiveErrors)
				}
				consecutiveErrors = 0
				// Cross-device-aware cadence: tighten to fastInterval when
				// utilization is climbing in the snapshot history (any device
				// on this account is burning). Settle to defaultInterval when
				// utilization is flat (everyone idle).
				backoff = adaptivePollInterval(app.DataDir, fastInterval, defaultInterval)
				if cycleCount%10 == 0 {
					slog.Debug("poll heartbeat", "cycle", cycleCount, "nextInterval", backoff)
				}
			} else {
				consecutiveErrors++
				if anthropic.IsRateLimited(err) {
					if consecutiveErrors >= 3 {
						// Hard stop: Anthropic's usage API has ~5 req/token budget.
						// Each 429 retry resets their cooldown (~90min).
						// Stop retrying and wait for the max interval.
						backoff = maxInterval
						slog.Warn("usage API rate limited — backing off to max interval", "consecutive", consecutiveErrors, "nextRetry", backoff)
					} else {
						backoff = min(backoff*4, maxInterval)
					}
				} else {
					backoff = min(backoff*2, maxInterval)
				}
				if consecutiveErrors == 1 {
					slog.Warn("usage fetch failed", "consecutive", consecutiveErrors, "nextRetry", backoff, "err", err)
				}
			}

			// Re-import new JSONL lines + sidecars written by hooks (always runs)
			store.ImportJSONL(app.DB, filepath.Join(app.DataDir, "usage-history.jsonl"), "history")
			store.ImportSidecars(app.DB, app.DataDir)

			// Snapshot current sidecar states into history for continuous charting
			lastSessionSnapshotMu.Lock()
			store.SnapshotSidecarsToHistory(app.DB, lastSessionSnapshot)
			lastSessionSnapshotMu.Unlock()
		}
	}()

	// Health watchdog: monitors DB health, sends push on degradation
	go healthWatchdog(ctx, app)

	addr := fmt.Sprintf("%s:%d", app.Config.Server.Host, app.Config.Server.Port)
	slog.Info("server starting", "addr", addr)
	slog.Info("websocket endpoint", "addr", "ws://"+addr+"/ws")

	server := &http.Server{
		Addr:         addr,
		Handler:      authMiddleware(app.Config.Server.Token, rateLimitMiddleware(corsMiddleware(mux))),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Always keep a loopback listener alongside any non-local bind. The launcher
	// script, `periscope status`, and the hooks all reach the server over
	// localhost, and binding only to a remote address silently breaks them.
	var loopback *http.Server
	if h := app.Config.Server.Host; h != "localhost" && h != "127.0.0.1" && h != "" {
		loopback = &http.Server{
			Addr:         fmt.Sprintf("127.0.0.1:%d", app.Config.Server.Port),
			Handler:      server.Handler,
			ReadTimeout:  server.ReadTimeout,
			WriteTimeout: server.WriteTimeout,
			IdleTimeout:  server.IdleTimeout,
		}
		go func() {
			slog.Info("loopback listener starting", "addr", loopback.Addr)
			if err := loopback.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Warn("loopback listener stopped", "err", err)
			}
		}()
	}

	// Graceful shutdown: wait for context cancellation, then drain connections
	go func() {
		<-ctx.Done()
		slog.Info("graceful shutdown initiated")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if loopback != nil {
			loopback.Shutdown(shutdownCtx)
		}
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown error", "err", err)
		}
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server fatal error", "err", err)
		os.Exit(1)
	}
	slog.Info("server stopped")
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if originAllowed(r) {
			if origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}
		} else if origin != "" {
			slog.Warn("cors rejected", "origin", origin, "method", r.Method, "path", r.URL.Path)
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- Handlers ---

func serveDashboard(app *App, w http.ResponseWriter, r *http.Request) {
	// Serve plugin runtime shell
	runtimePath := filepath.Join(app.PluginDir, "runtime.html")
	if data, err := os.ReadFile(runtimePath); err == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
		return
	}

	slog.Warn("dashboard not found", "path", runtimePath)
	http.Error(w, "Dashboard not found — run 'periscope init' to extract plugins", 404)
}

func handleData(app *App, w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}

	// Re-import changed files before building response
	if err := store.ImportFileData(app.DB, app.DataDir, app.ClaudeDir); err != nil {
		slog.Warn("data import error", "err", err)
	}

	data, err := store.BuildDashboardData(app.DB, app.DataDir)
	if err != nil {
		slog.Error("data build error", "err", err)
		writeError(w, 500, err.Error())
		return
	}

	// Annotate the most recently captured live effort from any active session.
	annotateLiveEffort(data)

	// Side effect: refresh profile if stale
	go refreshProfileIfStale(app)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("data encode error", "err", err)
	}
}

func handleConfig(app *App, w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB max
	if err != nil {
		slog.Error("config read error", "err", err)
		writeError(w, 400, "cannot read body")
		return
	}

	// Validate JSON
	var test json.RawMessage
	if json.Unmarshal(body, &test) != nil {
		slog.Warn("config invalid JSON")
		writeError(w, 400, "invalid JSON")
		return
	}

	// Pretty-print and save
	var pretty json.RawMessage
	if err := json.Unmarshal(body, &pretty); err == nil {
		indented, _ := json.MarshalIndent(json.RawMessage(body), "", "  ")
		body = indented
	}

	configPath := filepath.Join(app.ClaudeDir, "statusline", "statusline-config.json")
	if err := os.WriteFile(configPath, body, 0644); err != nil {
		slog.Error("config write error", "err", err)
		writeError(w, 500, err.Error())
		return
	}

	// Update DB
	store.KVSet(app.DB, "config:statusline", string(body))
	slog.Info("config saved")

	writeJSON(w, map[string]bool{"ok": true})
}

func handleStatuslineToggle(app *App, w http.ResponseWriter, r *http.Request) {
	settingsPath := filepath.Join(app.ClaudeDir, "settings.json")

	if r.Method == "GET" {
		data, err := os.ReadFile(settingsPath)
		if err != nil {
			slog.Error("statusline read error", "err", err)
			writeJSON(w, map[string]any{"enabled": false, "error": err.Error()})
			return
		}
		var settings map[string]any
		if json.Unmarshal(data, &settings) != nil {
			writeJSON(w, map[string]any{"enabled": false})
			return
		}
		_, has := settings["statusLine"]
		writeJSON(w, map[string]any{"enabled": has})
		return
	}

	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}

	// Read current settings
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		slog.Error("statusline settings read error", "err", err)
		writeError(w, 500, "cannot read settings.json: "+err.Error())
		return
	}

	// Use ordered map to preserve key order
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		slog.Error("statusline settings parse error", "err", err)
		writeError(w, 500, "cannot parse settings.json: "+err.Error())
		return
	}

	// Parse request body for desired state
	var req struct {
		Enabled bool `json:"enabled"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, 400, "cannot read body")
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, 400, "invalid JSON body")
		return
	}

	if req.Enabled {
		binary, exeErr := os.Executable()
		if exeErr != nil {
			binaryName := "periscope"
			if runtime.GOOS == "windows" {
				binaryName = "periscope.exe"
			}
			binary = filepath.Join(app.HomeDir, binaryName)
		}
		cmd := map[string]string{"type": "command", "command": binary + " statusline"}
		cmdJSON, _ := json.Marshal(cmd)
		settings["statusLine"] = cmdJSON
		slog.Info("statusline enabled")
	} else {
		delete(settings, "statusLine")
		slog.Info("statusline disabled")
	}

	out, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.WriteFile(settingsPath, out, 0644); err != nil {
		slog.Error("statusline write error", "err", err)
		writeError(w, 500, "cannot write settings.json: "+err.Error())
		return
	}

	writeJSON(w, map[string]any{"ok": true, "enabled": req.Enabled})
}

var (
	lastManualSync time.Time
	manualSyncMu   sync.Mutex
)

func handleUsage(app *App, w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}

	// Cooldown: manual sync limited to once per 5 minutes to protect API budget
	manualSyncMu.Lock()
	since := time.Since(lastManualSync)
	if since < 5*time.Minute {
		manualSyncMu.Unlock()
		remaining := 5*time.Minute - since
		slog.Info("sync cooldown active", "remaining", remaining.Round(time.Second))
		// Serve cached data instead
		if cached := store.KVGet(app.DB, "cache:usage-api"); cached != nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Write(cached)
			return
		}
		writeError(w, 429, fmt.Sprintf("sync cooldown: %s remaining", remaining.Round(time.Second)))
		return
	}
	lastManualSync = time.Now()
	manualSyncMu.Unlock()

	result, err := fetchAndCacheUsage(app)
	if err != nil {
		slog.Error("usage fetch error", "err", err)
		// On rate limit or transient error, serve cached data instead of failing
		if cached := store.KVGet(app.DB, "cache:usage-api"); cached != nil {
			slog.Info("serving cached usage after API error")
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Write(cached)
			return
		}
		writeError(w, 500, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(result)

	// Push to WS clients
	app.Hub.broadcastJSON("usage", json.RawMessage(result))
}

func handlePricing(app *App, w http.ResponseWriter, r *http.Request) {
	result, err := pricing.FetchLiteLLMPricing(app.DataDir)
	if err != nil {
		slog.Error("pricing fetch error", "err", err)
		writeError(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(result)
}

func handleLayout(app *App, w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		raw := store.KVGet(app.DB, "config:layout")
		if raw == nil {
			writeJSON(w, nil)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write(raw)
	case "POST":
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			slog.Error("layout read error", "err", err)
			writeError(w, 400, "cannot read body")
			return
		}
		val := strings.TrimSpace(string(body))
		if val == "null" || val == "" {
			if _, err := app.DB.Exec("DELETE FROM kv WHERE key = ?", "config:layout"); err != nil {
				slog.Error("layout delete failed", "err", err)
			}
			slog.Info("layout cleared")
		} else {
			// Validate JSON before storing
			var test json.RawMessage
			if json.Unmarshal([]byte(val), &test) != nil {
				writeError(w, 400, "invalid JSON")
				return
			}
			store.KVSet(app.DB, "config:layout", val)
			slog.Info("layout saved")
		}
		writeJSON(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func handlePlugins(app *App, w http.ResponseWriter, r *http.Request) {
	// /api/plugins/{type} — list plugins
	// /api/plugins/{type}/{name} — get specific plugin file
	path := strings.TrimPrefix(r.URL.Path, "/api/plugins/")
	parts := strings.SplitN(path, "/", 2)

	pluginType := parts[0]
	validTypes := map[string]bool{
		"themes": true, "widgets": true, "pricing": true,
		"forecasters": true, "canvas": true, "vendor": true,
	}
	if !validTypes[pluginType] {
		slog.Warn("unknown plugin type", "type", pluginType)
		writeError(w, 404, "unknown plugin type")
		return
	}

	dir := filepath.Join(app.PluginDir, pluginType)

	if len(parts) == 1 || parts[1] == "" {
		// List plugins
		entries, err := os.ReadDir(dir)
		if err != nil {
			slog.Warn("plugin readdir error", "type", pluginType, "err", err)
			writeJSON(w, []string{})
			return
		}
		var names []string
		for _, e := range entries {
			if !e.IsDir() {
				names = append(names, e.Name())
			}
		}
		writeJSON(w, names)
		return
	}

	// Serve specific plugin — sanitize path traversal
	name := filepath.Base(parts[1])
	resolved := filepath.Join(dir, name)
	absDir, _ := filepath.Abs(dir)
	absPath, _ := filepath.Abs(resolved)
	if !strings.HasPrefix(strings.ToLower(absPath), strings.ToLower(absDir)+string(filepath.Separator)) {
		writeError(w, 400, "invalid filename")
		return
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		slog.Debug("plugin not found", "type", pluginType, "name", name, "err", err)
		writeError(w, 404, "plugin not found")
		return
	}

	// Set content type based on extension
	switch {
	case strings.HasSuffix(name, ".toml"):
		w.Header().Set("Content-Type", "application/toml; charset=utf-8")
	case strings.HasSuffix(name, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Write(data)
}

// lastSessionSnapshot tracks per-session cost for dedup in snapshotSidecarsToHistory
var (
	lastSessionSnapshot   = map[string]float64{}
	lastSessionSnapshotMu sync.RWMutex
)

// adaptivePollInterval returns the next polling interval based on whether
// utilization is climbing in the recent snapshot history.
//
// "Climbing" means pct5hr or pctWeekly increased between the last two
// non-overlapping snapshots — which captures cross-device burn (e.g.
// you're on a different machine). When that happens we tighten to `fast`
// so dormant devices still see fresh data. When utilization is flat we
// settle to `slow` to keep ping costs minimal.
//
// With /v1/messages-header polling, this is purely a cost dial — there's
// no Anthropic-side rate-limit budget to protect anymore.
func adaptivePollInterval(dataDir string, fast, slow time.Duration) time.Duration {
	histPath := filepath.Join(dataDir, "limit-history.jsonl")
	f, err := os.Open(histPath)
	if err != nil {
		return slow
	}
	defer f.Close()

	// Read tail (~last 16 entries) so we can find the last two distinct snapshots.
	fi, err := f.Stat()
	if err != nil {
		return slow
	}
	const tailBytes = 4096
	off := fi.Size() - tailBytes
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return slow
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return slow
	}

	type snap struct {
		ts     time.Time
		pct5hr int
		pctWk  int
	}
	var snaps []snap
	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var e struct {
			TS    string  `json:"ts"`
			Pct5  float64 `json:"pct5hr"`
			PctWk float64 `json:"pctWeekly"`
		}
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, e.TS)
		if err != nil {
			continue
		}
		snaps = append(snaps, snap{ts: t, pct5hr: int(e.Pct5), pctWk: int(e.PctWk)})
	}
	if len(snaps) < 2 {
		return slow
	}

	// Compare most recent snapshot against the most recent one >=2min older
	// (avoids false positives from the same poll appearing twice in history).
	last := snaps[len(snaps)-1]
	var prev *snap
	for i := len(snaps) - 2; i >= 0; i-- {
		if last.ts.Sub(snaps[i].ts) >= 2*time.Minute {
			prev = &snaps[i]
			break
		}
	}
	if prev == nil {
		return slow
	}
	if last.pct5hr > prev.pct5hr || last.pctWk > prev.pctWk {
		return fast
	}
	return slow
}

// lastOAuthUsageFetch tracks when /api/oauth/usage was last called so the
// hourly refresh schedule can stay well under the ~5-call-per-90min budget.
var (
	lastOAuthUsageFetch time.Time
	oauthFetchMu        sync.Mutex
)

// annotateLiveEffort sets data.LiveEffort to the most recently captured
// effort level from ~/.periscope/effort/<sid>.json (written by cmdStatusline
// on every Claude Code statusline render). Newest mtime wins.
//
// Called from BOTH the GET /api/data handler and the watcher's broadcast
// path so the dashboard sees a consistent value whether it loaded fresh or
// is taking a websocket push.
func annotateLiveEffort(data *store.DashboardData) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	effortDir := filepath.Join(home, ".periscope", "effort")
	entries, err := os.ReadDir(effortDir)
	if err != nil {
		return
	}
	var newestMtime time.Time
	var newestLevel string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestMtime) {
			raw, err := os.ReadFile(filepath.Join(effortDir, e.Name()))
			if err != nil {
				continue
			}
			var live struct {
				Level string `json:"level"`
			}
			if json.Unmarshal(raw, &live) == nil && live.Level != "" {
				newestMtime = info.ModTime()
				newestLevel = live.Level
			}
		}
	}
	if newestLevel != "" {
		data.LiveEffort = newestLevel
	}
}

// needsOAuthRefresh returns true if /api/oauth/usage should run this cycle:
// either it hasn't run in over an hour (so the static `monthly_limit` cap
// stays fresh) or no cap is cached yet (fresh install / first poll).
func needsOAuthRefresh(app *App) bool {
	const refreshInterval = time.Hour
	oauthFetchMu.Lock()
	t := lastOAuthUsageFetch
	oauthFetchMu.Unlock()
	if !t.IsZero() && time.Since(t) < refreshInterval {
		return false
	}
	return true
}

// supplementWithCachedExtraUsage augments a fresh header-derived usage map
// with the static `extra_usage` fields (is_enabled, monthly_limit) preserved
// from the previous cache, deriving `used_credits` from the live overage
// utilization that headers DO report. Also preserves sonnet/opus utilization
// from the previous cache when headers omit those windows (Anthropic only
// returns 7d_sonnet/7d_opus in headers when those models were recently used,
// so a sonnet-quiet header response would otherwise blank the dashboard
// segment to -1).
// resetInFuture reports whether an RFC3339 reset timestamp is still ahead of
// now. An unparseable or empty value counts as expired.
func resetInFuture(s string) bool {
	if s == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return false
	}
	return t.After(time.Now())
}

// preserveScopedIfMissing carries per-model weekly caps across header pings,
// which never report them — only /api/oauth/usage does, and that runs hourly.
// Entries past their reset are dropped rather than carried.
func preserveScopedIfMissing(fresh, prevMap map[string]any) {
	if _, ok := fresh["scoped"]; ok {
		return
	}
	prevScoped, ok := prevMap["scoped"].([]any)
	if !ok {
		return
	}
	var live []any
	for _, e := range prevScoped {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if r, _ := m["reset"].(string); resetInFuture(r) {
			live = append(live, m)
		}
	}
	if len(live) > 0 {
		fresh["scoped"] = live
	}
}

// spendMaxAge bounds how long a cached `spend` value may be carried forward.
// spend has no reset timestamp of its own (unlike scoped weekly caps), so
// staleness is bounded by age instead, using the spend_fetched_at recorded
// alongside it.
const spendMaxAge = 24 * time.Hour

// preserveSpendIfMissing carries the `spend` object across header pings,
// which never report it — only /api/oauth/usage does. Dropped once older
// than spendMaxAge rather than carried indefinitely.
func preserveSpendIfMissing(fresh, prevMap map[string]any) {
	if _, ok := fresh["spend"]; ok {
		return
	}
	prevSpend, ok := prevMap["spend"]
	if !ok {
		return
	}
	fetchedAt, ok := prevMap["spend_fetched_at"].(float64)
	if !ok {
		return
	}
	if time.Since(time.Unix(int64(fetchedAt), 0)) > spendMaxAge {
		return
	}
	fresh["spend"] = prevSpend
	fresh["spend_fetched_at"] = prevMap["spend_fetched_at"]
}

func supplementWithCachedExtraUsage(app *App, fresh map[string]any) {
	prev := store.KVGet(app.DB, "cache:usage-api")
	var prevMap map[string]any
	if prev != nil {
		_ = json.Unmarshal(prev, &prevMap)
	}

	// Preserve per-window utilization that headers may legitimately omit.
	// pctSonnet/pctOpus default to -1 when the corresponding header pair
	// is missing — fall back to the last good value rather than blank.
	// TransformUsage stores these as int; cache reads come back as float64
	// after json round-trip, so accept either.
	asNumber := func(v any) (float64, bool) {
		switch n := v.(type) {
		case float64:
			return n, true
		case int:
			return float64(n), true
		case int64:
			return float64(n), true
		}
		return 0, false
	}
	if prevMap != nil {
		preserveIfMissing := func(pctKey, resetKey string) {
			cur, ok := asNumber(fresh[pctKey])
			if ok && cur >= 0 {
				return
			}
			prevPct, ok := asNumber(prevMap[pctKey])
			if !ok || prevPct < 0 {
				return
			}
			// Only carry a window forward while its own reset is still ahead.
			// Anthropic stopped reporting seven_day_sonnet on 2026-06-30; an
			// unconditional carry-forward pinned that dead 22% to the
			// statusline for six weeks.
			resetStr, _ := prevMap[resetKey].(string)
			if !resetInFuture(resetStr) {
				return
			}
			fresh[pctKey] = prevPct
			fresh[resetKey] = resetStr
		}
		preserveIfMissing("pctSonnet", "resetSonnet")
		preserveIfMissing("pctOpus", "resetOpus")
		preserveScopedIfMissing(fresh, prevMap)
		preserveSpendIfMissing(fresh, prevMap)
	}

	if _, ok := fresh["extra_usage"].(map[string]any); ok {
		return // oauth/usage path already populated this fully
	}
	overageUtil, hasOverage := fresh["overage_utilization"].(float64)
	if !hasOverage {
		return // headers didn't carry overage; nothing to derive
	}
	if prevMap == nil {
		return
	}
	prevEU, ok := prevMap["extra_usage"].(map[string]any)
	if !ok {
		return
	}
	cap_, capOk := prevEU["monthly_limit"].(float64)
	enabled, _ := prevEU["is_enabled"].(bool)
	merged := map[string]any{
		"is_enabled": enabled,
	}
	if capOk {
		merged["monthly_limit"] = cap_
		merged["used_credits"] = cap_ * overageUtil
	}
	merged["utilization"] = overageUtil
	fresh["extra_usage"] = merged
}

// fetchAndCacheUsage fetches usage from the Anthropic API, caches result to DB and file.
func fetchAndCacheUsage(app *App) (json.RawMessage, error) {
	app.clientMu.RLock()
	client := app.AnthropicClient
	app.clientMu.RUnlock()

	if client == nil {
		// Re-try loading client (token may have been created since startup)
		newClient, err := anthropic.NewClientFromDisk(app.ClaudeDir)
		if err != nil {
			return nil, err
		}
		app.clientMu.Lock()
		app.AnthropicClient = newClient
		client = newClient
		app.clientMu.Unlock()
	}

	// Primary path: read rate-limit headers from a /v1/messages ping. This
	// avoids /api/oauth/usage's tight 5-req-per-90min budget and gives the
	// same five_hour / seven_day / seven_day_sonnet utilization data per
	// Claude Code issue #12829. One ping costs ~$0.00001.
	//
	// Fallback to /api/oauth/usage runs (a) on header failure and (b) on the
	// hourly schedule so the static `extra_usage.monthly_limit` cap stays
	// fresh for derive-used-credits-from-overage-utilization.
	useOAuth := needsOAuthRefresh(app)
	var resp *anthropic.APIResponse
	var err error
	if !useOAuth {
		resp, err = client.FetchUsageFromHeaders()
		if err != nil {
			slog.Debug("header ping failed, falling back to oauth/usage", "err", err)
			resp, err = client.FetchUsage()
			if err == nil {
				oauthFetchMu.Lock()
				lastOAuthUsageFetch = time.Now()
				oauthFetchMu.Unlock()
			}
		}
	} else {
		resp, err = client.FetchUsage()
		if err == nil {
			oauthFetchMu.Lock()
			lastOAuthUsageFetch = time.Now()
			oauthFetchMu.Unlock()
		} else {
			slog.Debug("oauth/usage refresh failed, falling back to header ping", "err", err)
			resp, err = client.FetchUsageFromHeaders()
		}
	}
	if err != nil {
		// On auth error, try reloading token (may have been refreshed)
		if anthropic.IsAuthError(err) {
			newClient, reloadErr := anthropic.NewClientFromDisk(app.ClaudeDir)
			if reloadErr == nil {
				app.clientMu.Lock()
				app.AnthropicClient = newClient
				app.clientMu.Unlock()
				resp, err = newClient.FetchUsageFromHeaders()
				if err != nil {
					resp, err = newClient.FetchUsage()
				}
				if err != nil {
					return nil, err
				}
			} else {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	usage := anthropic.TransformUsage(resp)
	applyRateLimitHints(usage, app.DataDir)
	if resp.OverageUtilization != nil {
		usage["overage_utilization"] = *resp.OverageUtilization
	}
	supplementWithCachedExtraUsage(app, usage)
	result, _ := json.Marshal(usage)

	// Cache to DB and file
	store.KVSet(app.DB, "cache:usage-api", string(result))
	if err := os.WriteFile(filepath.Join(app.DataDir, "usage-api-cache.json"), result, 0644); err != nil {
		slog.Warn("usage cache write failed", "err", err)
	}

	return result, nil
}

// refreshProfileIfStale fetches profile from API if cache is >5 min old.
func refreshProfileIfStale(app *App) {
	raw := store.KVGet(app.DB, "cache:profile")
	if raw != nil {
		var p map[string]any
		if json.Unmarshal(raw, &p) == nil {
			if fetched, ok := p["fetched_at"].(float64); ok {
				if time.Since(time.Unix(int64(fetched), 0)) < 5*time.Minute {
					return
				}
			}
		}
	}
	fetchAndCacheProfile(app)
}

// fetchAndCacheProfile fetches profile from API, transforms, caches.
func fetchAndCacheProfile(app *App) {
	app.clientMu.RLock()
	client := app.AnthropicClient
	app.clientMu.RUnlock()
	if client == nil {
		return
	}
	apiResp, err := client.FetchProfile()
	if err != nil {
		return
	}

	profile := map[string]any{
		"fetched_at": time.Now().Unix(),
	}
	if acct, ok := apiResp["account"].(map[string]any); ok {
		profile["name"], _ = acct["full_name"]
		profile["email"], _ = acct["email"]
		if v, ok := acct["has_claude_max"].(bool); ok {
			profile["has_claude_max"] = v
		}
		if v, ok := acct["has_claude_pro"].(bool); ok {
			profile["has_claude_pro"] = v
		}
		if v, ok := acct["created_at"].(string); ok {
			profile["created_at"] = v
		}
		if v, ok := acct["display_name"].(string); ok {
			profile["display_name"] = v
		}
	}
	if org, ok := apiResp["organization"].(map[string]any); ok {
		profile["subscription"], _ = org["organization_type"]
		profile["tier"], _ = org["rate_limit_tier"]
		profile["org"], _ = org["name"]
		profile["status"], _ = org["subscription_status"]
		if v, ok := org["has_extra_usage_enabled"].(bool); ok {
			profile["has_extra_usage_enabled"] = v
		}
		if v, ok := org["billing_type"].(string); ok {
			profile["billing_type"] = v
		}
		if v, ok := org["uuid"].(string); ok {
			profile["org_uuid"] = v
		}
	}

	result, _ := json.Marshal(profile)
	store.KVSet(app.DB, "cache:profile", string(result))
	if err := os.WriteFile(filepath.Join(app.DataDir, "profile-cache.json"), result, 0600); err != nil {
		slog.Warn("profile cache write failed", "err", err)
	}
}

// --- Middleware ---

// titleBackfill generates session titles asynchronously, off the Stop-hook
// critical path. It scans sidecars for sessions with >=5 turns and no title,
// re-extracts prompts from the transcript, and calls Haiku via the OAuth token.
// Bounded per cycle and to recent sessions so a first run can't burst the API.
// Titles only generate while `periscope serve` is running (the normal mode).
func titleBackfill(ctx context.Context, app *App) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("title backfill goroutine panicked", "err", r)
		}
	}()
	const (
		interval     = 60 * time.Second
		initialDelay = 20 * time.Second
		maxPerCycle  = 2
		maxAge       = 7 * 24 * time.Hour
	)
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		generated := 0
		if entries, err := os.ReadDir(app.DataDir); err == nil {
			for _, e := range entries {
				if generated >= maxPerCycle {
					break
				}
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || store.SidecarExclude[e.Name()] {
					continue
				}
				fpath := filepath.Join(app.DataDir, e.Name())
				info, err := os.Stat(fpath)
				if err != nil || time.Since(info.ModTime()) > maxAge {
					continue
				}
				raw, err := os.ReadFile(fpath)
				if err != nil {
					continue
				}
				var state SidecarState
				if json.Unmarshal(store.StripBOM(raw), &state) != nil || state.Cumulative == nil {
					continue
				}
				if state.GeneratedTitle != "" || state.TranscriptPath == "" {
					continue
				}
				totalCalls := state.Cumulative.AgentCalls + state.Cumulative.ToolCalls + state.Cumulative.ChatCalls
				if totalCalls < 5 {
					continue
				}
				prompts := extractUserPrompts(state.TranscriptPath, 3)
				if len(prompts) < 2 {
					continue
				}
				generateSessionTitle(fpath, state.Project, prompts)
				generated++
			}
		}

		timer.Reset(interval)
	}
}

func authMiddleware(token string, next http.Handler) http.Handler {
	if token == "" {
		return next // auth disabled — backward compatible
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for health check, dashboard HTML, and PWA assets
		if r.URL.Path == "/api/health" || r.URL.Path == "/" ||
			r.URL.Path == "/manifest.json" || r.URL.Path == "/sw.js" ||
			strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		// Check bearer token header
		authHeader := r.Header.Get("Authorization")
		expected := "Bearer " + token
		if len(authHeader) == len(expected) && subtle.ConstantTimeCompare([]byte(authHeader), []byte(expected)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		// Check query param (for WebSocket — browsers can't set custom headers on WS upgrade)
		if r.URL.Query().Get("token") == token {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, 401, "unauthorized")
	})
}

type rateLimiter struct {
	mu       sync.Mutex
	tokens   float64
	lastTime time.Time
	rate     float64 // tokens per second
	burst    float64 // max tokens
}

func newRateLimiter(ratePerMin, burst float64) *rateLimiter {
	return &rateLimiter{
		tokens:   burst,
		lastTime: time.Now(),
		rate:     ratePerMin / 60.0,
		burst:    burst,
	}
}

func (rl *rateLimiter) reset() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.tokens = rl.burst
	rl.lastTime = time.Now()
}

func (rl *rateLimiter) allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(rl.lastTime).Seconds()
	rl.tokens = min(rl.burst, rl.tokens+elapsed*rl.rate)
	rl.lastTime = now
	if rl.tokens < 1 {
		return false
	}
	rl.tokens--
	return true
}

var (
	externalLimiter = newRateLimiter(10, 3)   // /api/usage, /api/pricing — hits external APIs
	generalLimiter  = newRateLimiter(120, 10) // everything else
)

func rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip rate limiting for local-only routes (no external API calls)
		if r.URL.Path == "/" || r.URL.Path == "/ws" ||
			strings.HasPrefix(r.URL.Path, "/api/plugins/") ||
			strings.HasPrefix(r.URL.Path, "/static/") ||
			r.URL.Path == "/manifest.json" || r.URL.Path == "/sw.js" ||
			r.URL.Path == "/api/health" || r.URL.Path == "/api/data" ||
			r.URL.Path == "/api/layout" || r.URL.Path == "/api/config" {
			next.ServeHTTP(w, r)
			return
		}
		var limiter *rateLimiter
		switch r.URL.Path {
		case "/api/usage", "/api/pricing":
			limiter = externalLimiter
		default:
			limiter = generalLimiter
		}
		if !limiter.allow() {
			w.Header().Set("Retry-After", "5")
			writeError(w, 429, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- Health Watchdog ---

// healthWatchdog monitors internal health and sends push notifications on degradation.
func healthWatchdog(ctx context.Context, app *App) {
	// Notify subscribers that the server (re)started
	time.Sleep(5 * time.Second) // let push subscriptions load
	sendPushNotification(app.DB, "Periscope", "Server started")

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	consecutiveDBFailures := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Check DB accessibility
		var n int
		err := app.DB.QueryRow("SELECT 1").Scan(&n)
		if err != nil {
			consecutiveDBFailures++
			slog.Error("health: DB check failed", "consecutive", consecutiveDBFailures, "err", err)
			if consecutiveDBFailures == 1 || consecutiveDBFailures%12 == 0 { // first failure + every hour
				sendPushNotification(app.DB, "Periscope", fmt.Sprintf("DB health check failed (%dx)", consecutiveDBFailures))
			}
			continue
		}

		if consecutiveDBFailures > 0 {
			slog.Info("health: DB recovered", "previousFailures", consecutiveDBFailures)
			sendPushNotification(app.DB, "Periscope", fmt.Sprintf("DB recovered after %d failures", consecutiveDBFailures))
			consecutiveDBFailures = 0
		}
	}
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
