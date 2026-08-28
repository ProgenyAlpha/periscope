package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ProgenyAlpha/periscope/internal/store"
)

// Every mutating dashboard endpoint, driven through the real mux against a
// temp ClaudeDir/DataDir and a temp SQLite DB, asserting the *effect* landed
// rather than that a 200 came back.

func resetLimiters() {
	externalLimiter.reset()
	generalLimiter.reset()
}

// do drives the full middleware stack (auth + rate limit + CORS + mux).
func do(t *testing.T, app *App, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	resetLimiters()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rr := httptest.NewRecorder()
	newTestHandler(app).ServeHTTP(rr, req)
	return rr
}

func statuslineConfigPath(app *App) string {
	return filepath.Join(app.ClaudeDir, "statusline", "statusline-config.json")
}

func claudeSettingsPath(app *App) string {
	return filepath.Join(app.ClaudeDir, "settings.json")
}

// oversized returns a syntactically valid JSON document larger than the 1 MiB
// body cap every mutating handler applies.
func oversized() string {
	return `{"pad":"` + strings.Repeat("a", 2<<20) + `"}`
}

// ── POST /api/config ────────────────────────────────────────────────────────

func TestAPIConfig_PersistsToDiskAndDB(t *testing.T) {
	app := newTestApp(t, "")
	os.RemoveAll(filepath.Join(app.ClaudeDir, "statusline")) // fresh install

	rr := do(t, app, "POST", "/api/config", `{"segments":{"tools":{"enabled":false}}}`)
	if rr.Code != 200 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	saved, err := os.ReadFile(statuslineConfigPath(app))
	if err != nil {
		t.Fatalf("config not written to disk: %v", err)
	}
	var onDisk struct {
		Segments map[string]struct {
			Enabled *bool `json:"enabled"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(saved, &onDisk); err != nil {
		t.Fatalf("saved config is not valid JSON: %v (%s)", err, saved)
	}
	if onDisk.Segments["tools"].Enabled == nil || *onDisk.Segments["tools"].Enabled {
		t.Errorf("the toggle the user made did not survive: %s", saved)
	}

	// The dashboard reads its copy out of kv, not off disk.
	if raw := store.KVGet(app.DB, "config:statusline"); raw == nil {
		t.Error("config:statusline was never stored in the DB")
	}
}

func TestAPIConfig_RejectsMalformedBodyWithoutWriting(t *testing.T) {
	app := newTestApp(t, "")

	rr := do(t, app, "POST", "/api/config", "{not json")
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if _, err := os.Stat(statuslineConfigPath(app)); !os.IsNotExist(err) {
		t.Errorf("a malformed body still produced a config file (%v)", err)
	}
}

func TestAPIConfig_RejectsOversizedBodyWithoutClobbering(t *testing.T) {
	app := newTestApp(t, "")
	// Seed a good config so a truncated write would be visible.
	if err := os.MkdirAll(filepath.Dir(statuslineConfigPath(app)), 0755); err != nil {
		t.Fatal(err)
	}
	good := []byte(`{"segments":{"tools":{"enabled":true}}}`)
	if err := os.WriteFile(statuslineConfigPath(app), good, 0644); err != nil {
		t.Fatal(err)
	}

	rr := do(t, app, "POST", "/api/config", oversized())
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400 for a body over the 1MiB cap", rr.Code)
	}
	after, err := os.ReadFile(statuslineConfigPath(app))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(good) {
		t.Errorf("oversized body damaged the saved config: %s", after)
	}
}

func TestAPIConfig_WrongMethod(t *testing.T) {
	app := newTestApp(t, "")
	if rr := do(t, app, "GET", "/api/config", ""); rr.Code != 405 {
		t.Errorf("GET /api/config = %d, want 405", rr.Code)
	}
	if rr := do(t, app, "DELETE", "/api/config", ""); rr.Code != 405 {
		t.Errorf("DELETE /api/config = %d, want 405", rr.Code)
	}
}

// The file is the statusline's source of truth but the dashboard renders from
// the kv copy, so a kv write that fails leaves the two disagreeing. Answering
// 200 to that is how a discarded save stays invisible.
func TestAPIConfig_ReportsDBFailure(t *testing.T) {
	app := newTestApp(t, "")
	if _, err := app.DB.Exec("DROP TABLE kv"); err != nil {
		t.Fatal(err)
	}

	rr := do(t, app, "POST", "/api/config", `{"segments":{}}`)
	if rr.Code == 200 {
		t.Fatalf("config save answered 200 although the DB write failed")
	}
	if rr.Code != 500 {
		t.Errorf("status = %d, want 500", rr.Code)
	}
}

// ── POST /api/layout ────────────────────────────────────────────────────────

func TestAPILayout_PersistsAndReadsBack(t *testing.T) {
	app := newTestApp(t, "")

	if rr := do(t, app, "POST", "/api/layout", `{"grid":[1,2]}`); rr.Code != 200 {
		t.Fatalf("POST = %d, body = %s", rr.Code, rr.Body.String())
	}
	raw := store.KVGet(app.DB, "config:layout")
	if raw == nil {
		t.Fatal("layout was never stored")
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("stored layout is not JSON: %v (%s)", err, raw)
	}
	if _, ok := got["grid"]; !ok {
		t.Errorf("stored layout lost its grid: %s", raw)
	}

	rr := do(t, app, "GET", "/api/layout", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"grid"`) {
		t.Errorf("GET = %d body %q", rr.Code, rr.Body.String())
	}
}

func TestAPILayout_NullClearsTheStoredLayout(t *testing.T) {
	app := newTestApp(t, "")
	if rr := do(t, app, "POST", "/api/layout", `{"grid":[1]}`); rr.Code != 200 {
		t.Fatalf("seed POST = %d", rr.Code)
	}
	if rr := do(t, app, "POST", "/api/layout", "null"); rr.Code != 200 {
		t.Fatalf("clear POST = %d", rr.Code)
	}
	if raw := store.KVGet(app.DB, "config:layout"); raw != nil {
		t.Errorf("layout survived the clear: %s", raw)
	}
}

func TestAPILayout_RejectsMalformedBodyWithoutClobbering(t *testing.T) {
	app := newTestApp(t, "")
	if rr := do(t, app, "POST", "/api/layout", `{"grid":[1]}`); rr.Code != 200 {
		t.Fatalf("seed POST = %d", rr.Code)
	}

	rr := do(t, app, "POST", "/api/layout", "{not json")
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	raw := store.KVGet(app.DB, "config:layout")
	if raw == nil || !strings.Contains(string(raw), "grid") {
		t.Errorf("a malformed body destroyed the saved layout: %s", raw)
	}
}

func TestAPILayout_RejectsOversizedBodyWithoutClobbering(t *testing.T) {
	app := newTestApp(t, "")
	if rr := do(t, app, "POST", "/api/layout", `{"grid":[1]}`); rr.Code != 200 {
		t.Fatalf("seed POST = %d", rr.Code)
	}

	rr := do(t, app, "POST", "/api/layout", oversized())
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400 for a body over the 1MiB cap", rr.Code)
	}
	raw := store.KVGet(app.DB, "config:layout")
	if raw == nil || !strings.Contains(string(raw), "grid") {
		t.Errorf("oversized body destroyed the saved layout: %s", raw)
	}
}

func TestAPILayout_WrongMethod(t *testing.T) {
	app := newTestApp(t, "")
	if rr := do(t, app, "PUT", "/api/layout", `{}`); rr.Code != 405 {
		t.Errorf("PUT /api/layout = %d, want 405", rr.Code)
	}
}

// config:layout lives only in the DB. A failed insert that still answers 200
// loses the user's dashboard arrangement the moment the page reloads.
func TestAPILayout_ReportsDBFailure(t *testing.T) {
	app := newTestApp(t, "")
	if _, err := app.DB.Exec("DROP TABLE kv"); err != nil {
		t.Fatal(err)
	}

	rr := do(t, app, "POST", "/api/layout", `{"grid":[1]}`)
	if rr.Code == 200 {
		t.Fatal("layout save answered 200 although nothing was stored")
	}
	if rr.Code != 500 {
		t.Errorf("status = %d, want 500", rr.Code)
	}
}

// A failed DELETE was logged and then reported as success too.
func TestAPILayout_ReportsDeleteFailure(t *testing.T) {
	app := newTestApp(t, "")
	if _, err := app.DB.Exec("DROP TABLE kv"); err != nil {
		t.Fatal(err)
	}

	rr := do(t, app, "POST", "/api/layout", "null")
	if rr.Code == 200 {
		t.Fatal("layout clear answered 200 although the delete failed")
	}
}

// ── /api/statusline ─────────────────────────────────────────────────────────

func TestAPIStatusline_EnableCreatesMissingSettingsFile(t *testing.T) {
	app := newTestApp(t, "")
	os.Remove(claudeSettingsPath(app)) // fresh install: no settings.json yet

	rr := do(t, app, "POST", "/api/statusline", `{"enabled":true}`)
	if rr.Code != 200 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	raw, err := os.ReadFile(claudeSettingsPath(app))
	if err != nil {
		t.Fatalf("settings.json was not created: %v", err)
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v (%s)", err, raw)
	}
	sl, ok := settings["statusLine"]
	if !ok {
		t.Fatalf("statusLine was not registered: %s", raw)
	}
	if !strings.Contains(string(sl), "statusline") {
		t.Errorf("statusLine does not invoke the statusline command: %s", sl)
	}
}

func TestAPIStatusline_DisablePreservesOtherKeysAndMode(t *testing.T) {
	app := newTestApp(t, "")
	seed := `{
  "theme": "dark",
  "statusLine": {"type": "command", "command": "/bin/periscope statusline"},
  "hooks": {"Stop": []}
}`
	if err := os.WriteFile(claudeSettingsPath(app), []byte(seed), 0600); err != nil {
		t.Fatal(err)
	}

	rr := do(t, app, "POST", "/api/statusline", `{"enabled":false}`)
	if rr.Code != 200 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	raw, err := os.ReadFile(claudeSettingsPath(app))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v (%s)", err, raw)
	}
	if _, ok := settings["statusLine"]; ok {
		t.Errorf("statusLine was not removed: %s", raw)
	}
	for _, k := range []string{"theme", "hooks"} {
		if _, ok := settings[k]; !ok {
			t.Errorf("disabling the statusline dropped %q: %s", k, raw)
		}
	}
	st, err := os.Stat(claudeSettingsPath(app))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0600 {
		t.Errorf("settings.json mode = %04o, want the 0600 it had", perm)
	}
}

func TestAPIStatusline_GETReportsState(t *testing.T) {
	app := newTestApp(t, "")
	if err := os.WriteFile(claudeSettingsPath(app), []byte(`{"theme":"dark"}`), 0644); err != nil {
		t.Fatal(err)
	}
	rr := do(t, app, "GET", "/api/statusline", "")
	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	var got struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Error("reported enabled with no statusLine key")
	}

	if rr := do(t, app, "POST", "/api/statusline", `{"enabled":true}`); rr.Code != 200 {
		t.Fatalf("enable = %d, body = %s", rr.Code, rr.Body.String())
	}
	rr = do(t, app, "GET", "/api/statusline", "")
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled {
		t.Errorf("GET does not see the statusline that was just enabled: %s", rr.Body.String())
	}
}

func TestAPIStatusline_RejectsMalformedBodyWithoutTouchingSettings(t *testing.T) {
	app := newTestApp(t, "")
	seed := `{"theme":"dark"}`
	if err := os.WriteFile(claudeSettingsPath(app), []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	rr := do(t, app, "POST", "/api/statusline", "{not json")
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	raw, err := os.ReadFile(claudeSettingsPath(app))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != seed {
		t.Errorf("a malformed body rewrote settings.json: %s", raw)
	}
}

func TestAPIStatusline_RejectsOversizedBodyWithoutTouchingSettings(t *testing.T) {
	app := newTestApp(t, "")
	seed := `{"theme":"dark"}`
	if err := os.WriteFile(claudeSettingsPath(app), []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	rr := do(t, app, "POST", "/api/statusline", oversized())
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400 for a body over the 1MiB cap", rr.Code)
	}
	raw, err := os.ReadFile(claudeSettingsPath(app))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != seed {
		t.Errorf("oversized body rewrote settings.json: %s", raw)
	}
}

// settings.json holds every hook periscope registered. A corrupt file must not
// be answered by silently replacing it with a fresh one.
func TestAPIStatusline_CorruptSettingsIsRefusedNotOverwritten(t *testing.T) {
	app := newTestApp(t, "")
	broken := `{"theme": "dark"` // truncated
	if err := os.WriteFile(claudeSettingsPath(app), []byte(broken), 0644); err != nil {
		t.Fatal(err)
	}

	rr := do(t, app, "POST", "/api/statusline", `{"enabled":true}`)
	if rr.Code != 500 {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	raw, err := os.ReadFile(claudeSettingsPath(app))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != broken {
		t.Errorf("the unparseable settings.json was overwritten: %s", raw)
	}
}

func TestAPIStatusline_WrongMethod(t *testing.T) {
	app := newTestApp(t, "")
	if rr := do(t, app, "PUT", "/api/statusline", `{"enabled":true}`); rr.Code != 405 {
		t.Errorf("PUT /api/statusline = %d, want 405", rr.Code)
	}
}

// ── /api/push/* ─────────────────────────────────────────────────────────────

func TestAPIPushSubscribe_PersistsSubscription(t *testing.T) {
	app := newTestApp(t, "")
	endpoint := "https://push.example.test/subscription/abcdefghijklmnopqrstuvwxyz012345"

	body := fmt.Sprintf(`{"endpoint":%q,"keys":{"auth":"A","p256dh":"P"}}`, endpoint)
	if rr := do(t, app, "POST", "/api/push/subscribe", body); rr.Code != 200 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	subs, err := store.PushGetAll(app.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || subs[0].Endpoint != endpoint {
		t.Fatalf("subscription not persisted: %+v", subs)
	}
	if subs[0].Auth != "A" || subs[0].P256dh != "P" {
		t.Errorf("keys not persisted: %+v", subs[0])
	}
}

func TestAPIPushSubscribe_RejectsBadInput(t *testing.T) {
	app := newTestApp(t, "")
	for _, tc := range []struct{ name, body string }{
		{"malformed", "{not json"},
		{"no endpoint", `{"keys":{"auth":"A","p256dh":"P"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rr := do(t, app, "POST", "/api/push/subscribe", tc.body); rr.Code != 400 {
				t.Errorf("status = %d, want 400", rr.Code)
			}
			subs, _ := store.PushGetAll(app.DB)
			if len(subs) != 0 {
				t.Errorf("a rejected body still stored %d subscriptions", len(subs))
			}
		})
	}
}

func TestAPIPushSubscribe_WrongMethod(t *testing.T) {
	app := newTestApp(t, "")
	if rr := do(t, app, "GET", "/api/push/subscribe", ""); rr.Code != 405 {
		t.Errorf("GET /api/push/subscribe = %d, want 405", rr.Code)
	}
}

// A dashboard "test notification" click with a subscription whose endpoint is
// shorter than 40 bytes panicked the handler goroutine.
func TestAPIPushTest_ShortEndpointDoesNotPanic(t *testing.T) {
	app := newTestApp(t, "")
	if rr := do(t, app, "POST", "/api/push/subscribe", `{"endpoint":"x","keys":{}}`); rr.Code != 200 {
		t.Fatalf("subscribe = %d, body = %s", rr.Code, rr.Body.String())
	}

	rr := do(t, app, "POST", "/api/push/test", "")
	if rr.Code != 200 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestAPIPushTest_WrongMethod(t *testing.T) {
	app := newTestApp(t, "")
	if rr := do(t, app, "GET", "/api/push/test", ""); rr.Code != 405 {
		t.Errorf("GET /api/push/test = %d, want 405", rr.Code)
	}
}

func TestAPIPushPublicKey_PersistsVAPIDPair(t *testing.T) {
	app := newTestApp(t, "")

	rr := do(t, app, "GET", "/api/push/public-key", "")
	if rr.Code != 200 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		PublicKey string `json:"publicKey"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.PublicKey == "" {
		t.Fatal("no public key returned")
	}
	if store.KVGet(app.DB, "config:vapid-private") == nil {
		t.Error("the generated private key was not persisted; the next call would mint a new pair")
	}

	// Stable across calls, or every browser subscription is invalidated.
	rr2 := do(t, app, "GET", "/api/push/public-key", "")
	var again struct {
		PublicKey string `json:"publicKey"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &again); err != nil {
		t.Fatal(err)
	}
	if again.PublicKey != got.PublicKey {
		t.Errorf("public key changed between calls: %q then %q", got.PublicKey, again.PublicKey)
	}
}

// ── POST /api/usage ─────────────────────────────────────────────────────────

func TestAPIUsage_WrongMethod(t *testing.T) {
	app := newTestApp(t, "")
	if rr := do(t, app, "GET", "/api/usage", ""); rr.Code != 405 {
		t.Errorf("GET /api/usage = %d, want 405", rr.Code)
	}
}

// Inside the 5-minute cooldown the handler must serve the cached payload
// instead of spending an API call.
func TestAPIUsage_CooldownServesCache(t *testing.T) {
	app := newTestApp(t, "")
	store.KVSet(app.DB, "cache:usage-api", `{"pct5hr":42}`)

	manualSyncMu.Lock()
	prev := lastManualSync
	lastManualSync = time.Now()
	manualSyncMu.Unlock()
	t.Cleanup(func() {
		manualSyncMu.Lock()
		lastManualSync = prev
		manualSyncMu.Unlock()
	})

	rr := do(t, app, "POST", "/api/usage", "")
	if rr.Code != 200 {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"pct5hr":42`) {
		t.Errorf("cooldown did not serve the cache: %s", rr.Body.String())
	}
}

// ── /api/config atomicity ───────────────────────────────────────────────────

// statusline.go reads ~/.claude/statusline/statusline-config.json from a
// separate process on every render. A truncate-then-write save hands that
// reader a half-written document, which parses as nothing and silently
// reverts the statusline to compiled defaults for that frame.
func TestAPIConfig_SaveIsNeverObservedPartial(t *testing.T) {
	app := newTestApp(t, "")
	path := statuslineConfigPath(app)

	bodies := []string{
		`{"segments":{"tools":{"enabled":false}}}`,
		`{"segments":{"tools":{"enabled":true},"pad":"` + strings.Repeat("y", 40000) + `"}}`,
	}
	if rr := do(t, app, "POST", "/api/config", bodies[0]); rr.Code != 200 {
		t.Fatalf("seed POST = %d, body = %s", rr.Code, rr.Body.String())
	}

	stop := make(chan struct{})
	done := make(chan string, 1)
	go func() {
		for {
			select {
			case <-stop:
				done <- ""
				return
			default:
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var v map[string]any
			if json.Unmarshal(raw, &v) != nil {
				done <- fmt.Sprintf("read %d bytes that are not valid JSON: %.80q", len(raw), raw)
				return
			}
		}
	}()

	for i := 0; i < 80; i++ {
		if rr := do(t, app, "POST", "/api/config", bodies[i%len(bodies)]); rr.Code != 200 {
			close(stop)
			<-done
			t.Fatalf("POST %d = %d, body = %s", i, rr.Code, rr.Body.String())
		}
	}
	close(stop)
	if msg := <-done; msg != "" {
		t.Fatalf("the statusline could read a partially written config: %s", msg)
	}
}
