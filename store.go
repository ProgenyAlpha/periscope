package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"log"
	"math"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// stripBOM removes UTF-8 BOM if present (common in Windows-generated files)
func stripBOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[3:]
	}
	return data
}

// httpGet is a package-level helper so main.go can use it for status check.
func httpGet(url string) (*http.Response, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	return client.Get(url)
}

type usageWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

// --- Database ---

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS kv (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS sessions (
			id         TEXT PRIMARY KEY,
			data       TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS history (
			id   INTEGER PRIMARY KEY AUTOINCREMENT,
			ts   TEXT NOT NULL,
			data TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS limit_history (
			id   INTEGER PRIMARY KEY AUTOINCREMENT,
			ts   TEXT NOT NULL,
			data TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_history_ts ON history(ts);
		CREATE INDEX IF NOT EXISTS idx_limit_history_ts ON limit_history(ts);
	`)
	return err
}

// --- File Import ---

// Sidecar exclusions — these aren't session files
var sidecarExclude = map[string]bool{
	"usage-config.json":          true,
	"usage-api-cache.json":       true,
	"profile-cache.json":         true,
	"litellm-pricing-cache.json": true,
}

func importFileData(app *App) error {
	if err := importSidecars(app); err != nil {
		return fmt.Errorf("sidecars: %w", err)
	}
	if err := importJSONL(app, "usage-history.jsonl", "history"); err != nil {
		return fmt.Errorf("history: %w", err)
	}
	if err := importJSONL(app, "limit-history.jsonl", "limit_history"); err != nil {
		return fmt.Errorf("limit history: %w", err)
	}
	importKV(app, "usage-config.json", "config:usage")
	importKV(app, "usage-api-cache.json", "cache:usage-api")
	importKV(app, "profile-cache.json", "cache:profile")
	importStatuslineConfig(app)
	importSessionMeta(app)
	return nil
}

func importSidecars(app *App) error {
	entries, err := os.ReadDir(app.DataDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || sidecarExclude[e.Name()] {
			continue
		}
		sid := strings.TrimSuffix(e.Name(), ".json")
		data, err := os.ReadFile(filepath.Join(app.DataDir, e.Name()))
		if err != nil {
			continue
		}
		data = stripBOM(data)
		app.DB.Exec(`INSERT OR REPLACE INTO sessions(id, data, updated_at) VALUES(?, ?, CURRENT_TIMESTAMP)`,
			sid, string(data))
	}
	return nil
}

func importJSONL(app *App, filename, table string) error {
	path := filepath.Join(app.DataDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	data = stripBOM(data)

	// Get current count to skip already-imported lines
	var count int
	app.DB.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&count)

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if count >= len(lines) {
		return nil // Already up to date
	}

	tx, err := app.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(fmt.Sprintf("INSERT INTO %s(ts, data) VALUES(?, ?)", table))
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, line := range lines[count:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Extract ts field for indexing
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		ts, _ := obj["ts"].(string)
		if ts == "" {
			ts = time.Now().UTC().Format(time.RFC3339)
		}
		stmt.Exec(ts, line)
	}
	return tx.Commit()
}

func importKV(app *App, filename, key string) {
	path := filepath.Join(app.DataDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	data = stripBOM(data)
	app.DB.Exec(`INSERT OR REPLACE INTO kv(key, value, updated_at) VALUES(?, ?, CURRENT_TIMESTAMP)`,
		key, string(data))
}

func importStatuslineConfig(app *App) {
	path := filepath.Join(app.ClaudeDir, "statusline", "statusline-config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	data = stripBOM(data)
	app.DB.Exec(`INSERT OR REPLACE INTO kv(key, value, updated_at) VALUES(?, ?, CURRENT_TIMESTAMP)`,
		"config:statusline", string(data))
}

func importSessionMeta(app *App) {
	projectsDir := filepath.Join(app.ClaudeDir, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return
	}

	meta := make(map[string]any)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		indexPath := filepath.Join(projectsDir, e.Name(), "sessions-index.json")
		data, err := os.ReadFile(indexPath)
		if err != nil {
			continue
		}
		data = stripBOM(data)
		var index struct {
			Entries []map[string]any `json:"entries"`
		}
		if err := json.Unmarshal(data, &index); err != nil {
			continue
		}
		for _, entry := range index.Entries {
			if sid, ok := entry["sessionId"].(string); ok {
				meta[sid] = entry
			}
		}
	}

	if len(meta) > 0 {
		data, _ := json.Marshal(meta)
		app.DB.Exec(`INSERT OR REPLACE INTO kv(key, value, updated_at) VALUES(?, ?, CURRENT_TIMESTAMP)`,
			"cache:session-meta", string(data))
	}
}

// --- Build Dashboard Data ---

type DashboardData struct {
	GeneratedAt      string           `json:"generatedAt"`
	UsageConfig      json.RawMessage  `json:"usageConfig"`
	StatuslineConfig json.RawMessage  `json:"statuslineConfig"`
	Sessions         []any            `json:"sessions"`
	History          []json.RawMessage `json:"history"`
	Sidecars         []SidecarEntry   `json:"sidecars"`
	LiveUsage        json.RawMessage  `json:"liveUsage"`
	Profile          json.RawMessage  `json:"profile"`
	SessionMeta      json.RawMessage  `json:"sessionMeta"`
	LimitHistory     []json.RawMessage `json:"limitHistory"`
	Layout           json.RawMessage  `json:"layout"`
}

type SidecarEntry struct {
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data"`
}

func buildDashboardData(app *App) (*DashboardData, error) {
	d := &DashboardData{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Sessions:    []any{},
	}

	// Sidecars from DB
	if rows, err := app.DB.Query("SELECT id, data FROM sessions ORDER BY updated_at DESC"); err == nil {
		for rows.Next() {
			var id, data string
			if rows.Scan(&id, &data) == nil {
				d.Sidecars = append(d.Sidecars, SidecarEntry{
					ID:   id,
					Data: json.RawMessage(data),
				})
			}
		}
		rows.Close()
	}
	if d.Sidecars == nil {
		d.Sidecars = []SidecarEntry{}
	}

	// History from DB
	if rows, err := app.DB.Query("SELECT data FROM history ORDER BY ts ASC"); err == nil {
		for rows.Next() {
			var data string
			if rows.Scan(&data) == nil {
				d.History = append(d.History, json.RawMessage(data))
			}
		}
		rows.Close()
	}
	if d.History == nil {
		d.History = []json.RawMessage{}
	}

	// Limit history from DB — inject ts from SQL column if missing from JSON data
	if rows, err := app.DB.Query("SELECT ts, data FROM limit_history ORDER BY ts ASC"); err == nil {
		for rows.Next() {
			var ts, data string
			if rows.Scan(&ts, &data) == nil {
				// Check if data has ts field; if not, inject it
				if !strings.Contains(data[:min(len(data), 30)], `"ts"`) {
					var m map[string]any
					if json.Unmarshal([]byte(data), &m) == nil {
						if _, ok := m["ts"]; !ok {
							m["ts"] = ts
							if patched, err := json.Marshal(m); err == nil {
								data = string(patched)
							}
						}
					}
				}
				d.LimitHistory = append(d.LimitHistory, json.RawMessage(data))
			}
		}
		rows.Close()
	}
	if d.LimitHistory == nil {
		d.LimitHistory = []json.RawMessage{}
	}

	// KV lookups
	d.UsageConfig = kvGet(app.DB, "config:usage")
	d.StatuslineConfig = kvGet(app.DB, "config:statusline")
	d.LiveUsage = kvGet(app.DB, "cache:usage-api")
	d.Profile = kvGet(app.DB, "cache:profile")
	d.SessionMeta = kvGet(app.DB, "cache:session-meta")
	d.Layout = kvGet(app.DB, "config:layout")

	// Side effects: refresh profile if stale
	go refreshProfileIfStale(app)

	return d, nil
}

func kvGet(db *sql.DB, key string) json.RawMessage {
	var value string
	err := db.QueryRow("SELECT value FROM kv WHERE key = ?", key).Scan(&value)
	if err != nil || value == "" {
		return nil
	}
	return json.RawMessage(value)
}

func kvSet(db *sql.DB, key, value string) {
	db.Exec(`INSERT OR REPLACE INTO kv(key, value, updated_at) VALUES(?, ?, CURRENT_TIMESTAMP)`,
		key, value)
}

// --- Anthropic API ---

func getOAuthToken(app *App) (string, error) {
	credPath := filepath.Join(app.ClaudeDir, ".credentials.json")
	data, err := os.ReadFile(credPath)
	if err != nil {
		return "", fmt.Errorf("credentials not found: %w", err)
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("credentials parse error: %w", err)
	}
	if creds.ClaudeAiOauth.AccessToken == "" {
		return "", fmt.Errorf("no OAuth token found")
	}
	return creds.ClaudeAiOauth.AccessToken, nil
}

func fetchUsage(app *App) (json.RawMessage, error) {
	token, err := getOAuthToken(app)
	if err != nil {
		return nil, err
	}

	req, _ := http.NewRequest("GET", "https://api.anthropic.com/api/oauth/usage", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "periscope-telemetry")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
	}

	// Parse the API response
	var apiResp struct {
		FiveHour          *usageWindow `json:"five_hour"`
		SevenDay          *usageWindow `json:"seven_day"`
		SevenDaySonnet    *usageWindow `json:"seven_day_sonnet"`
		SevenDayOpus      *usageWindow `json:"seven_day_opus"`
		SevenDayOauthApps *usageWindow `json:"seven_day_oauth_apps"`
		SevenDayCowork    *usageWindow `json:"seven_day_cowork"`
		ExtraUsage *struct {
			IsEnabled    bool     `json:"is_enabled"`
			MonthlyLimit *float64 `json:"monthly_limit"`
			UsedCredits  float64  `json:"used_credits"`
			Utilization  *float64 `json:"utilization"`
		} `json:"extra_usage"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, err
	}

	usage := map[string]any{
		"fetched_at": time.Now().Unix(),
	}

	if apiResp.FiveHour != nil {
		usage["pct5hr"] = int(apiResp.FiveHour.Utilization + 0.5)
		usage["reset5hr"] = apiResp.FiveHour.ResetsAt
	} else {
		usage["pct5hr"] = -1
	}
	if apiResp.SevenDay != nil {
		usage["pctWeekly"] = int(apiResp.SevenDay.Utilization + 0.5)
		usage["resetWeekly"] = apiResp.SevenDay.ResetsAt
	} else {
		usage["pctWeekly"] = -1
	}
	if apiResp.SevenDaySonnet != nil {
		usage["pctSonnet"] = int(apiResp.SevenDaySonnet.Utilization + 0.5)
		usage["resetSonnet"] = apiResp.SevenDaySonnet.ResetsAt
	} else {
		usage["pctSonnet"] = -1
	}
	if apiResp.SevenDayOpus != nil {
		usage["pctOpus"] = int(apiResp.SevenDayOpus.Utilization + 0.5)
		usage["resetOpus"] = apiResp.SevenDayOpus.ResetsAt
	}
	if apiResp.SevenDayOauthApps != nil {
		usage["pctOauthApps"] = int(apiResp.SevenDayOauthApps.Utilization + 0.5)
		usage["resetOauthApps"] = apiResp.SevenDayOauthApps.ResetsAt
	}
	if apiResp.SevenDayCowork != nil {
		usage["pctCowork"] = int(apiResp.SevenDayCowork.Utilization + 0.5)
		usage["resetCowork"] = apiResp.SevenDayCowork.ResetsAt
	}
	if apiResp.ExtraUsage != nil {
		eu := map[string]any{
			"is_enabled":   apiResp.ExtraUsage.IsEnabled,
			"used_credits": apiResp.ExtraUsage.UsedCredits / 100, // API returns cents
		}
		if apiResp.ExtraUsage.MonthlyLimit != nil {
			eu["monthly_limit"] = *apiResp.ExtraUsage.MonthlyLimit / 100 // API returns cents
		}
		if apiResp.ExtraUsage.Utilization != nil {
			eu["utilization"] = *apiResp.ExtraUsage.Utilization
		}
		usage["extra_usage"] = eu
	}

	result, _ := json.Marshal(usage)

	// Cache to DB and file (so hooks/statusline can read it)
	kvSet(app.DB, "cache:usage-api", string(result))
	os.WriteFile(filepath.Join(app.DataDir, "usage-api-cache.json"), result, 0644)

	return result, nil
}

func fetchProfile(app *App) (json.RawMessage, error) {
	token, err := getOAuthToken(app)
	if err != nil {
		return nil, err
	}

	req, _ := http.NewRequest("GET", "https://api.anthropic.com/api/oauth/profile", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "periscope-telemetry")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API returned %d", resp.StatusCode)
	}

	var apiResp map[string]any
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, err
	}

	// Transform to our profile format
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
	kvSet(app.DB, "cache:profile", string(result))
	os.WriteFile(filepath.Join(app.DataDir, "profile-cache.json"), result, 0644)

	return result, nil
}

func refreshProfileIfStale(app *App) {
	raw := kvGet(app.DB, "cache:profile")
	if raw != nil {
		var p map[string]any
		if json.Unmarshal(raw, &p) == nil {
			if fetched, ok := p["fetched_at"].(float64); ok {
				if time.Since(time.Unix(int64(fetched), 0)) < 5*time.Minute {
					return // Fresh enough
				}
			}
		}
	}
	fetchProfile(app)
}

// snapshotSidecarsToHistory reads all current sidecar states and inserts a history
// entry for each active session. This runs every 30s so the usage timeline has
// continuous data even when no hooks are firing.
func snapshotSidecarsToHistory(app *App) {
	rows, err := app.DB.Query("SELECT id, data FROM sessions")
	if err != nil {
		return
	}
	defer rows.Close()

	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	for rows.Next() {
		var sid, raw string
		if rows.Scan(&sid, &raw) != nil {
			continue
		}
		var sc struct {
			Cumulative *struct {
				Input      int64   `json:"input"`
				CacheRead  int64   `json:"cache_read"`
				CacheWrite int64   `json:"cache_write"`
				Output     int64   `json:"output"`
				Cost       float64 `json:"cost"`
				AgentCalls int     `json:"agent_calls"`
				ToolCalls  int     `json:"tool_calls"`
				ChatCalls  int     `json:"chat_calls"`
			} `json:"cumulative"`
		}
		if json.Unmarshal([]byte(raw), &sc) != nil || sc.Cumulative == nil {
			continue
		}
		c := sc.Cumulative
		shortSid := sid
		if len(shortSid) > 8 {
			shortSid = shortSid[:8]
		}
		entry := map[string]any{
			"ts":    now,
			"sid":   shortSid,
			"input": c.Input,
			"cr":    c.CacheRead,
			"cw":    c.CacheWrite,
			"out":   c.Output,
			"cost":  math.Round(c.Cost*100) / 100,
			"turns": c.AgentCalls + c.ToolCalls + c.ChatCalls,
		}
		data, _ := json.Marshal(entry)
		app.DB.Exec("INSERT INTO history(ts, data) VALUES(?, ?)", now, string(data))
	}
}

func appendLimitSnapshot(app *App, liveUsage json.RawMessage) {
	if liveUsage == nil {
		return
	}

	var current map[string]any
	if json.Unmarshal(liveUsage, &current) != nil {
		return
	}

	// Check last snapshot: time dedup + value dedup
	var lastTS, lastData string
	app.DB.QueryRow("SELECT ts, data FROM limit_history ORDER BY id DESC LIMIT 1").Scan(&lastTS, &lastData)

	if lastTS != "" {
		if t, err := time.Parse(time.RFC3339, lastTS); err == nil {
			elapsed := time.Since(t)

			// Always enforce minimum 1-minute gap
			if elapsed < 1*time.Minute {
				return
			}

			// Value dedup: skip if pct values haven't changed
			// (reset timestamps have varying sub-second precision so we only compare pct)
			// Allow heartbeat every 15 min even if unchanged (for chart continuity)
			if lastData != "" && elapsed < 15*time.Minute {
				var last map[string]any
				if json.Unmarshal([]byte(lastData), &last) == nil {
					same := fmt.Sprintf("%v", current["pct5hr"]) == fmt.Sprintf("%v", last["pct5hr"]) &&
						fmt.Sprintf("%v", current["pctWeekly"]) == fmt.Sprintf("%v", last["pctWeekly"]) &&
						fmt.Sprintf("%v", current["pctSonnet"]) == fmt.Sprintf("%v", last["pctSonnet"])
					if same {
						return // No change — skip
					}
				}
			}
		}
	}

	now := time.Now().Format(time.RFC3339)
	current["ts"] = now
	dataWithTS, _ := json.Marshal(current)
	app.DB.Exec("INSERT INTO limit_history(ts, data) VALUES(?, ?)", now, string(dataWithTS))

	// Also append to JSONL for backward compat with statusline/hooks
	f, err := os.OpenFile(filepath.Join(app.DataDir, "limit-history.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		f.Write(append(dataWithTS, '\n'))
		f.Close()
	}
}

// --- Limit History Compaction ---
// Tiered dedup: <24h keep all, 24h-7d keep 5min, 7d-30d keep 60min, 30d+ keep 4hr
func compactLimitHistory(app *App) {
	rows, err := app.DB.Query("SELECT id, ts, data FROM limit_history ORDER BY ts ASC")
	if err != nil {
		return
	}
	defer rows.Close()

	type entry struct {
		id   int64
		ts   time.Time
		data string
	}
	var all []entry
	for rows.Next() {
		var e entry
		var tsStr string
		if rows.Scan(&e.id, &tsStr, &e.data) != nil {
			continue
		}
		if t, err := time.Parse(time.RFC3339, tsStr); err == nil {
			e.ts = t
		} else if t, err := time.Parse(time.RFC3339Nano, tsStr); err == nil {
			e.ts = t
		} else {
			continue
		}
		all = append(all, e)
	}

	if len(all) < 100 {
		return // Not worth compacting
	}

	// Fix ts-less entries permanently (from old appendLimitSnapshot bug)
	for _, e := range all {
		if !strings.Contains(e.data[:min(len(e.data), 30)], `"ts"`) {
			var m map[string]any
			if json.Unmarshal([]byte(e.data), &m) == nil {
				if _, ok := m["ts"]; !ok {
					m["ts"] = e.ts.Format(time.RFC3339)
					if patched, err := json.Marshal(m); err == nil {
						app.DB.Exec("UPDATE limit_history SET data = ? WHERE id = ?", string(patched), e.id)
					}
				}
			}
		}
	}

	now := time.Now()
	var deleteIDs []int64
	var lastKept time.Time

	for _, e := range all {
		age := now.Sub(e.ts)
		var minGap time.Duration
		switch {
		case age < 24*time.Hour:
			minGap = 0 // Keep all
		case age < 7*24*time.Hour:
			minGap = 5 * time.Minute
		case age < 30*24*time.Hour:
			minGap = 60 * time.Minute
		default:
			minGap = 4 * time.Hour
		}

		if minGap > 0 && !lastKept.IsZero() && e.ts.Sub(lastKept) < minGap {
			deleteIDs = append(deleteIDs, e.id)
		} else {
			lastKept = e.ts
		}
	}

	if len(deleteIDs) == 0 {
		return
	}

	// Delete pruned entries
	tx, err := app.DB.Begin()
	if err != nil {
		return
	}
	for _, id := range deleteIDs {
		tx.Exec("DELETE FROM limit_history WHERE id = ?", id)
	}
	tx.Commit()

	// Rewrite JSONL from surviving entries (inject ts if missing)
	surviving, err := app.DB.Query("SELECT ts, data FROM limit_history ORDER BY ts ASC")
	if err != nil {
		return
	}
	defer surviving.Close()

	jsonlPath := filepath.Join(app.DataDir, "limit-history.jsonl")
	f, err := os.Create(jsonlPath)
	if err != nil {
		return
	}
	defer f.Close()
	for surviving.Next() {
		var ts, data string
		if surviving.Scan(&ts, &data) == nil {
			// Inject ts if missing from JSON data
			if !strings.Contains(data[:min(len(data), 30)], `"ts"`) {
				var m map[string]any
				if json.Unmarshal([]byte(data), &m) == nil {
					if _, ok := m["ts"]; !ok {
						m["ts"] = ts
						if patched, err := json.Marshal(m); err == nil {
							data = string(patched)
						}
					}
				}
			}
			f.WriteString(data + "\n")
		}
	}

	log.Printf("compaction: pruned %d of %d limit_history entries", len(deleteIDs), len(all))
}

// --- Pricing (LiteLLM) ---

func fetchPricing(app *App) (json.RawMessage, error) {
	// Check cache first (24h TTL)
	cachePath := filepath.Join(app.DataDir, "litellm-pricing-cache.json")
	if data, err := os.ReadFile(cachePath); err == nil {
		var cache struct {
			FetchedAt int64           `json:"fetched_at"`
			Data      json.RawMessage `json:"data"`
		}
		if json.Unmarshal(data, &cache) == nil {
			if time.Since(time.Unix(cache.FetchedAt, 0)) < 24*time.Hour {
				return cache.Data, nil
			}
		}
	}

	// Fetch from LiteLLM
	resp, err := http.Get("https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json")
	if err != nil {
		return readCacheFallback(cachePath)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return readCacheFallback(cachePath)
	}

	var allModels map[string]map[string]any
	if err := json.Unmarshal(body, &allModels); err != nil {
		return readCacheFallback(cachePath)
	}

	// Filter to claude-* models, convert to $/MTok
	result := make(map[string]any)
	for name, info := range allModels {
		if !strings.HasPrefix(name, "claude-") {
			continue
		}
		if strings.Contains(name, "bedrock") || strings.Contains(name, "vertex") {
			continue
		}

		model := map[string]any{}
		if v, ok := info["input_cost_per_token"].(float64); ok {
			model["input"] = v * 1e6
		}
		if v, ok := info["output_cost_per_token"].(float64); ok {
			model["output"] = v * 1e6
		}
		if v, ok := info["cache_read_input_token_cost"].(float64); ok {
			model["cache_read"] = v * 1e6
		}
		if v, ok := info["cache_creation_input_token_cost"].(float64); ok {
			model["cache_write"] = v * 1e6
		}
		if v, ok := info["max_input_tokens"].(float64); ok {
			model["max_input"] = int(v)
		}
		if v, ok := info["max_output_tokens"].(float64); ok {
			model["max_output"] = int(v)
		}
		result[name] = model
	}

	data, _ := json.Marshal(result)

	// Cache it
	cache := map[string]any{"fetched_at": time.Now().Unix(), "data": result}
	cacheData, _ := json.Marshal(cache)
	os.WriteFile(cachePath, cacheData, 0644)

	return data, nil
}

func readCacheFallback(path string) (json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return json.RawMessage("{}"), nil
	}
	var cache struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &cache) == nil && cache.Data != nil {
		return cache.Data, nil
	}
	return json.RawMessage("{}"), nil
}
