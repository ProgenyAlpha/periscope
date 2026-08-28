package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// --- Transport-side history downsampling ---
//
// The `history` table is the single biggest thing /api/data ships: on a live
// install it was 3.5 MB of a 4.5 MB payload, 24k rows, and every byte of it
// went out again on every websocket push.
//
// NOTHING HERE DELETES A ROW. This is a transport decision only. CompactHistory
// still owns the table's 365-day retention; a row that is thinned out of a
// response is still in SQLite and still comes back at full resolution the
// moment a caller asks for its window (see QueryHistory / /api/history).
//
// Why first-and-last-per-(session, bucket) rather than a mean, LTTB, or a
// min/max band:
//
//   - A mean is disqualified outright. Averaging a $500 day with 23 quiet hours
//     hides the very day someone opened the dashboard to find.
//
//   - LTTB and min/max bands both assume ONE continuous series. `history` is not
//     that: it is many interleaved per-session snapshots, each row a CUMULATIVE
//     counter for one session (cost/cr/cw/input/out/turns only ever climb within
//     a session). A global extremum over the mixed series is meaningless — the
//     "max" is just whichever session happens to be oldest and biggest.
//
//   - Every consumer of this array computes a per-session DELTA: max-min for a
//     session inside some window (cost-overview, cache-savings, limit-timeline),
//     or first/last timestamps (session-list). For a monotonic counter the
//     first and last rows of a bucket ARE its minimum and maximum, so keeping
//     both makes every one of those deltas EXACT at bucket granularity rather
//     than merely peak-preserving. A session's lifetime total is exact for any
//     bucket size, because its global first and last rows are always the
//     first and last of some bucket.
//
//   - The rows that survive are the ORIGINAL rows, byte for byte. No synthesised
//     or averaged row is ever emitted, exactly as planLimitCompaction promises
//     for limit_history. Widgets read `sid`, `ts`, `effort` and the six counters
//     straight off these objects and a fabricated row would have no honest value
//     for them.
//
// A max-cost guard is kept alongside first/last so that a row which breaks the
// monotonic assumption (a session id reused after a reset, a repaired sidecar)
// still cannot hide a peak.

// Tiered rollup thresholds, mirroring the limit_history tiers in db.go.
const (
	// HistoryFullResolutionAge: everything newer than this is shipped verbatim.
	// A week, not the 24h limit_history uses, because the history consumers
	// explicitly reach back seven days at full detail — rate-limits' duty-cycle
	// card and runtime.html's estimateDutyCycle both filter on `> weekAgo`, and
	// usage-timeline's 5h/24h/7d ranges all land inside it. Anything thinner
	// here would be visible in the default view.
	HistoryFullResolutionAge = 7 * 24 * time.Hour

	// HistoryHourlyAge: from a week out to 30 days, one bucket per session per
	// hour. Hourly rather than daily because activity-breakdown's heatmap bins
	// by hour-of-day; a daily bucket would collapse a session that ran 09:00 to
	// 17:00 down to two hours and visibly distort that chart.
	HistoryHourlyAge = 30 * 24 * time.Hour

	// Beyond HistoryHourlyAge the bucket is a UTC day. At 191 days that is one
	// bucket per session per day, well under a pixel per hour on any chart the
	// dashboard draws at that range.
	HistoryDailyBucket = 24 * time.Hour
)

// MaxFullResolutionSpan bounds a full-resolution /api/history request. A caller
// wanting more than a month at once has to say what bucket it wants, which is
// how "give me the whole table" is rejected instead of served.
const MaxFullResolutionSpan = 31 * 24 * time.Hour

// MinHistoryBucket is the smallest bucket worth asking for. Below a minute the
// snapshot cadence itself is coarser, so the request is nonsense.
const MinHistoryBucket = time.Minute

// HistoryMode selects the shape of the history array in a response.
type HistoryMode int

const (
	// HistoryFull is the zero value: every row in range, untouched. This is
	// what an unparameterised /api/data has always returned and must keep
	// returning.
	HistoryFull HistoryMode = iota
	// HistoryRollup applies the tiered downsampling above.
	HistoryRollup
	// HistoryNone omits the rows entirely, keeping the (empty) array.
	HistoryNone
)

// HistoryQuery selects which history rows a response carries. Its zero value is
// deliberately today's behaviour: full mode, no bounds, no downsampling.
type HistoryQuery struct {
	Mode HistoryMode
	From time.Time // zero: unbounded below
	To   time.Time // zero: unbounded above

	// Bucket forces a single uniform bucket width instead of the tiers. Only
	// consulted when Mode is HistoryRollup. Zero means "use the tiers".
	Bucket time.Duration

	// Now anchors the tier ages. Zero means time.Now(), which is what every
	// caller outside tests wants.
	Now time.Time
}

func (q HistoryQuery) now() time.Time {
	if q.Now.IsZero() {
		return time.Now()
	}
	return q.Now
}

// historyStampLayout is the comparison form used for the ts column. It matches
// the normalisation CompactHistory already applies, so a legacy row written by
// the CURRENT_TIMESTAMP default ("2026-01-01 00:00:00") is compared correctly
// against the RFC3339 stamps everything else writes.
const historyStampLayout = "2006-01-02T15:04:05"

// HistoryHourly is an exact per-hour row count for the whole history table,
// as an unbroken run of hourly buckets starting at Start.
//
// It exists because activity-breakdown draws a lifetime hour-of-day heatmap and
// a PEAK HOUR by COUNTING rows, and a row count is the one thing no rollup can
// preserve: collapsing 300 snapshots in an hour down to two changes the count
// by definition. Measured on a real install the thinned array moved the
// widget's peak hour by seven hours.
//
// So the exact counts travel alongside the thinned array, at a few kilobytes
// instead of the megabytes of rows they summarise.
//
// The buckets are absolute hours, not hour-of-day, because only the browser
// knows the viewer's timezone — the dashboard is reachable over the LAN and a
// phone need not share the server's zone — and because a per-date offset is the
// only way to get DST right. Binning absolute hours into local hour-of-day is
// then the same Date arithmetic the widget already did per row, just once per
// hour instead of once per snapshot, and it is exact.
type HistoryHourly struct {
	Start  string `json:"start"`  // RFC3339 UTC, hour-aligned
	Counts []int  `json:"counts"` // one per hour from Start, inclusive
}

// maxHistoryHourlyBuckets bounds the array at the table's own retention, so a
// single row with a corrupt far-future timestamp cannot make it enormous.
const maxHistoryHourlyBuckets = int(HistoryRetention/time.Hour) + 24

// HistoryHourlyCounts builds the exact hourly histogram from every row. Rows
// with an unreadable timestamp are skipped: they have no hour to be counted in.
// Returns nil when there is nothing to summarise.
func HistoryHourlyCounts(rows []json.RawMessage) *HistoryHourly {
	var first, last time.Time
	stamps := make([]time.Time, 0, len(rows))
	for _, raw := range rows {
		var p historyPoint
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		ts, ok := parseHistoryTS(p.TS)
		if !ok {
			continue
		}
		ts = ts.UTC().Truncate(time.Hour)
		stamps = append(stamps, ts)
		if first.IsZero() || ts.Before(first) {
			first = ts
		}
		if last.IsZero() || ts.After(last) {
			last = ts
		}
	}
	if len(stamps) == 0 {
		return nil
	}
	n := int(last.Sub(first)/time.Hour) + 1
	if n > maxHistoryHourlyBuckets {
		n = maxHistoryHourlyBuckets
	}
	counts := make([]int, n)
	for _, ts := range stamps {
		i := int(ts.Sub(first) / time.Hour)
		if i >= 0 && i < n {
			counts[i]++
		}
	}
	return &HistoryHourly{Start: first.Format(time.RFC3339), Counts: counts}
}

// QueryHistory reads the history table for one HistoryQuery.
//
// The zero query issues byte-for-byte the statement BuildDashboardData has
// always issued, including its ordering, so an old client sees an identical
// array.
func QueryHistory(db *sql.DB, q HistoryQuery) ([]json.RawMessage, error) {
	if q.Mode == HistoryNone {
		return []json.RawMessage{}, nil
	}

	query := "SELECT id, data FROM history"
	var args []any
	var conds []string
	// substr+replace matches CompactHistory. It costs a scan of a table bounded
	// at one year of snapshots (~24k rows on the largest install seen), which is
	// cheaper than the alternative of trusting the raw column's mixed
	// separators to sort correctly against an index.
	if !q.From.IsZero() {
		conds = append(conds, "replace(substr(ts, 1, 19), ' ', 'T') >= ?")
		args = append(args, q.From.UTC().Format(historyStampLayout))
	}
	if !q.To.IsZero() {
		conds = append(conds, "replace(substr(ts, 1, 19), ' ', 'T') <= ?")
		args = append(args, q.To.UTC().Format(historyStampLayout))
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY replace(ts, ' ', 'T') ASC"

	rows, err := queryRawColumnArgs(db, "history", query, args...)
	if err != nil {
		return nil, err
	}
	return q.apply(rows), nil
}

// apply performs the mode's downsampling on rows already read.
func (q HistoryQuery) apply(rows []json.RawMessage) []json.RawMessage {
	if q.Mode != HistoryRollup {
		return rows
	}
	if q.Bucket > 0 {
		return BucketHistoryUniform(rows, q.Bucket)
	}
	return RollupHistory(rows, q.now())
}

// historyTier is one age band of the rollup. Bucket zero means "keep verbatim".
type historyTier struct {
	maxAge time.Duration
	bucket time.Duration
}

// defaultHistoryTiers is the tiering described at the top of this file.
var defaultHistoryTiers = []historyTier{
	{maxAge: HistoryFullResolutionAge, bucket: 0},
	{maxAge: HistoryHourlyAge, bucket: time.Hour},
	{maxAge: 1<<63 - 1, bucket: HistoryDailyBucket},
}

// RollupHistory applies the tiered downsampling to an already-loaded history
// array and returns a subset of the SAME json.RawMessage values, in the same
// order. Rows it cannot parse are always kept: unrecognised data is not
// something to silently drop from a transport.
func RollupHistory(rows []json.RawMessage, now time.Time) []json.RawMessage {
	return planHistory(rows, now, defaultHistoryTiers)
}

// BucketHistoryUniform applies one bucket width across the whole input,
// ignoring age. A zero or negative bucket returns the input unchanged.
func BucketHistoryUniform(rows []json.RawMessage, bucket time.Duration) []json.RawMessage {
	if bucket <= 0 {
		return rows
	}
	// maxAge is irrelevant with a single tier; anchor `now` at the newest row so
	// every row lands in the one band.
	return planHistory(rows, time.Time{}, []historyTier{{maxAge: 1<<63 - 1, bucket: bucket}})
}

// historyPoint is a history row decoded down to the fields the rollup reasons
// about. Everything else stays in the raw bytes and is passed through untouched.
type historyPoint struct {
	Sid  string          `json:"sid"`
	TS   string          `json:"ts"`
	Cost json.RawMessage `json:"cost"`
}

// historyBucketKey groups rows competing for the same slot. The session is part
// of the key because two sessions running in the same hour are two independent
// cumulative series and must not evict each other; the tier is part of it so an
// hourly bucket and a daily bucket can never collide.
type historyBucketKey struct {
	sid    string
	tier   int
	bucket time.Time
}

// planHistory is the shared engine behind RollupHistory and
// BucketHistoryUniform. It returns the kept rows in input order.
func planHistory(rows []json.RawMessage, now time.Time, tiers []historyTier) []json.RawMessage {
	if len(rows) == 0 {
		return []json.RawMessage{}
	}
	keep := make(map[int]bool, len(rows))
	buckets := make(map[historyBucketKey][]int)
	costs := make(map[int]float64, len(rows))

	for i, raw := range rows {
		var p historyPoint
		if err := json.Unmarshal(raw, &p); err != nil {
			keep[i] = true
			continue
		}
		ts, ok := parseHistoryTS(p.TS)
		if !ok {
			keep[i] = true
			continue
		}
		tier, bucket := pickHistoryTier(tiers, now, ts)
		if bucket == 0 {
			keep[i] = true
			continue
		}
		costs[i] = historyCost(p.Cost)
		k := historyBucketKey{sid: p.Sid, tier: tier, bucket: ts.UTC().Truncate(bucket)}
		buckets[k] = append(buckets[k], i)
	}

	for _, idxs := range buckets {
		// First: the bucket's floor. For a cumulative counter this is the
		// minimum, which is what makes a windowed delta exact rather than
		// approximate.
		keep[idxs[0]] = true
		// Last: the bucket's ceiling, and the point the next bucket's line
		// segment joins.
		keep[idxs[len(idxs)-1]] = true
		// Max cost: a guard for rows that are not monotonic (a reused session
		// id, a repaired sidecar). Without it such a peak could hide.
		maxIdx := idxs[0]
		for _, i := range idxs {
			if costs[i] >= costs[maxIdx] {
				maxIdx = i
			}
		}
		keep[maxIdx] = true
	}

	out := make([]int, 0, len(keep))
	for i := range keep {
		out = append(out, i)
	}
	sort.Ints(out)

	kept := make([]json.RawMessage, 0, len(out))
	for _, i := range out {
		kept = append(kept, rows[i])
	}
	return kept
}

// pickHistoryTier returns the tier index and bucket width for one row's age.
// A zero `now` means the tiers are not age-gated (single-bucket mode).
func pickHistoryTier(tiers []historyTier, now, ts time.Time) (int, time.Duration) {
	if now.IsZero() {
		return len(tiers) - 1, tiers[len(tiers)-1].bucket
	}
	age := now.Sub(ts)
	for i, t := range tiers {
		if age <= t.maxAge {
			return i, t.bucket
		}
	}
	last := len(tiers) - 1
	return last, tiers[last].bucket
}

// historyTSLayouts covers every separator the ts field has ever carried.
var historyTSLayouts = []string{
	time.RFC3339,
	time.RFC3339Nano,
	historyStampLayout,
	"2006-01-02 15:04:05",
	"2006-01-02 15:04:05Z07:00",
}

func parseHistoryTS(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range historyTSLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// historyCost tolerates the number-as-string form old rows occasionally carry,
// the same way limitNum does for limit_history.
func historyCost(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return 0
	}
	s := strings.Trim(string(raw), `"`)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

// ValidateHistoryRange rejects a window before it reaches SQLite, so a nonsense
// request is a 400 rather than a scan.
func ValidateHistoryRange(from, to time.Time, bucket time.Duration) error {
	switch {
	case from.IsZero():
		return fmt.Errorf("from is required")
	case to.IsZero():
		return fmt.Errorf("to is required")
	case !to.After(from):
		return fmt.Errorf("to must be after from")
	}
	if bucket < 0 {
		return fmt.Errorf("bucket must not be negative")
	}
	if bucket > 0 && bucket < MinHistoryBucket {
		return fmt.Errorf("bucket must be at least %v", MinHistoryBucket)
	}
	span := to.Sub(from)
	if span > HistoryRetention {
		return fmt.Errorf("range of %v exceeds the %v retention window",
			span.Round(time.Hour), HistoryRetention)
	}
	if bucket == 0 && span > MaxFullResolutionSpan {
		return fmt.Errorf("range of %v exceeds the %v full-resolution limit; pass bucket= to downsample",
			span.Round(time.Hour), MaxFullResolutionSpan)
	}
	return nil
}

// CurrentSchemaVersion exposes the migration version doctor.go otherwise has to
// duplicate as a literal.
func CurrentSchemaVersion() int { return currentSchemaVersion }

// modeForRead maps a response mode onto the mode the raw read should use.
// HistoryNone still reads nothing; everything else reads the untouched rows and
// thins them afterwards.
func modeForRead(m HistoryMode) HistoryMode {
	if m == HistoryNone {
		return HistoryNone
	}
	return HistoryFull
}
