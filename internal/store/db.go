package store

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SchemaVersion defines the current DB schema version.
const SchemaVersion = 1

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
	if err != nil {
		log.Printf("[DB] schema migration failed: %v", err)
	} else {
		log.Printf("[DB] schema initialized")
	}
	return err
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
	db.Exec(`INSERT OR REPLACE INTO kv(key, value, updated_at) VALUES(?, ?, CURRENT_TIMESTAMP)`,
		key, value)
}

// --- Import Logic ---

func ImportFileData(db *sql.DB, dataDir, claudeDir string) error {
	if err := importSidecars(db, dataDir); err != nil {
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
	return nil
}

func importSidecars(db *sql.DB, dataDir string) error {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		log.Printf("[IMPORT] sidecars read dir failed: %v", err)
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
			log.Printf("[IMPORT] sidecar %s read failed: %v", e.Name(), err)
			continue
		}
		data = stripBOM(data)
		modTime := info.ModTime().UTC().Format("2006-01-02T15:04:05Z")
		if _, err := db.Exec(`INSERT OR REPLACE INTO sessions(id, data, updated_at) VALUES(?, ?, ?)`,
			sid, string(data), modTime); err != nil {
			log.Printf("[IMPORT] sidecar %s insert failed: %v", sid, err)
		} else {
			imported++
		}
	}
	log.Printf("[IMPORT] sidecars: %d imported", imported)
	return nil
}

func ImportJSONL(db *sql.DB, path, table string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	data = stripBOM(data)

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
	data = stripBOM(data)
	KVSet(db, key, string(data))
}

func stripBOM(data []byte) []byte {
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

func cleanFirstPrompt(raw string) string {
	s := raw
	s = reSlashCmd.ReplaceAllString(s, "")
	s = reAgentMention.ReplaceAllString(s, "")
	s = reHTMLTags.ReplaceAllString(s, "")
	s = reWhitespace.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	if len(s) > 0 {
		r := []rune(s)
		r[0] = []rune(strings.ToUpper(string(r[0])))[0]
		s = string(r)
	}
	if len(s) > 50 {
		cut := 50
		for i := cut; i > 30; i-- {
			if s[i] == ' ' {
				cut = i
				break
			}
		}
		s = s[:cut] + "..."
	}
	return s
}

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
			data = stripBOM(data)
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
						existing["firstPrompt"] = cleanFirstPrompt(he.Display)
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
		log.Printf("[SNAPSHOT] sidecars: %d snapshotted", snapshotted)
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
	Layout           json.RawMessage   `json:"layout"`
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
		rows.Close()
	}
	if d.Sidecars == nil {
		d.Sidecars = []SidecarEntry{}
	}

	// History
	if rows, err := db.Query("SELECT data FROM history ORDER BY ts ASC"); err == nil {
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

	// Limit History
	if rows, err := db.Query("SELECT ts, data FROM limit_history ORDER BY ts ASC"); err == nil {
		for rows.Next() {
			var ts, data string
			if rows.Scan(&ts, &data) == nil {
				d.LimitHistory = append(d.LimitHistory, json.RawMessage(data))
			}
		}
		rows.Close()
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

	return d, nil
}
