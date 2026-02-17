package store

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ProgenyAlpha/periscope/internal/analytics"

	_ "modernc.org/sqlite"
)

// currentSchemaVersion is the latest migration version.
const currentSchemaVersion = 2

// migrations is an ordered list of schema changes. Each entry runs once,
// keyed by its index+1 as the version number.
var migrations = []string{
	// v1: initial schema
	`CREATE TABLE IF NOT EXISTS kv (
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
	CREATE TABLE IF NOT EXISTS push_subscriptions (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		endpoint   TEXT NOT NULL UNIQUE,
		auth_key   TEXT NOT NULL,
		p256dh_key TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_history_ts ON history(ts);
	CREATE INDEX IF NOT EXISTS idx_limit_history_ts ON limit_history(ts);`,
	// v2: index on sessions.updated_at (hot path: phantom calc, dashboard)
	`CREATE INDEX IF NOT EXISTS idx_sessions_updated_at ON sessions(updated_at);`,
	// v3, v4, ... append here
}

// OpenDB opens and migrates the SQLite database.
func OpenDB(path string) (*sql.DB, error) {
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
	// Ensure schema_version table exists (bootstraps itself)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		return fmt.Errorf("create schema_version table: %w", err)
	}

	// Read current version
	var version int
	err := db.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&version)
	if err != nil {
		// No row yet — insert initial
		if _, err := db.Exec("INSERT INTO schema_version(version) VALUES(0)"); err != nil {
			return fmt.Errorf("init schema_version: %w", err)
		}
		version = 0
	}

	// Run pending migrations
	for i := version; i < len(migrations); i++ {
		slog.Info("running migration", "version", i+1)
		if _, err := db.Exec(migrations[i]); err != nil {
			return fmt.Errorf("migration v%d failed: %w", i+1, err)
		}
		if _, err := db.Exec("UPDATE schema_version SET version = ?", i+1); err != nil {
			return fmt.Errorf("update schema_version to v%d: %w", i+1, err)
		}
	}

	slog.Info("schema ready", "version", len(migrations))
	return nil
}

// Sidecar exclusions
var SidecarExclude = map[string]bool{
	"usage-config.json":          true,
	"usage-api-cache.json":       true,
	"profile-cache.json":         true,
	"litellm-pricing-cache.json": true,
}

// --- KV Helpers ---

func KVGet(db *sql.DB, key string) json.RawMessage {
	var value string
	err := db.QueryRow("SELECT value FROM kv WHERE key = ?", key).Scan(&value)
	if err != nil || value == "" {
		return nil
	}
	return json.RawMessage(value)
}

func KVSet(db *sql.DB, key, value string) {
	_, err := db.Exec(`INSERT OR REPLACE INTO kv(key, value, updated_at) VALUES(?, ?, CURRENT_TIMESTAMP)`,
		key, value)
	if err != nil {
		slog.Error("KVSet failed", "key", key, "err", err)
	}
}

// --- Push Subscription Helpers ---

type PushSubscription struct {
	ID       int64  `json:"id"`
	Endpoint string `json:"endpoint"`
	Auth     string `json:"auth"`
	P256dh   string `json:"p256dh"`
}

func PushSubscribe(db *sql.DB, endpoint, auth, p256dh string) error {
	_, err := db.Exec(`INSERT OR REPLACE INTO push_subscriptions(endpoint, auth_key, p256dh_key) VALUES(?, ?, ?)`,
		endpoint, auth, p256dh)
	return err
}

func PushUnsubscribe(db *sql.DB, endpoint string) error {
	_, err := db.Exec(`DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint)
	return err
}

func PushGetAll(db *sql.DB) ([]PushSubscription, error) {
	rows, err := db.Query("SELECT id, endpoint, auth_key, p256dh_key FROM push_subscriptions")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []PushSubscription
	for rows.Next() {
		var s PushSubscription
		if rows.Scan(&s.ID, &s.Endpoint, &s.Auth, &s.P256dh) == nil {
			subs = append(subs, s)
		}
	}
	return subs, nil
}

// --- Team Data ---

type TeamData struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	CreatedAt   int64        `json:"createdAt"`
	LeadSession string       `json:"leadSessionId"`
	Members     []TeamMember `json:"members"`
	TotalCost   float64      `json:"totalCost"`
	TotalTurns  int          `json:"totalTurns"`
	ActiveCount int          `json:"activeCount"`
}

type TeamMember struct {
	AgentID   string  `json:"agentId"`
	Name      string  `json:"name"`
	AgentType string  `json:"agentType"`
	Model     string  `json:"model"`
	JoinedAt  int64   `json:"joinedAt"`
	SessionID string  `json:"sessionId,omitempty"`
	Cost      float64 `json:"cost,omitempty"`
	Turns     int     `json:"turns,omitempty"`
	Status    string  `json:"status,omitempty"`
}

// importTeamConfigs reads ~/.claude/teams/*/config.json, correlates members
// to sidecars, and stores the result as cache:teams.
func importTeamConfigs(db *sql.DB, claudeDir string) {
	teamsDir := filepath.Join(claudeDir, "teams")
	entries, err := os.ReadDir(teamsDir)
	if err != nil {
		// No teams directory — normal for solo users
		return
	}

	var teams []TeamData
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cfgPath := filepath.Join(teamsDir, e.Name(), "config.json")
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			continue
		}
		data = StripBOM(data)
		var td TeamData
		if json.Unmarshal(data, &td) != nil {
			slog.Warn("team config parse failed", "dir", e.Name())
			continue
		}

		// Enrich members from sidecars
		for i := range td.Members {
			m := &td.Members[i]
			// Lead gets direct session match
			if m.AgentID == td.Members[0].AgentID && td.LeadSession != "" {
				m.SessionID = td.LeadSession
			}
			// Try to pull cost/turns from the matched sidecar
			if m.SessionID != "" {
				enrichMemberFromSidecar(db, m)
			}
			if m.Status == "" {
				if m.SessionID != "" && m.Cost > 0 {
					m.Status = "active"
				} else if m.SessionID != "" {
					m.Status = "idle"
				} else {
					m.Status = "unknown"
				}
			}
			td.TotalCost += m.Cost
			td.TotalTurns += m.Turns
			if m.Status == "active" {
				td.ActiveCount++
			}
		}
		teams = append(teams, td)
	}

	if teams == nil {
		teams = []TeamData{}
	}
	out, _ := json.Marshal(teams)
	KVSet(db, "cache:teams", string(out))
	slog.Info("teams imported", "count", len(teams))
}

// enrichMemberFromSidecar reads a sidecar from the sessions table and
// populates cost/turns on the member.
func enrichMemberFromSidecar(db *sql.DB, m *TeamMember) {
	var raw string
	if db.QueryRow("SELECT data FROM sessions WHERE id = ?", m.SessionID).Scan(&raw) != nil {
		return
	}
	var sc struct {
		Cumulative *struct {
			Cost       float64 `json:"cost"`
			AgentCalls int     `json:"agent_calls"`
			ToolCalls  int     `json:"tool_calls"`
			ChatCalls  int     `json:"chat_calls"`
		} `json:"cumulative"`
	}
	if json.Unmarshal([]byte(raw), &sc) != nil || sc.Cumulative == nil {
		return
	}
	m.Cost = math.Round(sc.Cumulative.Cost*100) / 100
	m.Turns = sc.Cumulative.AgentCalls + sc.Cumulative.ToolCalls + sc.Cumulative.ChatCalls
	if m.Turns > 0 {
		m.Status = "active"
	}
}

// --- Import Logic ---

func ImportFileData(db *sql.DB, dataDir, claudeDir string) error {
	if err := ImportSidecars(db, dataDir); err != nil {
		return fmt.Errorf("sidecars: %w", err)
	}
	if err := ImportJSONL(db, filepath.Join(dataDir, "usage-history.jsonl"), "history"); err != nil {
		return fmt.Errorf("history: %w", err)
	}
	if err := ImportJSONL(db, filepath.Join(dataDir, "limit-history.jsonl"), "limit_history"); err != nil {
		return fmt.Errorf("limit history: %w", err)
	}
	importKVFile(db, filepath.Join(dataDir, "usage-config.json"), "config:usage")
	importKVFile(db, filepath.Join(dataDir, "usage-api-cache.json"), "cache:usage-api")
	importKVFile(db, filepath.Join(dataDir, "profile-cache.json"), "cache:profile")

	// Statusline config comes from ~/.claude/statusline/statusline-config.json
	importKVFile(db, filepath.Join(claudeDir, "statusline", "statusline-config.json"), "config:statusline")

	importSessionMeta(db, claudeDir)
	importTeamConfigs(db, claudeDir)
	return nil
}

// ImportSidecars imports session sidecar JSON files from the data directory.
func ImportSidecars(db *sql.DB, dataDir string) error {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		slog.Error("sidecars read dir failed", "err", err)
		return err
	}
	imported := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || SidecarExclude[e.Name()] {
			continue
		}
		sid := strings.TrimSuffix(e.Name(), ".json")
		fpath := filepath.Join(dataDir, e.Name())
		info, err := os.Stat(fpath)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(fpath)
		if err != nil {
			slog.Warn("sidecar read failed", "file", e.Name(), "err", err)
			continue
		}
		data = StripBOM(data)
		modTime := info.ModTime().UTC().Format("2006-01-02T15:04:05Z")
		if _, err := db.Exec(`INSERT OR REPLACE INTO sessions(id, data, updated_at) VALUES(?, ?, ?)`,
			sid, string(data), modTime); err != nil {
			slog.Warn("sidecar insert failed", "sid", sid, "err", err)
		} else {
			imported++
		}
	}
	slog.Info("sidecars imported", "count", imported)
	return nil
}

var validTables = map[string]bool{"history": true, "limit_history": true}

func ImportJSONL(db *sql.DB, path, table string) error {
	if !validTables[table] {
		return fmt.Errorf("invalid table: %s", table)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	data = StripBOM(data)

	var count int
	db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&count)

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if count >= len(lines) {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(fmt.Sprintf("INSERT INTO %s(ts, data) VALUES(?, ?)", table))
	if err != nil {
		return err
	}
	defer stmt.Close()

	imported := 0
	for _, line := range lines[count:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		ts, _ := obj["ts"].(string)
		if ts == "" {
			ts = time.Now().UTC().Format(time.RFC3339)
		}
		if _, err := stmt.Exec(ts, line); err == nil {
			imported++
		}
	}
	return tx.Commit()
}

func importKVFile(db *sql.DB, path, key string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	data = StripBOM(data)
	KVSet(db, key, string(data))
}

// StripBOM removes a UTF-8 BOM if present.
func StripBOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[3:]
	}
	return data
}

// --- Session Meta ---

var (
	reSlashCmd     = regexp.MustCompile(`^/\w+\s*`)
	reAgentMention = regexp.MustCompile(`^@[\w-]+\s*`)
	reHTMLTags     = regexp.MustCompile(`<[^>]*>`)
	reWhitespace   = regexp.MustCompile(`[\s]+`)
)

func importSessionMeta(db *sql.DB, claudeDir string) {
	meta := make(map[string]map[string]any)

	// Source 1: sessions-index.json (legacy)
	projectsDir := filepath.Join(claudeDir, "projects")
	if entries, err := os.ReadDir(projectsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			indexPath := filepath.Join(projectsDir, e.Name(), "sessions-index.json")
			data, err := os.ReadFile(indexPath)
			if err != nil {
				continue
			}
			data = StripBOM(data)
			var index struct {
				Entries []map[string]any `json:"entries"`
			}
			if json.Unmarshal(data, &index) != nil {
				continue
			}
			for _, entry := range index.Entries {
				if sid, ok := entry["sessionId"].(string); ok {
					meta[sid] = entry
				}
			}
		}
	}

	// Source 2: history.jsonl
	histPath := filepath.Join(claudeDir, "history.jsonl")
	if f, err := os.Open(histPath); err == nil {
		defer f.Close()
		type HistEntry struct {
			SessionID string  `json:"sessionId"`
			Display   string  `json:"display"`
			Timestamp float64 `json:"timestamp"`
			Project   string  `json:"project"`
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 256*1024), 2*1024*1024)
		for scanner.Scan() {
			var he HistEntry
			if json.Unmarshal(scanner.Bytes(), &he) != nil || he.SessionID == "" {
				continue
			}
			existing, ok := meta[he.SessionID]
			if !ok {
				existing = map[string]any{
					"sessionId": he.SessionID,
				}
				meta[he.SessionID] = existing
			}
			if _, hasSummary := existing["summary"]; !hasSummary {
				if _, hasFirstPrompt := existing["firstPrompt"]; !hasFirstPrompt {
					if he.Display != "" {
						existing["firstPrompt"] = CleanFirstPrompt(he.Display)
					}
				}
			}
			if he.Timestamp > 0 {
				ts := time.UnixMilli(int64(he.Timestamp)).UTC().Format(time.RFC3339)
				existing["modified"] = ts
				if _, hasCreated := existing["created"]; !hasCreated {
					existing["created"] = ts
				}
			}
			if he.Project != "" {
				existing["project"] = he.Project
			}
		}
	}

	// Source 3: JSONL summaries
	jsonlSummaries := scanSessionJSONLSummaries(claudeDir)
	for sid, summary := range jsonlSummaries {
		existing, ok := meta[sid]
		if !ok {
			existing = map[string]any{"sessionId": sid}
			meta[sid] = existing
		}
		existing["summary"] = summary
	}

	if len(meta) > 0 {
		out := make(map[string]any, len(meta))
		for k, v := range meta {
			out[k] = v
		}
		data, _ := json.Marshal(out)
		KVSet(db, "cache:session-meta", string(data))
	}
}

func scanSessionJSONLSummaries(claudeDir string) map[string]string {
	result := make(map[string]string)
	projectsDir := filepath.Join(claudeDir, "projects")
	projEntries, err := os.ReadDir(projectsDir)
	if err != nil {
		return result
	}
	for _, proj := range projEntries {
		if !proj.IsDir() {
			continue
		}
		projPath := filepath.Join(projectsDir, proj.Name())
		files, err := os.ReadDir(projPath)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			sid := strings.TrimSuffix(f.Name(), ".jsonl")
			if len(sid) != 36 {
				continue
			}
			fpath := filepath.Join(projPath, f.Name())
			if summary := scanFileForSummary(fpath); summary != "" {
				result[sid] = summary
			}
		}
	}
	return result
}

func scanFileForSummary(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	needle := []byte(`"type":"summary"`)
	var lastSummary string

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.Contains(line, needle) {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Summary string `json:"summary"`
		}
		if json.Unmarshal(line, &entry) == nil && entry.Type == "summary" && entry.Summary != "" {
			lastSummary = entry.Summary
		}
	}
	return lastSummary
}

// --- Snapshot Helpers ---

func SnapshotSidecarsToHistory(db *sql.DB, lastSessionSnapshot map[string]float64) {
	rows, err := db.Query("SELECT id, data FROM sessions")
	if err != nil {
		return
	}
	defer rows.Close()

	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	snapshotted := 0

	for rows.Next() {
		var sid, raw string
		if rows.Scan(&sid, &raw) != nil {
			continue
		}
		var sc struct {
			Cumulative *struct {
				Cost       float64 `json:"cost"`
				Input      int64   `json:"input"`
				CacheRead  int64   `json:"cache_read"`
				CacheWrite int64   `json:"cache_write"`
				Output     int64   `json:"output"`
				AgentCalls int     `json:"agent_calls"`
				ToolCalls  int     `json:"tool_calls"`
				ChatCalls  int     `json:"chat_calls"`
			} `json:"cumulative"`
		}
		if json.Unmarshal([]byte(raw), &sc) != nil || sc.Cumulative == nil {
			continue
		}
		c := sc.Cumulative
		cost := math.Round(c.Cost*100) / 100

		if prev, ok := lastSessionSnapshot[sid]; ok && prev == cost {
			continue
		}
		lastSessionSnapshot[sid] = cost

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
			"cost":  cost,
			"turns": c.AgentCalls + c.ToolCalls + c.ChatCalls,
		}
		data, _ := json.Marshal(entry)
		if _, err := db.Exec("INSERT INTO history(ts, data) VALUES(?, ?)", now, string(data)); err == nil {
			snapshotted++
		}
	}
	if snapshotted > 0 {
		slog.Info("sidecars snapshotted", "count", snapshotted)
	}
}

// --- Dashboard Data ---

type DashboardData struct {
	GeneratedAt      string            `json:"generatedAt"`
	UsageConfig      json.RawMessage   `json:"usageConfig"`
	StatuslineConfig json.RawMessage   `json:"statuslineConfig"`
	Sessions         []any             `json:"sessions"`
	History          []json.RawMessage `json:"history"`
	Sidecars         []SidecarEntry    `json:"sidecars"`
	LiveUsage        json.RawMessage   `json:"liveUsage"`
	Profile          json.RawMessage   `json:"profile"`
	SessionMeta      json.RawMessage   `json:"sessionMeta"`
	LimitHistory     []json.RawMessage `json:"limitHistory"`
	Layout           json.RawMessage        `json:"layout"`
	PhantomUsage     *analytics.PhantomData `json:"phantomUsage,omitempty"`
	Teams            json.RawMessage        `json:"teams,omitempty"`
}

type SidecarEntry struct {
	ID        string          `json:"id"`
	Data      json.RawMessage `json:"data"`
	UpdatedAt string          `json:"updated_at,omitempty"`
}

func BuildDashboardData(db *sql.DB) (*DashboardData, error) {
	d := &DashboardData{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Sessions:    []any{},
	}

	// Sidecars
	if rows, err := db.Query("SELECT id, data, updated_at FROM sessions ORDER BY updated_at DESC"); err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, data, updatedAt string
			if rows.Scan(&id, &data, &updatedAt) == nil {
				d.Sidecars = append(d.Sidecars, SidecarEntry{
					ID:        id,
					Data:      json.RawMessage(data),
					UpdatedAt: updatedAt,
				})
			}
		}
	}
	if d.Sidecars == nil {
		d.Sidecars = []SidecarEntry{}
	}

	// History
	if rows, err := db.Query("SELECT data FROM history ORDER BY ts ASC"); err == nil {
		defer rows.Close()
		for rows.Next() {
			var data string
			if rows.Scan(&data) == nil {
				d.History = append(d.History, json.RawMessage(data))
			}
		}
	}
	if d.History == nil {
		d.History = []json.RawMessage{}
	}

	// Limit History
	if rows, err := db.Query("SELECT ts, data FROM limit_history ORDER BY ts ASC"); err == nil {
		defer rows.Close()
		for rows.Next() {
			var ts, data string
			if rows.Scan(&ts, &data) == nil {
				d.LimitHistory = append(d.LimitHistory, json.RawMessage(data))
			}
		}
	}
	if d.LimitHistory == nil {
		d.LimitHistory = []json.RawMessage{}
	}

	// KV
	d.UsageConfig = KVGet(db, "config:usage")
	d.StatuslineConfig = KVGet(db, "config:statusline")
	d.LiveUsage = KVGet(db, "cache:usage-api")
	d.Profile = KVGet(db, "cache:profile")
	d.SessionMeta = KVGet(db, "cache:session-meta")
	d.Layout = KVGet(db, "config:layout")
	d.PhantomUsage = analytics.CalcPhantomUsage(db)
	d.Teams = KVGet(db, "cache:teams")

	return d, nil
}

// --- Limit History ---

// AppendLimitSnapshot inserts a rate-limit data point with time and value dedup.
func AppendLimitSnapshot(db *sql.DB, dataDir string, liveUsage json.RawMessage) {
	if liveUsage == nil {
		return
	}

	var current map[string]any
	if json.Unmarshal(liveUsage, &current) != nil {
		return
	}

	var lastTS, lastData string
	db.QueryRow("SELECT ts, data FROM limit_history ORDER BY id DESC LIMIT 1").Scan(&lastTS, &lastData)

	if lastTS != "" {
		if t, err := time.Parse(time.RFC3339, lastTS); err == nil {
			elapsed := time.Since(t)
			if elapsed < 1*time.Minute {
				slog.Debug("limit snapshot skipped", "reason", "time_dedup", "age_s", int(elapsed.Seconds()))
				return
			}
			if lastData != "" && elapsed < 5*time.Minute {
				var last map[string]any
				if json.Unmarshal([]byte(lastData), &last) == nil {
					same := fmt.Sprintf("%v", current["pct5hr"]) == fmt.Sprintf("%v", last["pct5hr"]) &&
						fmt.Sprintf("%v", current["pctWeekly"]) == fmt.Sprintf("%v", last["pctWeekly"]) &&
						fmt.Sprintf("%v", current["pctSonnet"]) == fmt.Sprintf("%v", last["pctSonnet"])
					if same {
						slog.Debug("limit snapshot skipped", "reason", "value_dedup")
						return
					}
				}
			}
		}
	}

	now := time.Now().Format(time.RFC3339)
	current["ts"] = now
	dataWithTS, _ := json.Marshal(current)
	if _, err := db.Exec("INSERT INTO limit_history(ts, data) VALUES(?, ?)", now, string(dataWithTS)); err != nil {
		slog.Error("limit snapshot insert failed", "err", err)
		return
	}
	slog.Info("limit snapshot written", "pct5hr", current["pct5hr"], "pctWeekly", current["pctWeekly"])

	f, err := os.OpenFile(filepath.Join(dataDir, "limit-history.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		f.Write(append(dataWithTS, '\n'))
		f.Close()
	}
}

// CompactLimitHistory applies tiered dedup: <24h keep all, 24h-7d keep 5min, 7d-30d keep 60min, 30d+ keep 4hr.
func CompactLimitHistory(db *sql.DB, dataDir string) {
	rows, err := db.Query("SELECT id, ts, data FROM limit_history ORDER BY ts ASC")
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
		return
	}

	// Fix ts-less entries permanently
	for _, e := range all {
		if !strings.Contains(e.data[:min(len(e.data), 30)], `"ts"`) {
			var m map[string]any
			if json.Unmarshal([]byte(e.data), &m) == nil {
				if _, ok := m["ts"]; !ok {
					m["ts"] = e.ts.Format(time.RFC3339)
					if patched, err := json.Marshal(m); err == nil {
						if _, err := db.Exec("UPDATE limit_history SET data = ? WHERE id = ?", string(patched), e.id); err != nil {
							slog.Warn("compact: ts patch failed", "id", e.id, "err", err)
						}
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
			minGap = 0
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
		slog.Debug("compact: no entries pruned", "total", len(all))
		return
	}

	tx, err := db.Begin()
	if err != nil {
		slog.Error("compact: transaction failed", "err", err)
		return
	}
	for _, id := range deleteIDs {
		if _, err := tx.Exec("DELETE FROM limit_history WHERE id = ?", id); err != nil {
			slog.Warn("compact: delete failed", "id", id, "err", err)
		}
	}
	if err := tx.Commit(); err != nil {
		slog.Error("compact: commit failed", "err", err)
		return
	}
	slog.Info("compact: pruned entries", "pruned", len(deleteIDs), "total", len(all))

	// Rewrite JSONL from surviving entries
	surviving, err := db.Query("SELECT ts, data FROM limit_history ORDER BY ts ASC")
	if err != nil {
		slog.Error("compact: surviving query failed", "err", err)
		return
	}
	defer surviving.Close()

	jsonlPath := filepath.Join(dataDir, "limit-history.jsonl")
	f, err := os.Create(jsonlPath)
	if err != nil {
		slog.Error("compact: JSONL rewrite failed", "err", err)
		return
	}
	defer f.Close()
	for surviving.Next() {
		var ts, data string
		if surviving.Scan(&ts, &data) == nil {
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
}

