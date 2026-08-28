package store

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ProgenyAlpha/periscope/internal/analytics"
	"github.com/ProgenyAlpha/periscope/internal/forecast"
	"github.com/ProgenyAlpha/periscope/internal/logutil"

	_ "modernc.org/sqlite"
)

// currentSchemaVersion is the latest migration version.
const currentSchemaVersion = 4

// stampLayout is the one timestamp format periscope stores.
//
// SQLite renders CURRENT_TIMESTAMP with a space ("2026-01-01 00:00:00") while
// every insert in this file writes RFC3339 with a T. ' ' sorts before 'T', so a
// row that fell back to a column default would sort and filter wrongly against
// the rest. Every write goes through this layout, and the reads that order or
// filter on a timestamp normalise the separator so legacy rows still compare
// correctly.
const stampLayout = "2006-01-02T15:04:05Z"

func nowStamp() string { return time.Now().UTC().Format(stampLayout) }

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
	// v3: idempotent JSONL re-imports — purge existing duplicates, then add
	// unique indexes so INSERT OR IGNORE makes subsequent imports safe.
	// history: natural key is (ts, data) — SnapshotSidecarsToHistory can emit
	// multiple rows at the same second (one per session), so ts alone is not
	// sufficient; pairing with data covers both JSONL imports and snapshots.
	// limit_history: natural key is ts — AppendLimitSnapshot guarantees
	// unique timestamps per snapshot.
	`DELETE FROM history WHERE id NOT IN (SELECT MIN(id) FROM history GROUP BY ts, data);
CREATE UNIQUE INDEX IF NOT EXISTS idx_history_ts_data ON history(ts, data);
DELETE FROM limit_history WHERE id NOT IN (SELECT MIN(id) FROM limit_history GROUP BY ts);
CREATE UNIQUE INDEX IF NOT EXISTS idx_limit_history_ts_unique ON limit_history(ts);`,
	// v4: sidecar import guard.  Records the (mtime, size) the sessions row for
	// each sidecar was built from, so an unchanged file is not re-read and
	// re-written into the WAL on every poll cycle and every /api/data request.
	// Purely additive — no existing row is touched, and an empty guard table
	// simply means the first pass after upgrade re-imports everything once.
	`CREATE TABLE IF NOT EXISTS sidecar_import (
		id       TEXT PRIMARY KEY,
		mtime_ns INTEGER NOT NULL,
		size     INTEGER NOT NULL
	);`,
	// v5, v6, ... append here
}

const (
	// walSizeLimit caps the on-disk WAL. SQLite's auto-checkpoint restarts the
	// WAL but never shrinks the file, so without journal_size_limit the largest
	// WAL the process ever needed becomes a permanent floor (6.6 MB here, for a
	// 13 MB database). With the limit set, each checkpoint truncates back down.
	walSizeLimit = 4 << 20 // 4 MiB

	// walCheckpointThreshold is the size at which the maintenance pass forces a
	// TRUNCATE checkpoint. It sits well above the 1000-page (~4 MB)
	// auto-checkpoint threshold so the forced pass only runs when the automatic
	// one is not keeping up — checkpointing more eagerly than that would fight
	// normal operation for the writer lock.
	walCheckpointThreshold = 8 << 20 // 8 MiB
)

// OpenDB opens and migrates the SQLite database.
func OpenDB(path string) (*sql.DB, error) {
	// Create the file ourselves so it is 0600 from the first byte.  sql.Open is
	// lazy and never touches the filesystem, so chmod-ing straight after it
	// failed with ENOENT on every first run and SQLite went on to create the
	// file at 0644 — a database that holds the VAPID private key.
	if f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600); err == nil {
		f.Close()
	} else {
		slog.Warn("db pre-create failed", "path", path, "err", err)
	}

	dsn := path + "?" + strings.Join([]string{
		"_pragma=journal_mode(wal)",
		"_pragma=foreign_keys(1)",
		"_pragma=busy_timeout(5000)",
		// NORMAL is the documented pairing for WAL: durability is still
		// crash-safe, only a power loss can cost the most recent commits, and
		// it removes an fsync from every single write.
		"_pragma=synchronous(normal)",
		fmt.Sprintf("_pragma=journal_size_limit(%d)", walSizeLimit),
	}, "&")

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	// SQLite allows exactly one writer.  Three goroutines write through this
	// handle (the HTTP handler, the fsnotify flush and the poll ticker); with an
	// unbounded pool they collided, and a SQLITE_BUSY past the 5s busy_timeout
	// surfaced as a warning while the write was silently dropped.  One
	// connection makes the collision structurally impossible instead of merely
	// unlikely, at the cost of serialising reads — which are all small,
	// in-process scans of a 13 MB database.
	//
	// Every read path in this package must therefore finish (or close) its
	// cursor before issuing another statement; holding one open while writing
	// would now deadlock rather than quietly open a second read snapshot.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	// Never recycle: a fresh connection would come up without the pragmas above.
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}

	// Now that SQLite has materialised the database and its sidecars, tighten
	// the permissions — this also repairs a database created by an older build.
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := os.Chmod(p, 0600); err != nil {
			slog.Warn("db chmod failed", "path", p, "err", err)
		}
	}

	return db, nil
}

// CheckpointWAL forces a full checkpoint and truncates the WAL back to zero.
//
// Auto-checkpoints restart the WAL in place, so the file keeps whatever size it
// once needed. TRUNCATE is the only mode that gives the space back.
func CheckpointWAL(db *sql.DB) error {
	var busy, logFrames, checkpointed int
	err := db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed)
	if err != nil {
		return fmt.Errorf("wal_checkpoint(TRUNCATE): %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("wal_checkpoint(TRUNCATE): blocked by a concurrent reader")
	}
	return nil
}

// CheckpointWALIfLarge truncates the WAL only when it has grown past
// walCheckpointThreshold. Reports whether a checkpoint was run.
func CheckpointWALIfLarge(db *sql.DB, dbPath string) (bool, error) {
	fi, err := os.Stat(dbPath + "-wal")
	if err != nil {
		return false, nil // no WAL yet, nothing to do
	}
	if fi.Size() < walCheckpointThreshold {
		return false, nil
	}
	if err := CheckpointWAL(db); err != nil {
		return false, err
	}
	slog.Info("wal checkpointed", "wasBytes", fi.Size())
	return true, nil
}

// HistoryRetention is how far back the history table is kept.
//
// history grew forever: SnapshotSidecarsToHistory appends a row per changed
// session per poll cycle and nothing ever deleted from it, which is what pushed
// the dashboard payload to a 5.5 MB maximum.
//
// A year is deliberately conservative rather than tight. The dashboard widgets
// offer an "all" range that plots the whole table, so a short window would
// silently amputate charts a user can actually see; a year bounds the growth
// without touching any history a current install holds. Reducing the payload
// further belongs in the API (serving a windowed slice), not in a delete.
const HistoryRetention = 365 * 24 * time.Hour

// CompactHistory deletes history rows older than retention and reports how many
// were removed.
//
// The separator is normalised on read so a legacy row written by the
// CURRENT_TIMESTAMP default ("2026-01-01 00:00:00") is compared correctly
// against the RFC3339 stamps everything else writes.
func CompactHistory(db *sql.DB, retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, fmt.Errorf("compact history: retention must be positive")
	}
	cutoff := time.Now().UTC().Add(-retention).Format("2006-01-02T15:04:05")
	res, err := db.Exec(
		"DELETE FROM history WHERE replace(substr(ts, 1, 19), ' ', 'T') < ?", cutoff)
	if err != nil {
		return 0, fmt.Errorf("compact history: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("compact history: rows affected: %w", err)
	}
	if n > 0 {
		slog.Info("history compacted", "deleted", n, "retentionDays", int(retention.Hours()/24))
	}
	return n, nil
}

// MaintenanceInterval is how often StartMaintenance wakes up.
const MaintenanceInterval = 15 * time.Minute

// StartMaintenance runs periodic database housekeeping until ctx is cancelled:
// a size-gated WAL truncate every pass, and history retention plus limit
// compaction once a day. It returns immediately; the work runs in a goroutine.
func StartMaintenance(ctx context.Context, db *sql.DB, dbPath, dataDir string) {
	go func() {
		ticker := time.NewTicker(MaintenanceInterval)
		defer ticker.Stop()
		const passesPerDay = int(24 * time.Hour / MaintenanceInterval)
		pass := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			pass++
			if _, err := CheckpointWALIfLarge(db, dbPath); err != nil {
				slog.Warn("maintenance: wal checkpoint failed", "err", err)
			}
			if pass%passesPerDay != 0 {
				continue
			}
			if _, err := CompactHistory(db, HistoryRetention); err != nil {
				slog.Error("maintenance: history compaction failed", "err", err)
			}
			if err := CompactLimitHistory(db, dataDir); err != nil {
				slog.Error("maintenance: limit history compaction failed", "err", err)
			}
			if err := CheckpointWAL(db); err != nil {
				slog.Warn("maintenance: post-compaction checkpoint failed", "err", err)
			}
		}
	}()
}

func migrate(db *sql.DB) error {
	// Ensure schema_version table exists (bootstraps itself)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		return fmt.Errorf("create schema_version table: %w", err)
	}

	// Read current version.  schema_version has no primary key, so treating a
	// scan failure as "no rows" inserted a second version-0 row and re-ran every
	// migration from scratch — only a genuine ErrNoRows may bootstrap.
	var version int
	err := db.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := db.Exec("INSERT INTO schema_version(version) VALUES(0)"); err != nil {
			return fmt.Errorf("init schema_version: %w", err)
		}
		version = 0
	case err != nil:
		return fmt.Errorf("read schema_version: %w", err)
	}

	// Run pending migrations — each DDL + version bump in one transaction
	for i := version; i < len(migrations); i++ {
		slog.Info("running migration", "version", i+1)
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("migration v%d begin: %w", i+1, err)
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration v%d failed: %w", i+1, err)
		}
		if _, err := tx.Exec("UPDATE schema_version SET version = ?", i+1); err != nil {
			tx.Rollback()
			return fmt.Errorf("update schema_version to v%d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration v%d commit: %w", i+1, err)
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
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			slog.Warn("KVGet failed", "key", key, "err", err)
		}
		return nil
	}
	// A truncated cache file stored verbatim would break the encode of the
	// whole dashboard payload; nil renders as JSON null instead.
	if value == "" || !json.Valid([]byte(value)) {
		if value != "" {
			slog.Warn("kv value is not valid JSON, omitting from payload", "key", key, "bytes", len(value))
		}
		return nil
	}
	return json.RawMessage(value)
}

// KVSet stores value under key, skipping the write when nothing changed.
//
// The cache keys are rewritten on every poll cycle and every /api/data request
// with byte-identical payloads; each rewrite dirtied a page into the WAL for no
// reason.  CURRENT_TIMESTAMP is replaced by an explicit RFC3339 stamp so the
// column never mixes separators (see stampLayout).
func KVSet(db *sql.DB, key, value string) {
	if err := KVSetErr(db, key, value); err != nil {
		slog.Error("KVSet failed", "key", key, "err", err)
	}
}

// KVSetErr is KVSet for callers that can act on a failure. A handler that
// persists only into kv (the dashboard layout is stored nowhere else) must not
// answer 200 when the row never landed.
func KVSetErr(db *sql.DB, key, value string) error {
	var existing string
	switch err := db.QueryRow("SELECT value FROM kv WHERE key = ?", key).Scan(&existing); {
	case err == nil && existing == value:
		return nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		slog.Warn("KVSet: could not read existing value", "key", key, "err", err)
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO kv(key, value, updated_at) VALUES(?, ?, ?)`,
		key, value, nowStamp()); err != nil {
		return err
	}
	return nil
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
		if err := rows.Scan(&s.ID, &s.Endpoint, &s.Auth, &s.P256dh); err != nil {
			slog.Error("PushGetAll: scan failed", "err", err)
			continue
		}
		subs = append(subs, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
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

// ImportFileData refreshes every on-disk source into the database.
//
// Each source is attempted regardless of what the ones before it did, and every
// failure is reported in the returned error. Bailing out on the first failure
// meant a spell of truncated sidecars also stopped the usage history, the
// rate-limit history and the session metadata from being imported — one broken
// writer taking down four unrelated ones.
func ImportFileData(db *sql.DB, dataDir, claudeDir string) error {
	var errs []error

	if _, err := ImportSidecars(db, dataDir); err != nil {
		errs = append(errs, fmt.Errorf("sidecars: %w", err))
	}
	if err := ImportJSONL(db, filepath.Join(dataDir, "usage-history.jsonl"), "history"); err != nil {
		errs = append(errs, fmt.Errorf("history: %w", err))
	}
	if err := ImportJSONL(db, filepath.Join(dataDir, "limit-history.jsonl"), "limit_history"); err != nil {
		errs = append(errs, fmt.Errorf("limit history: %w", err))
	}
	importKVFile(db, filepath.Join(dataDir, "usage-config.json"), "config:usage")
	importKVFile(db, filepath.Join(dataDir, "usage-api-cache.json"), "cache:usage-api")
	importKVFile(db, filepath.Join(dataDir, "profile-cache.json"), "cache:profile")

	// Statusline config comes from ~/.claude/statusline/statusline-config.json
	importKVFile(db, filepath.Join(claudeDir, "statusline", "statusline-config.json"), "config:statusline")

	importSessionMeta(db, claudeDir)
	importTeamConfigs(db, claudeDir)
	return errors.Join(errs...)
}

// SidecarStats summarises one ImportSidecars pass. The counts are what tell an
// empty data directory apart from one where every single file failed.
type SidecarStats struct {
	Total     int // candidate sidecar files seen
	Imported  int // rows written
	Unchanged int // skipped: file has not changed since the last import
	Invalid   int // skipped: content was not valid JSON
	Failed    int // could not be read or written
}

// sessionIDRe is the allowlist for sidecar filenames.
//
// The old code was a denylist of four hardcoded names, so any other *.json that
// happened to sit in the data directory became a sessions row keyed on its
// filename and had its cumulative.cost summed into the phantom-usage baseline.
// A sidecar is named after a session, and a session id is a UUID.
var sessionIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// importLatch collapses conditions that repeat at full poll cadence into one
// log line per transition. A missing data directory used to emit one ERROR per
// cycle — 6,671 copies of a single fact in one log file.
var importLatch logutil.Latch

// latchHeartbeat is how many suppressed repeats pass before a persistent
// condition is re-stated, so an outage is quiet but never invisible.
const latchHeartbeat = 500

// logLatched emits msg on the transition into a condition and once every
// latchHeartbeat repeats thereafter.
func logLatched(key, cause, msg string, args ...any) {
	first, n := importLatch.Fail(key, cause)
	switch {
	case first:
		slog.Error(msg, args...)
	case n%latchHeartbeat == 0:
		slog.Error(msg+" (still failing)", append(args, "suppressedRepeats", n)...)
	}
}

// clearLatched reports the recovery of a previously latched condition.
func clearLatched(key, msg string, args ...any) {
	if recovered, suppressed := importLatch.OK(key); recovered {
		slog.Info(msg, append(args, "suppressedRepeats", suppressed)...)
	}
}

type sidecarGuard struct {
	mtimeNS int64
	size    int64
}

// loadSidecarGuard reads the (mtime, size) each sessions row was built from.
// The join makes the guard self-correcting: a guard row whose sessions row went
// away does not suppress the re-import.
func loadSidecarGuard(db *sql.DB) (map[string]sidecarGuard, error) {
	rows, err := db.Query(`SELECT si.id, si.mtime_ns, si.size
		FROM sidecar_import si JOIN sessions s ON s.id = si.id`)
	if err != nil {
		return nil, fmt.Errorf("read sidecar guard: %w", err)
	}
	defer rows.Close()
	guard := make(map[string]sidecarGuard)
	for rows.Next() {
		var id string
		var g sidecarGuard
		if err := rows.Scan(&id, &g.mtimeNS, &g.size); err != nil {
			return nil, fmt.Errorf("scan sidecar guard: %w", err)
		}
		guard[id] = g
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read sidecar guard: %w", err)
	}
	return guard, nil
}

// pendingSidecar is one validated sidecar waiting to be written.
type pendingSidecar struct {
	sid     string
	data    string
	stamp   string // mtime in stampLayout, stored as sessions.updated_at
	mtimeNS int64
	size    int64
	stable  bool // the file did not change under us while we read it
}

// ImportSidecars imports session sidecar JSON files from the data directory.
//
// It returns per-pass counts and a real error: an empty directory, a directory
// where nothing changed, and a directory where every file failed used to be
// indistinguishable — all three logged "count=0" at Info and returned nil,
// which is how a five-day data outage stayed invisible.
func ImportSidecars(db *sql.DB, dataDir string) (SidecarStats, error) {
	var st SidecarStats

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		logLatched("sidecars:readdir", err.Error(), "sidecars read dir failed", "dir", dataDir, "err", err)
		return st, fmt.Errorf("read sidecar dir %s: %w", dataDir, err)
	}
	clearLatched("sidecars:readdir", "sidecars read dir recovered", "dir", dataDir)

	guard, err := loadSidecarGuard(db)
	if err != nil {
		return st, err
	}

	var writes []pendingSidecar
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || SidecarExclude[e.Name()] {
			continue
		}
		sid := strings.TrimSuffix(e.Name(), ".json")
		if !sessionIDRe.MatchString(sid) {
			slog.Debug("sidecar skipped: filename is not a session id", "file", e.Name())
			continue
		}
		st.Total++

		fpath := filepath.Join(dataDir, e.Name())
		before, err := os.Stat(fpath)
		if err != nil {
			st.Failed++
			slog.Warn("sidecar stat failed", "file", e.Name(), "err", err)
			continue
		}
		if g, ok := guard[sid]; ok && g.mtimeNS == before.ModTime().UnixNano() && g.size == before.Size() {
			st.Unchanged++
			continue
		}

		data, err := os.ReadFile(fpath)
		if err != nil {
			st.Failed++
			slog.Warn("sidecar read failed", "file", e.Name(), "err", err)
			continue
		}

		// Re-stat after the read. If the file moved under us the bytes we hold
		// may be a half-written mix, so keep the older stamp and do not record a
		// guard entry — the next pass re-reads it rather than trusting this one.
		after, statErr := os.Stat(fpath)
		stable := statErr == nil &&
			after.ModTime().Equal(before.ModTime()) &&
			after.Size() == before.Size()

		data = StripBOM(data)
		if !json.Valid(data) {
			// A truncated sidecar stored verbatim surfaces later as
			// json.RawMessage("") and breaks the encode of the entire dashboard
			// payload — after the 200 has already gone out, and for every
			// websocket client at once. It never reaches the table.
			st.Invalid++
			slog.Warn("sidecar skipped: not valid JSON", "file", e.Name(), "bytes", len(data))
			continue
		}

		writes = append(writes, pendingSidecar{
			sid:     sid,
			data:    string(data),
			stamp:   before.ModTime().UTC().Format(stampLayout),
			mtimeNS: before.ModTime().UnixNano(),
			size:    before.Size(),
			stable:  stable,
		})
	}

	if len(writes) > 0 {
		if err := writeSidecars(db, writes, &st); err != nil {
			return st, err
		}
	}

	switch {
	case st.Total == 0:
		slog.Debug("sidecars: no sidecar files present", "dir", dataDir)
	case st.Imported == 0 && st.Unchanged == 0:
		err := fmt.Errorf("all %d sidecars failed to import (%d unreadable, %d invalid)",
			st.Total, st.Failed, st.Invalid)
		logLatched("sidecars:total-failure", err.Error(), "sidecar import produced nothing",
			"dir", dataDir, "total", st.Total, "failed", st.Failed, "invalid", st.Invalid)
		return st, err
	case st.Imported > 0 || st.Failed > 0 || st.Invalid > 0:
		slog.Info("sidecars imported", "total", st.Total, "imported", st.Imported,
			"unchanged", st.Unchanged, "invalid", st.Invalid, "failed", st.Failed)
	default:
		slog.Debug("sidecars unchanged", "total", st.Total)
	}
	clearLatched("sidecars:total-failure", "sidecar import recovered", "dir", dataDir)
	return st, nil
}

// writeSidecars commits one batch of validated sidecars. Batching keeps a poll
// cycle to a single transaction instead of one WAL-dirtying write per file.
func writeSidecars(db *sql.DB, writes []pendingSidecar, st *SidecarStats) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("sidecar tx: %w", err)
	}
	defer tx.Rollback()

	for _, w := range writes {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO sessions(id, data, updated_at) VALUES(?, ?, ?)`,
			w.sid, w.data, w.stamp); err != nil {
			return fmt.Errorf("sidecar insert %s: %w", w.sid, err)
		}
		st.Imported++
		if w.stable {
			if _, err := tx.Exec(`INSERT OR REPLACE INTO sidecar_import(id, mtime_ns, size) VALUES(?, ?, ?)`,
				w.sid, w.mtimeNS, w.size); err != nil {
				return fmt.Errorf("sidecar guard update %s: %w", w.sid, err)
			}
		} else if _, err := tx.Exec(`DELETE FROM sidecar_import WHERE id = ?`, w.sid); err != nil {
			return fmt.Errorf("sidecar guard clear %s: %w", w.sid, err)
		}
	}

	if err := tx.Commit(); err != nil {
		st.Imported = 0
		return fmt.Errorf("sidecar commit: %w", err)
	}
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

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// INSERT OR IGNORE relies on the unique indexes added in migration v3
	// (history: (ts,data), limit_history: ts).  All lines are attempted on
	// every import, so malformed/skipped lines can never cause subsequent
	// lines to be re-inserted.
	stmt, err := tx.Prepare(fmt.Sprintf("INSERT OR IGNORE INTO %s(ts, data) VALUES(?, ?)", table))
	if err != nil {
		return err
	}
	defer stmt.Close()

	var inserted, malformed, undated, failed int
	var firstErr error
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			malformed++
			continue
		}
		ts, _ := obj["ts"].(string)
		if ts == "" {
			// Substituting time.Now() here made the line unique against the
			// (ts, data) index on every single import, so one undated line was
			// re-inserted forever.  Both tables are pure time series — every
			// consumer orders, filters or plots by ts — so a point with no
			// timestamp is dropped and counted rather than given a fake one.
			undated++
			continue
		}
		res, err := stmt.Exec(ts, line)
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			inserted++
		}
	}

	if failed > 0 {
		return fmt.Errorf("%s: %d line(s) failed to insert: %w", table, failed, firstErr)
	}
	if undated > 0 {
		slog.Warn("jsonl lines skipped: no ts field", "table", table, "path", path, "count", undated)
	}
	if malformed > 0 {
		slog.Warn("jsonl lines skipped: malformed", "table", table, "path", path, "count", malformed)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", table, err)
	}
	if inserted > 0 {
		slog.Debug("jsonl imported", "table", table, "inserted", inserted)
	}
	return nil
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

// summaryCache remembers the last summary scanned out of each session JSONL,
// keyed by path and validated against (mtime, size).
//
// scanSessionJSONLSummaries ran on every /api/data request and every watcher
// flush, line-scanning every .jsonl in every project directory each time. The
// transcripts are append-only and almost always unchanged between two polls a
// second apart, so the guard turns that whole-corpus re-read into one stat per
// file.
var (
	summaryCacheMu sync.Mutex
	summaryCache   = map[string]summaryCacheEntry{}
)

type summaryCacheEntry struct {
	mtimeNS int64
	size    int64
	summary string
}

func scanSessionJSONLSummaries(claudeDir string) map[string]string {
	result := make(map[string]string)
	projectsDir := filepath.Join(claudeDir, "projects")
	projEntries, err := os.ReadDir(projectsDir)
	if err != nil {
		return result
	}

	summaryCacheMu.Lock()
	prev := summaryCache
	summaryCacheMu.Unlock()

	// Rebuilt from what is on disk this pass, so entries for deleted
	// transcripts do not accumulate.
	next := make(map[string]summaryCacheEntry, len(prev))

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
			info, err := f.Info()
			if err != nil {
				continue
			}
			entry, cached := prev[fpath]
			if !cached || entry.mtimeNS != info.ModTime().UnixNano() || entry.size != info.Size() {
				entry = summaryCacheEntry{
					mtimeNS: info.ModTime().UnixNano(),
					size:    info.Size(),
					summary: scanFileForSummary(fpath),
				}
			}
			next[fpath] = entry
			if entry.summary != "" {
				result[sid] = entry.summary
			}
		}
	}

	summaryCacheMu.Lock()
	summaryCache = next
	summaryCacheMu.Unlock()
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

// snapshotCostLookback is how many recent history rows are consulted to rebuild
// the per-session dedup key after a restart.
const snapshotCostLookback = 5000

// loadRecentSnapshotCosts rebuilds the last-known cost per short session id
// from the history already in the table.
//
// The dedup key lived only in memory, so every restart re-emitted one duplicate
// snapshot row per session. history rows carry the 8-character short id, which
// is what the caller's map is keyed against here.
func loadRecentSnapshotCosts(db *sql.DB) (map[string]float64, error) {
	rows, err := db.Query(
		"SELECT data FROM history ORDER BY id DESC LIMIT ?", snapshotCostLookback)
	if err != nil {
		return nil, fmt.Errorf("seed snapshot costs: %w", err)
	}
	defer rows.Close()

	costs := make(map[string]float64)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("seed snapshot costs: %w", err)
		}
		var e struct {
			Sid  string  `json:"sid"`
			Cost float64 `json:"cost"`
		}
		if json.Unmarshal([]byte(raw), &e) != nil || e.Sid == "" {
			continue
		}
		// Rows arrive newest first; the first sighting wins.
		if _, seen := costs[e.Sid]; !seen {
			costs[e.Sid] = e.Cost
		}
	}
	return costs, rows.Err()
}

// SnapshotSidecarsToHistory appends a history point for every session whose
// cumulative cost has moved since the last snapshot.
//
// The read is fully drained and the cursor closed before anything is written.
// The previous version ran an INSERT from inside the rows.Next() loop, so a
// second pooled connection appended WAL frames while the first connection's
// read snapshot was still open — pinning the WAL against checkpointing, and
// deadlocking outright once the pool is limited to a single connection.
func SnapshotSidecarsToHistory(db *sql.DB, lastSessionSnapshot map[string]float64) error {
	type sessionRow struct{ sid, raw string }

	rows, err := db.Query("SELECT id, data FROM sessions")
	if err != nil {
		return fmt.Errorf("snapshot: read sessions: %w", err)
	}
	var sessions []sessionRow
	for rows.Next() {
		var r sessionRow
		if err := rows.Scan(&r.sid, &r.raw); err != nil {
			rows.Close()
			return fmt.Errorf("snapshot: scan session: %w", err)
		}
		sessions = append(sessions, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("snapshot: read sessions: %w", err)
	}
	rows.Close()

	// An empty map means this process has not snapshotted yet — rebuild the
	// dedup key from history so a restart does not re-emit every session.
	seed := map[string]float64{}
	if len(lastSessionSnapshot) == 0 {
		seed, err = loadRecentSnapshotCosts(db)
		if err != nil {
			slog.Warn("snapshot: could not seed dedup key from history", "err", err)
			seed = map[string]float64{}
		}
	}

	now := nowStamp()
	var pending []string

	for _, r := range sessions {
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
		if json.Unmarshal([]byte(r.raw), &sc) != nil || sc.Cumulative == nil {
			continue
		}
		c := sc.Cumulative
		cost := math.Round(c.Cost*100) / 100

		shortSid := r.sid
		if len(shortSid) > 8 {
			shortSid = shortSid[:8]
		}

		if prev, ok := lastSessionSnapshot[r.sid]; ok && prev == cost {
			continue
		}
		if prev, ok := seed[shortSid]; ok && prev == cost {
			lastSessionSnapshot[r.sid] = cost
			continue
		}
		lastSessionSnapshot[r.sid] = cost

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
		data, err := json.Marshal(entry)
		if err != nil {
			slog.Warn("snapshot: could not encode entry", "sid", shortSid, "err", err)
			continue
		}
		pending = append(pending, string(data))
	}

	if len(pending) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("snapshot: begin: %w", err)
	}
	defer tx.Rollback()

	// OR IGNORE, matching every other insert in this file: the unique (ts, data)
	// index from migration v3 is the dedup of last resort, and a plain INSERT
	// turned that into an error that the old code swallowed.
	stmt, err := tx.Prepare("INSERT OR IGNORE INTO history(ts, data) VALUES(?, ?)")
	if err != nil {
		return fmt.Errorf("snapshot: prepare: %w", err)
	}
	defer stmt.Close()

	snapshotted := 0
	for _, data := range pending {
		res, err := stmt.Exec(now, data)
		if err != nil {
			return fmt.Errorf("snapshot: insert: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			snapshotted++
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("snapshot: commit: %w", err)
	}

	if snapshotted > 0 {
		slog.Info("sidecars snapshotted", "count", snapshotted)
	}
	return nil
}

// --- Dashboard Data ---

type DashboardData struct {
	GeneratedAt      string                 `json:"generatedAt"`
	UsageConfig      json.RawMessage        `json:"usageConfig"`
	StatuslineConfig json.RawMessage        `json:"statuslineConfig"`
	Sessions         []any                  `json:"sessions"`
	History          []json.RawMessage      `json:"history"`
	Sidecars         []SidecarEntry         `json:"sidecars"`
	LiveUsage        json.RawMessage        `json:"liveUsage"`
	Profile          json.RawMessage        `json:"profile"`
	SessionMeta      json.RawMessage        `json:"sessionMeta"`
	LimitHistory     []json.RawMessage      `json:"limitHistory"`
	Layout           json.RawMessage        `json:"layout"`
	PhantomUsage     *analytics.PhantomData `json:"phantomUsage,omitempty"`
	Teams            json.RawMessage        `json:"teams,omitempty"`
	LiveEffort       string                 `json:"live_effort,omitempty"`

	// HistoryHourly is the exact per-hour row count of the FULL history table.
	// It is only populated when History has been thinned, because an untouched
	// History already contains every row it summarises — which keeps the
	// default response byte-identical to what it always was.
	HistoryHourly   *HistoryHourly `json:"historyHourly,omitempty"`
	BurnRatePerHour float64        `json:"burnRatePerHour,omitempty"`
}

type SidecarEntry struct {
	ID        string          `json:"id"`
	Data      json.RawMessage `json:"data"`
	UpdatedAt string          `json:"updated_at,omitempty"`
}

// scanLatch dedups the "row is not valid JSON" warnings, which would otherwise
// repeat for the same poisoned row on every request.
var scanLatch logutil.Latch

// warnCorrupt logs a corrupt row once per (table, row) until it is fixed.
func warnCorrupt(table, id string, n int) {
	if first, _ := scanLatch.Fail("corrupt:"+table+":"+id, fmt.Sprint(n)); first {
		slog.Warn("dropping row with invalid JSON from dashboard payload",
			"table", table, "id", id, "bytes", n)
	}
}

// querySidecars reads the sessions table.
//
// updated_at is normalised on read: a row that fell back to the
// CURRENT_TIMESTAMP default renders with a space instead of a T, and ' ' sorts
// before 'T', so unnormalised ordering would file such a row wrongly.
func querySidecars(db *sql.DB) ([]SidecarEntry, error) {
	rows, err := db.Query(`SELECT id, data, updated_at FROM sessions
		ORDER BY replace(updated_at, ' ', 'T') DESC`)
	if err != nil {
		return nil, fmt.Errorf("sidecars query: %w", err)
	}
	defer rows.Close()

	out := []SidecarEntry{}
	for rows.Next() {
		var id, data, updatedAt string
		if err := rows.Scan(&id, &data, &updatedAt); err != nil {
			return nil, fmt.Errorf("sidecars scan: %w", err)
		}
		// A single invalid row would otherwise reach json.RawMessage and break
		// the encode of the whole payload — mid-body, after the 200 had gone
		// out, and for every websocket client at once.
		if !json.Valid([]byte(data)) {
			warnCorrupt("sessions", id, len(data))
			continue
		}
		out = append(out, SidecarEntry{
			ID:        id,
			Data:      json.RawMessage(data),
			UpdatedAt: updatedAt,
		})
	}
	return out, rows.Err()
}

// queryRawColumn reads one JSON column, dropping rows that would not encode.
func queryRawColumn(db *sql.DB, table, query string) ([]json.RawMessage, error) {
	return queryRawColumnArgs(db, table, query)
}

// queryRawColumnArgs is queryRawColumn with bind parameters, used by the
// windowed history reads in history.go.
func queryRawColumnArgs(db *sql.DB, table, query string, args ...any) ([]json.RawMessage, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s query: %w", table, err)
	}
	defer rows.Close()

	out := []json.RawMessage{}
	for rows.Next() {
		var id int64
		var data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, fmt.Errorf("%s scan: %w", table, err)
		}
		if !json.Valid([]byte(data)) {
			warnCorrupt(table, fmt.Sprint(id), len(data))
			continue
		}
		out = append(out, json.RawMessage(data))
	}
	return out, rows.Err()
}

// BuildDashboardData assembles the /api/data payload.
//
// Every query result is materialised and its cursor closed before the next
// query runs — the old version deferred all three closes to function exit and
// then called into CalcPhantomUsage, which opened three more, so a single
// dashboard build held six read snapshots at once.
//
// A failed query is returned, not swallowed: the previous `if err == nil` with
// no else rendered a locked database as HTTP 200 with empty arrays and no log
// line at all.
func BuildDashboardData(db *sql.DB, dataDir string) (*DashboardData, error) {
	return BuildDashboardDataQuery(db, dataDir, HistoryQuery{})
}

// BuildDashboardDataQuery is BuildDashboardData with control over which history
// rows the payload carries. The zero HistoryQuery reproduces BuildDashboardData
// exactly, which is what keeps every pre-existing caller and every pre-existing
// dashboard client on the response they already know.
func BuildDashboardDataQuery(db *sql.DB, dataDir string, hq HistoryQuery) (*DashboardData, error) {
	d := &DashboardData{
		GeneratedAt:  time.Now().Format(time.RFC3339),
		Sessions:     []any{},
		Sidecars:     []SidecarEntry{},
		History:      []json.RawMessage{},
		LimitHistory: []json.RawMessage{},
	}

	sidecars, err := querySidecars(db)
	if err != nil {
		return nil, err
	}
	d.Sidecars = sidecars

	// The rows are read once and thinned in memory so the exact hour-of-day
	// histogram can be taken from the full set before anything is dropped.
	rawHistory, err := QueryHistory(db, HistoryQuery{From: hq.From, To: hq.To, Mode: modeForRead(hq.Mode)})
	if err != nil {
		return nil, err
	}
	d.History = hq.apply(rawHistory)
	if hq.Mode == HistoryRollup {
		d.HistoryHourly = HistoryHourlyCounts(rawHistory)
	}

	limitHistory, err := queryRawColumn(db, "limit_history",
		"SELECT id, data FROM limit_history ORDER BY replace(ts, ' ', 'T') ASC")
	if err != nil {
		return nil, err
	}
	d.LimitHistory = limitHistory

	// KV
	d.UsageConfig = KVGet(db, "config:usage")
	d.StatuslineConfig = KVGet(db, "config:statusline")
	d.LiveUsage = KVGet(db, "cache:usage-api")
	d.Profile = KVGet(db, "cache:profile")
	d.SessionMeta = KVGet(db, "cache:session-meta")
	d.Layout = KVGet(db, "config:layout")
	d.PhantomUsage = analytics.CalcPhantomUsage(db)
	d.Teams = KVGet(db, "cache:teams")
	if br, ok := forecast.LocalBurnRate(dataDir, time.Hour); ok && br > 0 {
		d.BurnRatePerHour = br
	}

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

	now := time.Now().UTC().Format(time.RFC3339)
	current["ts"] = now
	dataWithTS, _ := json.Marshal(current)
	if _, err := db.Exec("INSERT INTO limit_history(ts, data) VALUES(?, ?)", now, string(dataWithTS)); err != nil {
		slog.Error("limit snapshot insert failed", "err", err)
		return
	}
	slog.Info("limit snapshot written", "pct5hr", current["pct5hr"], "pctWeekly", current["pctWeekly"])

	// The JSONL mirror is what internal/forecast reads and what ImportFileData
	// replays into a fresh DB. dataDir may not exist yet, and the open failure
	// used to be discarded without so much as a log line.
	if err := appendLimitHistoryLine(dataDir, dataWithTS); err != nil {
		slog.Error("limit snapshot JSONL append failed", "dir", dataDir, "err", err)
	}
}

// appendLimitHistoryLine appends one snapshot to limit-history.jsonl, creating
// the data directory if it is missing.
func appendLimitHistoryLine(dataDir string, line []byte) error {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dataDir, err)
	}
	f, err := os.OpenFile(filepath.Join(dataDir, "limit-history.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Tiered rollup thresholds for limit_history.
//
// The old policy was a pure time-gap thinner — "keep whichever sample happens
// to come first in each 5/60/240-minute window, drop the rest". Two problems.
// It was far too lax (the 24h–7d tier alone licensed 1728 rows at 5-minute
// spacing, which is most of the 2907 rows / 1.28 MB the live table was
// shipping in every /api/data response and every websocket broadcast), and
// because it kept the *first* sample of each window rather than the extremes,
// a rate-limit spike that peaked between two grid points was silently erased.
//
// The replacement buckets by wall-clock time and keeps the rows that carry the
// shape: the bucket minimum, the bucket maximum, and the bucket's last sample.
// Peaks survive by construction. So does the sawtooth trough after a 5h-window
// reset, which is what the minimum is there for — the limit-timeline widget
// draws a line through these points, and dropping the trough turns a reset
// edge into a plateau.
const (
	// limitFullResolutionAge: everything newer is kept verbatim. The widget's
	// 6h and 24h ranges plot these samples one-for-one and computeWeightedRate
	// regresses over them, so any thinning here is visible. At the 60s snapshot
	// cadence this tier is bounded at ~1440 rows.
	limitFullResolutionAge = 24 * time.Hour

	// limitHourlyAge: from 24h out to 30d, one hourly bucket. The widget's
	// widest bucketed ranges are 8d and 30d; 30 days is 720 hours drawn across
	// a chart a few hundred pixels wide, so an hourly bucket is already at or
	// finer than the rendering resolution and extra samples cannot be seen.
	limitHourlyAge = 30 * 24 * time.Hour

	// Beyond limitHourlyAge the bucket is a UTC day. Past 30 days the only
	// range the widget offers is "all", where a 6-month span leaves well under
	// a pixel per hour; a day's min/max/last is precisely the band it can draw.
	limitDailyBucket = 24 * time.Hour
)

// limitEntry is one limit_history row, decoded down to the fields the rollup
// reasons about.
type limitEntry struct {
	id        int64
	ts        time.Time
	data      string
	pct5hr    float64
	pctWeekly float64
}

// limitBucketKey groups rows that compete for the same slot. The tier is part
// of the key so an hourly bucket and a daily bucket can never collide.
type limitBucketKey struct {
	tier  int
	start time.Time
}

// planLimitCompaction returns the ids to delete, applying the tiered rollup
// above plus the same 365-day retention `history` uses.
//
// Buckets are aligned to absolute UTC time rather than to `now`, which makes
// the function idempotent: re-running it at the same instant picks the same
// extrema out of the survivors and deletes nothing more. A gap-from-last-kept
// scheme has no such property.
//
// Nothing inside the retention window is deleted outright — a row is only ever
// dropped because a neighbour in its own bucket carries the same shape.
func planLimitCompaction(all []limitEntry, now time.Time) []int64 {
	if len(all) == 0 {
		return nil
	}

	// The caller's query orders by ts, but do not depend on it: bucket
	// selection needs "last in bucket" to really be last.
	sorted := make([]limitEntry, len(all))
	copy(sorted, all)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ts.Before(sorted[j].ts) })

	keep := make(map[int64]bool, len(sorted))
	buckets := make(map[limitBucketKey][]int)
	var expired []int64

	for i, e := range sorted {
		age := now.Sub(e.ts)
		switch {
		case age > HistoryRetention:
			// Outside retention. Consistent with CompactHistory.
			expired = append(expired, e.id)
		case age <= limitFullResolutionAge:
			keep[e.id] = true
		case age <= limitHourlyAge:
			k := limitBucketKey{tier: 1, start: e.ts.UTC().Truncate(time.Hour)}
			buckets[k] = append(buckets[k], i)
		default:
			k := limitBucketKey{tier: 2, start: e.ts.UTC().Truncate(limitDailyBucket)}
			buckets[k] = append(buckets[k], i)
		}
	}

	for _, idxs := range buckets {
		// Minimum keeps the first sample of a rising bucket and, more
		// importantly, the trough left by a mid-bucket window reset.
		// Maximum keeps the peak — the whole reason this is not a mean.
		// Last anchors the bucket's right edge so the line joins the next
		// bucket at the right value.
		minIdx, maxIdx, wkMaxIdx := idxs[0], idxs[0], idxs[0]
		for _, i := range idxs {
			if sorted[i].pct5hr < sorted[minIdx].pct5hr {
				minIdx = i
			}
			// >= so a plateau keeps its trailing edge rather than its leading one.
			if sorted[i].pct5hr >= sorted[maxIdx].pct5hr {
				maxIdx = i
			}
			if sorted[i].pctWeekly >= sorted[wkMaxIdx].pctWeekly {
				wkMaxIdx = i
			}
		}
		keep[sorted[minIdx].id] = true
		keep[sorted[maxIdx].id] = true
		keep[sorted[wkMaxIdx].id] = true
		keep[sorted[idxs[len(idxs)-1]].id] = true
	}

	deleteIDs := expired
	for _, idxs := range buckets {
		for _, i := range idxs {
			if !keep[sorted[i].id] {
				deleteIDs = append(deleteIDs, sorted[i].id)
			}
		}
	}
	sort.Slice(deleteIDs, func(i, j int) bool { return deleteIDs[i] < deleteIDs[j] })
	return deleteIDs
}

// limitNum pulls a numeric field out of a decoded snapshot, tolerating the
// number-as-string forms older rows occasionally carry.
func limitNum(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case json.Number:
		f, _ := v.Float64()
		return f
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0
		}
		return f
	}
	return 0
}

// CompactLimitHistory rolls limit_history up in tiers: full resolution inside
// limitFullResolutionAge, hourly buckets out to limitHourlyAge, daily buckets
// beyond, and nothing older than HistoryRetention.
//
// Rows that survive are the ORIGINAL rows, byte for byte. No averaged or
// synthesised row is ever written: the dashboard reads reset5hr, resetWeekly
// and wtWeekly straight off these objects, and a fabricated row would have no
// coherent value for them. The /api/data JSON contract is unchanged — this
// only reduces how many elements the limitHistory array carries.
//
// The whole prune runs as one transaction with a rollback path. The old version
// began a transaction with no `defer tx.Rollback()`, logged a failed delete and
// fell straight through to Commit — committing a partial compaction — and the
// commit-failure branch returned without rolling back either.
func CompactLimitHistory(db *sql.DB, dataDir string) error {
	type entry = limitEntry

	// Drain and close the cursor before any write: on a single-connection pool
	// a write issued while this is open would deadlock.
	rows, err := db.Query("SELECT id, ts, data FROM limit_history ORDER BY replace(ts, ' ', 'T') ASC")
	if err != nil {
		return fmt.Errorf("compact: read limit_history: %w", err)
	}
	var all []entry
	for rows.Next() {
		var e entry
		var tsStr string
		if err := rows.Scan(&e.id, &tsStr, &e.data); err != nil {
			rows.Close()
			return fmt.Errorf("compact: scan limit_history: %w", err)
		}
		if t, err := time.Parse(time.RFC3339, tsStr); err == nil {
			e.ts = t
		} else if t, err := time.Parse(time.RFC3339Nano, tsStr); err == nil {
			e.ts = t
		} else {
			continue
		}
		// A row whose data will not decode gets zeroed metrics rather than
		// being skipped: it still occupies a bucket slot and must stay
		// eligible for pruning, but it must never win an extremum on garbage.
		var m map[string]any
		if json.Unmarshal([]byte(e.data), &m) == nil {
			e.pct5hr = limitNum(m, "pct5hr")
			e.pctWeekly = limitNum(m, "pctWeekly")
		}
		all = append(all, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("compact: read limit_history: %w", err)
	}
	rows.Close()

	if len(all) < 100 {
		return nil
	}

	// Fix ts-less entries permanently
	for _, e := range all {
		if strings.Contains(e.data[:min(len(e.data), 30)], `"ts"`) {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(e.data), &m) != nil {
			continue
		}
		if _, ok := m["ts"]; ok {
			continue
		}
		m["ts"] = e.ts.Format(time.RFC3339)
		patched, err := json.Marshal(m)
		if err != nil {
			continue
		}
		if _, err := db.Exec("UPDATE limit_history SET data = ? WHERE id = ?", string(patched), e.id); err != nil {
			slog.Warn("compact: ts patch failed", "id", e.id, "err", err)
		}
	}

	deleteIDs := planLimitCompaction(all, time.Now())

	if len(deleteIDs) == 0 {
		slog.Debug("compact: no entries pruned", "total", len(all))
		return nil
	}

	if err := deleteLimitHistory(db, deleteIDs); err != nil {
		return err
	}
	slog.Info("compact: pruned entries", "pruned", len(deleteIDs), "total", len(all))

	return rewriteLimitHistoryJSONL(db, dataDir)
}

// deleteLimitHistory removes the given ids in a single all-or-nothing
// transaction, in chunks small enough for SQLite's parameter limit.
func deleteLimitHistory(db *sql.DB, ids []int64) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("compact: begin: %w", err)
	}
	defer tx.Rollback()

	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		end := min(start+chunk, len(ids))
		batch := ids[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		if _, err := tx.Exec("DELETE FROM limit_history WHERE id IN ("+placeholders+")", args...); err != nil {
			return fmt.Errorf("compact: delete: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("compact: commit: %w", err)
	}
	return nil
}

// rewriteLimitHistoryJSONL regenerates the on-disk JSONL from the surviving rows.
func rewriteLimitHistoryJSONL(db *sql.DB, dataDir string) error {
	rows, err := db.Query("SELECT ts, data FROM limit_history ORDER BY replace(ts, ' ', 'T') ASC")
	if err != nil {
		return fmt.Errorf("compact: surviving query: %w", err)
	}
	type row struct{ ts, data string }
	var surviving []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ts, &r.data); err != nil {
			rows.Close()
			return fmt.Errorf("compact: surviving scan: %w", err)
		}
		surviving = append(surviving, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("compact: surviving rows: %w", err)
	}
	rows.Close()

	// Build the whole file before touching the live one. os.Create truncated
	// limit-history.jsonl in place, and `periscope serve` runs this at startup
	// while the statusline process is reading the same file — so a reader
	// could see an empty or half-written history, and a failure part-way
	// through destroyed it outright.
	var buf bytes.Buffer
	for _, r := range surviving {
		data := r.data
		if !strings.Contains(data[:min(len(data), 30)], `"ts"`) {
			var m map[string]any
			if json.Unmarshal([]byte(data), &m) == nil {
				if _, ok := m["ts"]; !ok {
					m["ts"] = r.ts
					if patched, err := json.Marshal(m); err == nil {
						data = string(patched)
					}
				}
			}
		}
		buf.WriteString(data)
		buf.WriteByte('\n')
	}

	if err := writeFileAtomic(filepath.Join(dataDir, "limit-history.jsonl"), buf.Bytes()); err != nil {
		return fmt.Errorf("compact: JSONL rewrite: %w", err)
	}
	return nil
}

// writeFileAtomic writes data to a uniquely named temp file in path's
// directory, fsyncs it and renames it into place, so a concurrent reader sees
// either the old file or the new one and never a partial one. It creates the
// directory if it is missing and preserves an existing file's mode.
//
// This mirrors writeFileAtomic in the root package, which internal/store
// cannot import. The temp name keeps its leading dot and does not end in
// .json or .jsonl, so the directory scans that build the dashboard payload
// skip it.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	perm := os.FileMode(0644)
	if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
		perm = fi.Mode().Perm()
	}

	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			os.Remove(tmp)
		}
	}()

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	tmp = ""
	return nil
}
