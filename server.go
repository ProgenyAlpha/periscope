package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
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
			log.Printf("[WS] Client connected (total: %d)", count)
		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			count := len(h.clients)
			h.mu.Unlock()
			log.Printf("[WS] Client disconnected (total: %d)", count)
		case message := <-h.broadcast:
			h.mu.Lock()
			clientCount := len(h.clients)
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					close(client.send)
					delete(h.clients, client)
				}
			}
			h.mu.Unlock()
			log.Printf("[HUB] Broadcast to %d clients (%d bytes)", clientCount, len(message))
		}
	}
}

func (h *Hub) broadcastJSON(msgType string, payload any) {
	msg := map[string]any{"type": msgType, "payload": payload}
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("[HUB] Failed to marshal broadcast (type=%s): %v", msgType, err)
		return
	}
	h.mu.RLock()
	clientCount := len(h.clients)
	h.mu.RUnlock()
	log.Printf("[HUB] Broadcasting %s to %d client(s)", msgType, clientCount)
	h.broadcast <- data
}

func (h *Hub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		allowed := origin == "" || strings.HasPrefix(origin, "http://localhost") || strings.HasPrefix(origin, "http://127.0.0.1")
		if !allowed {
			log.Printf("[CORS] WebSocket upgrade rejected from origin: %s", origin)
		}
		return allowed
	},
}

func serveWS(app *App, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WS] Upgrade failed: %v", err)
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
					log.Printf("[WS] Write error: %v", err)
					return
				}
			case <-ticker.C:
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					log.Printf("[WS] Ping failed: %v", err)
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
					log.Printf("[WS] Client closed connection normally")
				} else {
					log.Printf("[WS] Read error: %v", err)
				}
				break
			}
		}
	}()
}

// --- HTTP Server ---

func startServer(app *App) {
	mux := http.NewServeMux()

	// Dashboard HTML
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[HTTP] %s %s", r.Method, r.URL.Path)
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		serveDashboard(app, w, r)
	})

	// API routes
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[HTTP] %s %s", r.Method, r.URL.Path)
		writeJSON(w, map[string]any{"ok": true, "clients": app.Hub.clientCount()})
	})

	mux.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[HTTP] %s %s", r.Method, r.URL.Path)
		handleData(app, w, r)
	})

	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[HTTP] %s %s", r.Method, r.URL.Path)
		handleConfig(app, w, r)
	})

	mux.HandleFunc("/api/usage", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[HTTP] %s %s", r.Method, r.URL.Path)
		handleUsage(app, w, r)
	})

	mux.HandleFunc("/api/pricing", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[HTTP] %s %s", r.Method, r.URL.Path)
		handlePricing(app, w, r)
	})

	mux.HandleFunc("/api/layout", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[HTTP] %s %s", r.Method, r.URL.Path)
		handleLayout(app, w, r)
	})

	mux.HandleFunc("/api/statusline", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[HTTP] %s %s", r.Method, r.URL.Path)
		handleStatuslineToggle(app, w, r)
	})

	mux.HandleFunc("/api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[HTTP] %s %s", r.Method, r.URL.Path)
		if r.Method != "POST" {
			http.Error(w, "method not allowed", 405)
			return
		}
		log.Printf("[HTTP] Shutdown requested")
		writeJSON(w, map[string]bool{"ok": true})
		go func() {
			time.Sleep(100 * time.Millisecond)
			os.Exit(0)
		}()
	})

	// Plugin routes
	mux.HandleFunc("/api/plugins/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[HTTP] %s %s", r.Method, r.URL.Path)
		handlePlugins(app, w, r)
	})

	// WebSocket
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[HTTP] %s %s (WebSocket upgrade)", r.Method, r.URL.Path)
		serveWS(app, w, r)
	})

	// Background usage refresh — fetch from Anthropic API every 30s, push via WS
	// Also re-import JSONL history files so hook-written data appears in real-time
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[FATAL] polling goroutine panicked: %v", r)
			}
		}()
		// Initial fetch on startup
		log.Printf("[POLL] Starting background polling (interval: 30s)")
		if result, err := fetchUsage(app); err == nil {
			app.Hub.broadcastJSON("usage", json.RawMessage(result))
			appendLimitSnapshot(app, result)
			log.Printf("[POLL] Initial usage fetch successful")
		} else {
			log.Printf("[WARN] initial fetchUsage failed: %v", err)
		}
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		var consecutiveErrors int
		var cycleCount int
		for range ticker.C {
			cycleCount++
			// Fetch Anthropic API usage — failure here must NOT block local work
			if result, err := fetchUsage(app); err == nil {
				app.Hub.broadcastJSON("usage", json.RawMessage(result))
				appendLimitSnapshot(app, result)
				if consecutiveErrors > 0 {
					log.Printf("[POLL] fetchUsage recovered after %d consecutive errors", consecutiveErrors)
					consecutiveErrors = 0
				}
				// Log every 10th successful cycle as a heartbeat
				if cycleCount%10 == 0 {
					log.Printf("[POLL] Heartbeat: cycle %d, fetch OK", cycleCount)
				}
			} else {
				consecutiveErrors++
				if consecutiveErrors == 1 || consecutiveErrors%60 == 0 {
					log.Printf("[WARN] fetchUsage failed (%dx consecutive): %v", consecutiveErrors, err)
				}
			}

			// Re-import new JSONL lines + sidecars written by hooks
			importJSONL(app, "usage-history.jsonl", "history")
			importSidecars(app)

			// Snapshot current sidecar states into history for continuous charting
			snapshotSidecarsToHistory(app)
		}
	}()

	addr := fmt.Sprintf("%s:%d", app.Config.Server.Host, app.Config.Server.Port)
	log.Printf("[HTTP] Server starting on http://%s", addr)
	log.Printf("[HTTP] WebSocket endpoint: ws://%s/ws", addr)

	server := &http.Server{
		Addr:         addr,
		Handler:      corsMiddleware(mux),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("[HTTP] Server fatal error: %v", err)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" || strings.HasPrefix(origin, "http://localhost") || strings.HasPrefix(origin, "http://127.0.0.1") {
			if origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}
		} else if origin != "" {
			log.Printf("[CORS] Rejected origin: %s for %s %s", origin, r.Method, r.URL.Path)
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
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

	log.Printf("[HTTP] Dashboard not found at %s", runtimePath)
	http.Error(w, "Dashboard not found — run 'periscope init' to extract plugins", 404)
}

func handleData(app *App, w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}

	// Re-import changed files before building response
	if err := importFileData(app); err != nil {
		log.Printf("[HTTP] /api/data import error: %v", err)
	}

	data, err := buildDashboardData(app)
	if err != nil {
		log.Printf("[HTTP] /api/data build error: %v", err)
		writeError(w, 500, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("[HTTP] /api/data encode error: %v", err)
	}
}

func handleConfig(app *App, w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB max
	if err != nil {
		log.Printf("[HTTP] /api/config read error: %v", err)
		writeError(w, 400, "cannot read body")
		return
	}

	// Validate JSON
	var test json.RawMessage
	if json.Unmarshal(body, &test) != nil {
		log.Printf("[HTTP] /api/config invalid JSON")
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
		log.Printf("[HTTP] /api/config write error: %v", err)
		writeError(w, 500, err.Error())
		return
	}

	// Update DB
	kvSet(app.DB, "config:statusline", string(body))
	log.Printf("[HTTP] /api/config saved successfully")

	writeJSON(w, map[string]bool{"ok": true})
}

func handleStatuslineToggle(app *App, w http.ResponseWriter, r *http.Request) {
	settingsPath := filepath.Join(app.ClaudeDir, "settings.json")

	if r.Method == "GET" {
		data, err := os.ReadFile(settingsPath)
		if err != nil {
			log.Printf("[HTTP] /api/statusline read error: %v", err)
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
		log.Printf("[HTTP] /api/statusline read error: %v", err)
		writeError(w, 500, "cannot read settings.json: "+err.Error())
		return
	}

	// Use ordered map to preserve key order
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		log.Printf("[HTTP] /api/statusline parse error: %v", err)
		writeError(w, 500, "cannot parse settings.json: "+err.Error())
		return
	}

	// Parse request body for desired state
	var req struct {
		Enabled bool `json:"enabled"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	json.Unmarshal(body, &req)

	if req.Enabled {
		binary := filepath.Join(app.HomeDir, "periscope.exe")
		cmd := map[string]string{"type": "command", "command": binary + " statusline"}
		cmdJSON, _ := json.Marshal(cmd)
		settings["statusLine"] = cmdJSON
		log.Printf("[HTTP] /api/statusline enabled")
	} else {
		delete(settings, "statusLine")
		log.Printf("[HTTP] /api/statusline disabled")
	}

	out, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.WriteFile(settingsPath, out, 0644); err != nil {
		log.Printf("[HTTP] /api/statusline write error: %v", err)
		writeError(w, 500, "cannot write settings.json: "+err.Error())
		return
	}

	writeJSON(w, map[string]any{"ok": true, "enabled": req.Enabled})
}

func handleUsage(app *App, w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}

	result, err := fetchUsage(app)
	if err != nil {
		log.Printf("[HTTP] /api/usage fetch error: %v", err)
		writeError(w, 500, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(result)

	// Push to WS clients
	app.Hub.broadcastJSON("usage", json.RawMessage(result))
}

func handlePricing(app *App, w http.ResponseWriter, r *http.Request) {
	result, err := fetchPricing(app)
	if err != nil {
		log.Printf("[HTTP] /api/pricing fetch error: %v", err)
		writeError(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(result)
}

func handleLayout(app *App, w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		raw := kvGet(app.DB, "config:layout")
		if raw == nil {
			writeJSON(w, nil)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write(raw)
	case "POST":
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			log.Printf("[HTTP] /api/layout read error: %v", err)
			writeError(w, 400, "cannot read body")
			return
		}
		val := strings.TrimSpace(string(body))
		if val == "null" || val == "" {
			app.DB.Exec("DELETE FROM kv WHERE key = ?", "config:layout")
			log.Printf("[HTTP] /api/layout cleared")
		} else {
			kvSet(app.DB, "config:layout", val)
			log.Printf("[HTTP] /api/layout saved")
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
		log.Printf("[HTTP] /api/plugins unknown type: %s", pluginType)
		writeError(w, 404, "unknown plugin type")
		return
	}

	dir := filepath.Join(app.PluginDir, pluginType)

	if len(parts) == 1 || parts[1] == "" {
		// List plugins
		entries, err := os.ReadDir(dir)
		if err != nil {
			log.Printf("[HTTP] /api/plugins/%s readdir error: %v", pluginType, err)
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

	// Serve specific plugin
	name := filepath.Base(parts[1]) // Sanitize path traversal
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		log.Printf("[HTTP] /api/plugins/%s/%s not found: %v", pluginType, name, err)
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
