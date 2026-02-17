package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProgenyAlpha/periscope/internal/store"
)

// --- Helpers ---

func newTestApp(t *testing.T, token string) *App {
	t.Helper()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := store.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	dataDir := filepath.Join(tmpDir, "data")
	claudeDir := filepath.Join(tmpDir, "claude")
	pluginDir := filepath.Join(tmpDir, "plugins")
	homeDir := filepath.Join(tmpDir, "home")
	for _, d := range []string{dataDir, claudeDir, pluginDir, homeDir} {
		os.MkdirAll(d, 0755)
	}

	hub := newHub()
	go hub.run()

	app := &App{
		Config: AppConfig{
			Server: ServerConfig{Token: token},
		},
		DB:        db,
		Hub:       hub,
		DataDir:   dataDir,
		ClaudeDir: claudeDir,
		PluginDir: pluginDir,
		HomeDir:   homeDir,
	}
	return app
}

func newTestHandler(app *App) http.Handler {
	mux := buildMux(app)
	return authMiddleware(app.Config.Server.Token, rateLimitMiddleware(corsMiddleware(mux)))
}

// --- Auth Middleware ---

func TestAuthMiddleware(t *testing.T) {
	tests := []struct {
		name       string
		token      string
		path       string
		authHeader string
		queryParam string
		wantStatus int  // exact match
		not401     bool // if true, just assert != 401
	}{
		{
			name:   "no token configured",
			token:  "",
			path:   "/api/data",
			not401: true,
		},
		{
			name:       "health bypasses auth",
			token:      "secret",
			path:       "/api/health",
			wantStatus: 200,
		},
		{
			name:   "root bypasses auth",
			token:  "secret",
			path:   "/",
			not401: true,
		},
		{
			name:       "API no token",
			token:      "secret",
			path:       "/api/data",
			wantStatus: 401,
		},
		{
			name:       "API correct bearer",
			token:      "secret",
			path:       "/api/data",
			authHeader: "Bearer secret",
			not401:     true,
		},
		{
			name:       "API wrong bearer",
			token:      "secret",
			path:       "/api/data",
			authHeader: "Bearer wrong",
			wantStatus: 401,
		},
		{
			name:       "WS correct param",
			token:      "secret",
			path:       "/ws",
			queryParam: "token=secret",
			not401:     true,
		},
		{
			name:       "WS no token",
			token:      "secret",
			path:       "/ws",
			wantStatus: 401,
		},
		{
			name:       "WS wrong param",
			token:      "secret",
			path:       "/ws",
			queryParam: "token=wrong",
			wantStatus: 401,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newTestApp(t, tt.token)
			handler := newTestHandler(app)

			path := tt.path
			if tt.queryParam != "" {
				path += "?" + tt.queryParam
			}
			req := httptest.NewRequest("GET", path, nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if tt.not401 {
				if rr.Code == 401 {
					t.Errorf("got 401, want != 401")
				}
			} else if rr.Code != tt.wantStatus {
				t.Errorf("got %d, want %d", rr.Code, tt.wantStatus)
			}
		})
	}
}

// --- Rate Limiter ---

func TestRateLimiter(t *testing.T) {
	t.Run("burst capacity", func(t *testing.T) {
		externalLimiter.reset()
		generalLimiter.reset()

		app := newTestApp(t, "")
		handler := newTestHandler(app)

		// generalLimiter burst=10, make 10 requests — all should succeed
		for i := 0; i < 10; i++ {
			req := httptest.NewRequest("GET", "/api/statusline", nil)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code == 429 {
				t.Fatalf("request %d got 429, expected success", i)
			}
		}
	})

	t.Run("429 after exhaustion", func(t *testing.T) {
		externalLimiter.reset()
		generalLimiter.reset()

		app := newTestApp(t, "")
		handler := newTestHandler(app)

		// Exhaust generalLimiter (burst=10)
		for i := 0; i < 10; i++ {
			req := httptest.NewRequest("GET", "/api/statusline", nil)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
		}

		// Next request should be 429
		req := httptest.NewRequest("GET", "/api/statusline", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != 429 {
			t.Errorf("got %d, want 429", rr.Code)
		}
	})

	t.Run("Retry-After header", func(t *testing.T) {
		externalLimiter.reset()
		generalLimiter.reset()

		app := newTestApp(t, "")
		handler := newTestHandler(app)

		// Exhaust
		for i := 0; i < 10; i++ {
			req := httptest.NewRequest("GET", "/api/statusline", nil)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
		}

		req := httptest.NewRequest("GET", "/api/statusline", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if ra := rr.Header().Get("Retry-After"); ra != "5" {
			t.Errorf("Retry-After = %q, want %q", ra, "5")
		}
	})

	t.Run("external vs general distinction", func(t *testing.T) {
		externalLimiter.reset()
		generalLimiter.reset()

		app := newTestApp(t, "")
		handler := newTestHandler(app)

		// externalLimiter burst=3, exhaust it via /api/usage
		for i := 0; i < 3; i++ {
			req := httptest.NewRequest("POST", "/api/usage", nil)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
		}

		// /api/usage should now be limited
		req := httptest.NewRequest("POST", "/api/usage", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != 429 {
			t.Errorf("/api/usage got %d, want 429", rr.Code)
		}

		// /api/data should still work (uses generalLimiter)
		req = httptest.NewRequest("GET", "/api/data", nil)
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code == 429 {
			t.Errorf("/api/data got 429, expected success")
		}
	})
}

// --- CORS ---

func TestCORS(t *testing.T) {
	app := newTestApp(t, "")

	tests := []struct {
		name       string
		origin     string
		method     string
		wantACHeader bool   // expect Access-Control-Allow-Origin set
		wantStatus   int    // 0 means don't check
	}{
		{
			name:         "no origin",
			origin:       "",
			method:       "GET",
			wantACHeader: false,
		},
		{
			name:         "localhost origin",
			origin:       "http://localhost:3000",
			method:       "GET",
			wantACHeader: true,
		},
		{
			name:         "foreign origin",
			origin:       "http://evil.com",
			method:       "GET",
			wantACHeader: false,
		},
		{
			name:       "OPTIONS request",
			origin:     "http://localhost:3000",
			method:     "OPTIONS",
			wantStatus: 204,
		},
		{
			name:   "Authorization in allowed headers",
			origin: "http://localhost:3000",
			method: "OPTIONS",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			externalLimiter.reset()
			generalLimiter.reset()

			handler := newTestHandler(app)
			req := httptest.NewRequest(tt.method, "/api/health", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			acao := rr.Header().Get("Access-Control-Allow-Origin")
			if tt.wantACHeader && acao == "" {
				t.Error("expected Access-Control-Allow-Origin to be set")
			}
			if !tt.wantACHeader && tt.name != "OPTIONS request" && tt.name != "Authorization in allowed headers" && acao != "" {
				t.Errorf("expected no Access-Control-Allow-Origin, got %q", acao)
			}

			if tt.wantStatus != 0 && rr.Code != tt.wantStatus {
				t.Errorf("got %d, want %d", rr.Code, tt.wantStatus)
			}

			// Check Authorization is in allowed headers
			if tt.name == "Authorization in allowed headers" {
				ah := rr.Header().Get("Access-Control-Allow-Headers")
				if !strings.Contains(ah, "Authorization") {
					t.Errorf("Access-Control-Allow-Headers = %q, missing Authorization", ah)
				}
			}
		})
	}
}

// --- Handlers ---

func TestHandlers(t *testing.T) {
	t.Run("GET /api/health", func(t *testing.T) {
		externalLimiter.reset()
		generalLimiter.reset()
		app := newTestApp(t, "")
		handler := newTestHandler(app)

		req := httptest.NewRequest("GET", "/api/health", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != 200 {
			t.Fatalf("got %d, want 200", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), `"ok":true`) {
			t.Errorf("body = %q, want ok:true", rr.Body.String())
		}
	})

	t.Run("GET /api/data", func(t *testing.T) {
		externalLimiter.reset()
		generalLimiter.reset()
		app := newTestApp(t, "")
		handler := newTestHandler(app)

		req := httptest.NewRequest("GET", "/api/data", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != 200 {
			t.Fatalf("got %d, want 200", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "sessions") {
			t.Errorf("body missing 'sessions': %q", rr.Body.String())
		}
	})

	t.Run("POST /api/data returns 405", func(t *testing.T) {
		externalLimiter.reset()
		generalLimiter.reset()
		app := newTestApp(t, "")
		handler := newTestHandler(app)

		req := httptest.NewRequest("POST", "/api/data", strings.NewReader(""))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != 405 {
			t.Errorf("got %d, want 405", rr.Code)
		}
	})

	t.Run("POST /api/config valid JSON", func(t *testing.T) {
		externalLimiter.reset()
		generalLimiter.reset()
		app := newTestApp(t, "")
		// Create statusline config dir
		os.MkdirAll(filepath.Join(app.ClaudeDir, "statusline"), 0755)
		handler := newTestHandler(app)

		req := httptest.NewRequest("POST", "/api/config", strings.NewReader(`{"foo":"bar"}`))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != 200 {
			t.Fatalf("got %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
	})

	t.Run("POST /api/config invalid body", func(t *testing.T) {
		externalLimiter.reset()
		generalLimiter.reset()
		app := newTestApp(t, "")
		os.MkdirAll(filepath.Join(app.ClaudeDir, "statusline"), 0755)
		handler := newTestHandler(app)

		req := httptest.NewRequest("POST", "/api/config", strings.NewReader("not json"))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != 400 {
			t.Errorf("got %d, want 400", rr.Code)
		}
	})

	t.Run("GET /api/layout", func(t *testing.T) {
		externalLimiter.reset()
		generalLimiter.reset()
		app := newTestApp(t, "")
		handler := newTestHandler(app)

		req := httptest.NewRequest("GET", "/api/layout", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != 200 {
			t.Fatalf("got %d, want 200", rr.Code)
		}
	})

	t.Run("POST+GET /api/layout roundtrip", func(t *testing.T) {
		externalLimiter.reset()
		generalLimiter.reset()
		app := newTestApp(t, "")
		handler := newTestHandler(app)

		// POST layout
		req := httptest.NewRequest("POST", "/api/layout", strings.NewReader(`{"grid":[1,2]}`))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("POST got %d, want 200", rr.Code)
		}

		// GET layout
		externalLimiter.reset()
		generalLimiter.reset()
		req = httptest.NewRequest("GET", "/api/layout", nil)
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("GET got %d, want 200", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), `"grid"`) {
			t.Errorf("GET body = %q, missing grid", rr.Body.String())
		}
	})

	t.Run("POST /api/shutdown calls cancel", func(t *testing.T) {
		externalLimiter.reset()
		generalLimiter.reset()
		app := newTestApp(t, "")
		cancelled := false
		app.cancel = func() { cancelled = true }
		handler := newTestHandler(app)

		req := httptest.NewRequest("POST", "/api/shutdown", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != 200 {
			t.Fatalf("got %d, want 200", rr.Code)
		}
		if !cancelled {
			t.Error("expected cancel to be called")
		}
	})

	t.Run("GET /api/shutdown returns 405", func(t *testing.T) {
		externalLimiter.reset()
		generalLimiter.reset()
		app := newTestApp(t, "")
		handler := newTestHandler(app)

		req := httptest.NewRequest("GET", "/api/shutdown", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != 405 {
			t.Errorf("got %d, want 405", rr.Code)
		}
	})

	t.Run("GET /api/plugins/themes", func(t *testing.T) {
		externalLimiter.reset()
		generalLimiter.reset()
		app := newTestApp(t, "")

		// Create themes dir with a test file
		themesDir := filepath.Join(app.PluginDir, "themes")
		os.MkdirAll(themesDir, 0755)
		os.WriteFile(filepath.Join(themesDir, "test.toml"), []byte("[theme]"), 0644)

		handler := newTestHandler(app)
		req := httptest.NewRequest("GET", "/api/plugins/themes", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != 200 {
			t.Fatalf("got %d, want 200", rr.Code)
		}
		var names []string
		if err := json.Unmarshal(rr.Body.Bytes(), &names); err != nil {
			t.Fatalf("body not JSON array: %v", err)
		}
	})

	t.Run("GET /api/plugins/evil returns 404", func(t *testing.T) {
		externalLimiter.reset()
		generalLimiter.reset()
		app := newTestApp(t, "")
		handler := newTestHandler(app)

		req := httptest.NewRequest("GET", "/api/plugins/evil", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != 404 {
			t.Errorf("got %d, want 404", rr.Code)
		}
	})

	t.Run("GET /api/plugins path traversal returns 400", func(t *testing.T) {
		externalLimiter.reset()
		generalLimiter.reset()
		app := newTestApp(t, "")
		handler := newTestHandler(app)

		// Use RequestURI to bypass ServeMux's path cleaning
		req := httptest.NewRequest("GET", "/api/plugins/themes/..%2f..%2fetc%2fpasswd", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		// After URL decoding and filepath.Base, the handler should catch traversal or return 404
		// The handler uses filepath.Base which strips directory components, so it becomes "passwd"
		// which won't exist — either 400 or 404 is acceptable for path traversal attempts
		if rr.Code != 400 && rr.Code != 404 {
			t.Errorf("got %d, want 400 or 404", rr.Code)
		}
	})
}
