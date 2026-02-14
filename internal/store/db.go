package store

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
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
		CREATE TABLE IF NOT EXISTS push_subscriptions (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			endpoint   TEXT NOT NULL UNIQUE,
			auth_key   TEXT NOT NULL,
			p256dh_key TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
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
	_, err := db.Exec(`INSERT OR REPLACE INTO kv(key, value, updated_at) VALUES(?, ?, CURRENT_TIMESTAMP)`,
		key, value)
	if err != nil {
		log.Printf("[DB] KVSet(%q) failed: %v", key, err)
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
	return nil
}

// ImportSidecars imports session sidecar JSON files from the data directory.
func ImportSidecars(db *sql.DB, dataDir string) error {
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
		data = StripBOM(data)
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
		log.Printf("[SNAPSHOT] sidecars: %d snapshotted", snapshotted)
	}
}

// --- Phantom Usage ---

type PhantomData struct {
	ExtraUsageTotal   float64 `json:"extraUsageTotal"`
	LocalSessionTotal float64 `json:"localSessionTotal"`
	PhantomCost       float64 `json:"phantomCost"`
	Source            string  `json:"source"`
	Confidence        string  `json:"confidence"`
}

func CalcPhantomUsage(db *sql.DB) *PhantomData {
	// Sum sidecar costs for current 7-day window only
	sevenDaysCutoff := time.Now().Add(-7 * 24 * time.Hour).Format("2006-01-02T15:04:05Z")
	var localTotal float64
	rows, err := db.Query("SELECT data FROM sessions WHERE updated_at >= ?", sevenDaysCutoff)
	if err != nil {
		log.Printf("[PHANTOM] sessions query error: %v", err)
		return &PhantomData{Source: "none", Confidence: "none"}
	}
	defer rows.Close()

	for rows.Next() {
		var raw string
		if rows.Scan(&raw) != nil {
			continue
		}
		var sc struct {
			Cumulative *struct {
				Cost float64 `json:"cost"`
			} `json:"cumulative"`
		}
		if json.Unmarshal([]byte(raw), &sc) != nil || sc.Cumulative == nil {
			continue
		}
		localTotal += sc.Cumulative.Cost
	}

	if localTotal < 0.001 {
		return &PhantomData{Source: "none", Confidence: "none"}
	}

	// Detect phantom usage by finding periods where pctWeekly increased
	// but NO local CLI activity occurred (no sidecar updates).
	// During those intervals, 100% of the rate limit growth is phantom.
	//
	// Then: phantom_cost = local_total * (phantom_pct / cli_pct)
	// because both share the same quota denominator, costs scale proportionally.
	//
	// Scoped to the current 7-day window (matching pctWeekly's reset cycle).

	sevenDaysAgo := time.Now().Add(-7 * 24 * time.Hour).Format(time.RFC3339)

	// Load limit_history snapshots from last 7 days only
	type snapshot struct {
		ts        time.Time
		pctWeekly float64
	}
	var snapshots []snapshot
	lhRows, err := db.Query("SELECT ts, data FROM limit_history WHERE ts >= ? ORDER BY ts ASC", sevenDaysAgo)
	if err != nil {
		return &PhantomData{LocalSessionTotal: math.Round(localTotal*100) / 100, Source: "none", Confidence: "none"}
	}
	defer lhRows.Close()

	for lhRows.Next() {
		var tsStr, dataStr string
		if lhRows.Scan(&tsStr, &dataStr) != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, tsStr)
		if err != nil {
			t, err = time.Parse(time.RFC3339, tsStr)
			if err != nil {
				continue
			}
		}
		var d map[string]any
		if json.Unmarshal([]byte(dataStr), &d) != nil {
			continue
		}
		pctW, ok := d["pctWeekly"].(float64)
		if !ok || pctW < 0 {
			continue
		}
		snapshots = append(snapshots, snapshot{ts: t, pctWeekly: pctW})
	}

	if len(snapshots) < 2 {
		return &PhantomData{LocalSessionTotal: math.Round(localTotal*100) / 100, Source: "none", Confidence: "none"}
	}

	// Build a set of "active minutes" from history snapshots (last 7 days).
	// History entries are created per-session per-poll-cycle only when sidecars
	// have changed — they represent actual CLI activity, not just server uptime.
	activeMinutes := map[string]bool{}
	hRows, err := db.Query("SELECT ts FROM history WHERE ts >= ?", sevenDaysAgo)
	if err == nil {
		defer hRows.Close()
		for hRows.Next() {
			var ts string
			if hRows.Scan(&ts) != nil {
				continue
			}
			if t, err := time.Parse("2006-01-02T15:04:05Z", ts); err == nil {
				activeMinutes[t.Truncate(time.Minute).Format(time.RFC3339)] = true
			} else if t, err := time.Parse(time.RFC3339, ts); err == nil {
				activeMinutes[t.UTC().Truncate(time.Minute).Format(time.RFC3339)] = true
			}
		}
	}

	// Walk consecutive snapshots. If pctWeekly grew and no CLI activity
	// occurred between them, attribute that growth to phantom.
	var phantomPct, cliPct float64
	for i := 1; i < len(snapshots); i++ {
		prev := snapshots[i-1]
		cur := snapshots[i]
		delta := cur.pctWeekly - prev.pctWeekly
		if delta <= 0 {
			continue // reset or no change
		}

		// Check if any CLI activity occurred between these two snapshots
		hasActivity := false
		t := prev.ts.UTC().Truncate(time.Minute)
		end := cur.ts.UTC().Truncate(time.Minute)
		for !t.After(end) {
			if activeMinutes[t.Format(time.RFC3339)] {
				hasActivity = true
				break
			}
			t = t.Add(time.Minute)
		}

		if hasActivity {
			cliPct += delta
		} else {
			phantomPct += delta
		}
	}

	totalPct := phantomPct + cliPct
	if totalPct < 0.5 || phantomPct < 0.5 {
		// Not enough phantom signal
		return &PhantomData{
			LocalSessionTotal: math.Round(localTotal*100) / 100,
			PhantomCost:       0,
			Source:            "rate_delta",
			Confidence:        "estimated",
		}
	}

	// phantom_cost = local_total * (phantom_pct / cli_pct)
	phantomCost := localTotal * (phantomPct / cliPct)

	return &PhantomData{
		ExtraUsageTotal:   math.Round(phantomPct*10) / 10,   // % of weekly limit from phantom
		LocalSessionTotal: math.Round(localTotal*100) / 100,
		PhantomCost:       math.Round(phantomCost*100) / 100,
		Source:            "rate_delta",
		Confidence:        "estimated",
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
	PhantomUsage     *PhantomData      `json:"phantomUsage,omitempty"`
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
	d.PhantomUsage = CalcPhantomUsage(db)

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
				log.Printf("[SNAPSHOT] limit: skipped (time dedup, %ds since last)", int(elapsed.Seconds()))
				return
			}
			if lastData != "" && elapsed < 5*time.Minute {
				var last map[string]any
				if json.Unmarshal([]byte(lastData), &last) == nil {
					same := fmt.Sprintf("%v", current["pct5hr"]) == fmt.Sprintf("%v", last["pct5hr"]) &&
						fmt.Sprintf("%v", current["pctWeekly"]) == fmt.Sprintf("%v", last["pctWeekly"]) &&
						fmt.Sprintf("%v", current["pctSonnet"]) == fmt.Sprintf("%v", last["pctSonnet"])
					if same {
						log.Printf("[SNAPSHOT] limit: skipped (value dedup)")
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
		log.Printf("[SNAPSHOT] limit: insert failed: %v", err)
		return
	}
	log.Printf("[SNAPSHOT] limit: written (5hr=%v%%, weekly=%v%%)", current["pct5hr"], current["pctWeekly"])

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
						db.Exec("UPDATE limit_history SET data = ? WHERE id = ?", string(patched), e.id)
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
		log.Printf("[COMPACT] limit_history: no entries pruned (%d total)", len(all))
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("[COMPACT] limit_history: transaction failed: %v", err)
		return
	}
	for _, id := range deleteIDs {
		tx.Exec("DELETE FROM limit_history WHERE id = ?", id)
	}
	if err := tx.Commit(); err != nil {
		log.Printf("[COMPACT] limit_history: commit failed: %v", err)
		return
	}
	log.Printf("[COMPACT] limit_history: pruned %d of %d entries", len(deleteIDs), len(all))

	// Rewrite JSONL from surviving entries
	surviving, err := db.Query("SELECT ts, data FROM limit_history ORDER BY ts ASC")
	if err != nil {
		log.Printf("[COMPACT] limit_history: surviving query failed: %v", err)
		return
	}
	defer surviving.Close()

	jsonlPath := filepath.Join(dataDir, "limit-history.jsonl")
	f, err := os.Create(jsonlPath)
	if err != nil {
		log.Printf("[COMPACT] limit_history: JSONL rewrite failed: %v", err)
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

// FetchPricing fetches Claude model pricing from LiteLLM's GitHub source, with 24h cache.
func FetchPricing(dataDir string) (json.RawMessage, error) {
	cachePath := filepath.Join(dataDir, "litellm-pricing-cache.json")
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
