package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/ProgenyAlpha/periscope/internal/forecast"
	"golang.org/x/term"
)

// --- Statusline Input (piped from Claude Code via stdin) ---

// costField mirrors Claude Code's authoritative per-session cost. Named
// (rather than anonymous like most fields below) so segCost and its tests
// can reference the type directly.
type costField struct {
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// rateWindow is one rate-limit window (five_hour or seven_day). Claude Code
// sends rate_limits only for Pro/Max subscribers, and only after the first
// API response of a session — used_percentage is absent/null before that,
// and again after /compact until the next API call. UsedPercentage is a
// pointer so absence is distinguishable from a genuine 0.
type rateWindow struct {
	UsedPercentage *int  `json:"used_percentage"`
	ResetsAt       int64 `json:"resets_at"`
}

type rateLimitsField struct {
	FiveHour *rateWindow `json:"five_hour"`
	SevenDay *rateWindow `json:"seven_day"`
}

// currentUsageField is the token breakdown from the most recent API
// response. Null before the first API call in a session, and again
// immediately after /compact until the next API call repopulates it.
type currentUsageField struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type StatuslineInput struct {
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name"`
	Version     string `json:"version"`
	Workspace   *struct {
		CurrentDir  string  `json:"current_dir"`
		ProjectDir  string  `json:"project_dir"`
		GitWorktree *string `json:"git_worktree"`
		Repo        *struct {
			Host  string `json:"host"`
			Owner string `json:"owner"`
			Name  string `json:"name"`
		} `json:"repo"`
	} `json:"workspace"`
	Model *struct {
		ModelID     string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"model"`
	ContextWindow *struct {
		UsedPercentage    float64            `json:"used_percentage"`
		ContextWindowSize int                `json:"context_window_size"`
		TotalInputTokens  int                `json:"total_input_tokens"`
		TotalOutputTokens int                `json:"total_output_tokens"`
		CurrentUsage      *currentUsageField `json:"current_usage"`
	} `json:"context_window"`
	VimMode *struct {
		Mode string `json:"mode"`
	} `json:"vim"`
	Effort *struct {
		Level string `json:"level"`
	} `json:"effort"`
	Cost              *costField       `json:"cost"`
	RateLimits        *rateLimitsField `json:"rate_limits"`
	FastMode          bool             `json:"fast_mode"`
	Exceeds200kTokens bool             `json:"exceeds_200k_tokens"`
	Thinking          *struct {
		Enabled bool `json:"enabled"`
	} `json:"thinking"`
	OutputStyle *struct {
		Name string `json:"name"`
	} `json:"output_style"`
	Agent *struct {
		Name string `json:"name"`
	} `json:"agent"`
	PR *struct {
		Number      int     `json:"number"`
		URL         string  `json:"url"`
		ReviewState *string `json:"review_state"`
	} `json:"pr"`
}

// --- Terminal Theme (ANSI 256-color) ---

type TerminalTheme struct {
	Bg1    int `toml:"bg1"`
	Bg2    int `toml:"bg2"`
	Fg     int `toml:"fg"`
	Dim    int `toml:"dim"`
	Blue   int `toml:"blue"`
	Purple int `toml:"purple"`
	Cyan   int `toml:"cyan"`
	Green  int `toml:"green"`
	Yellow int `toml:"yellow"`
	Red    int `toml:"red"`
	Peach  int `toml:"peach"`
}

// --- Statusline Config ---

type StatuslineConfig struct {
	Theme    string                      `json:"theme"`
	Style    string                      `json:"style"`
	Segments map[string]StatuslineSegCfg `json:"segments"`
	Options  StatuslineOptions           `json:"options"`
	Order    []string                    `json:"order,omitempty"`
}

type StatuslineSegCfg struct {
	Enabled  *bool `json:"enabled"`
	Priority int   `json:"priority"`
	Row      int   `json:"row,omitempty"` // 1=top (work), 2=bottom (rates). 0 = use default.
}

type StatuslineOptions struct {
	ContextBarWidth  int    `json:"contextBarWidth"`
	CompactThreshold int    `json:"compactThreshold"`
	DashboardURL     string `json:"dashboardUrl,omitempty"`
	MinWidth         int    `json:"minWidth"`
}

// --- Segment ---

type segment struct {
	text      string
	color     int
	bg        int
	empty     bool
	name      string
	priority  int
	barCol    int
	dimCol    int
	filledStr string
	emptyStr  string
	pct       int
}

// --- Sidecar Data (for statusline) ---

type slSidecar struct {
	Turns      int
	CachePct   int
	Tools      []string
	HasSidecar bool
	Cost       float64
	BurnRate   float64
	BurnOK     bool
	ModTime    time.Time // mtime of the sidecar file this data came from
}

// --- Default catppuccin-mocha terminal colors ---

var defaultTermTheme = TerminalTheme{
	Bg1: 235, Bg2: 237, Fg: 255, Dim: 60,
	Blue: 117, Purple: 183, Cyan: 117,
	Green: 150, Yellow: 222, Red: 210, Peach: 216,
}

// --- ANSI Helpers ---

func fg(n int) string { return fmt.Sprintf("\x1b[38;5;%dm", n) }
func bg(n int) string { return fmt.Sprintf("\x1b[48;5;%dm", n) }

const reset = "\x1b[0m"

func rateColor(pct int, theme *TerminalTheme) int {
	if pct < 50 {
		return theme.Green
	}
	if pct < 75 {
		return theme.Yellow
	}
	return theme.Red
}

// --- Theme Loading ---

func loadTerminalTheme(pluginDir, themeName string) *TerminalTheme {
	// Try loading from plugin theme file
	themePath := filepath.Join(pluginDir, "themes", themeName+".toml")
	if data, err := os.ReadFile(themePath); err == nil {
		var themeFile struct {
			Terminal TerminalTheme `toml:"terminal"`
		}
		if _, err := toml.Decode(string(data), &themeFile); err == nil {
			if themeFile.Terminal.Bg1 != 0 || themeFile.Terminal.Fg != 0 {
				return &themeFile.Terminal
			}
		}
	}

	// Hardcoded fallbacks for common themes without [terminal] sections
	builtinThemes := map[string]TerminalTheme{
		"catppuccin-mocha": defaultTermTheme,
		"dracula": {
			Bg1: 235, Bg2: 237, Fg: 255, Dim: 60,
			Blue: 117, Purple: 141, Cyan: 159,
			Green: 120, Yellow: 228, Red: 210, Peach: 212,
		},
		"tokyo-night": {
			Bg1: 235, Bg2: 237, Fg: 255, Dim: 60,
			Blue: 111, Purple: 141, Cyan: 116,
			Green: 158, Yellow: 222, Red: 210, Peach: 216,
		},
		"nord": {
			Bg1: 235, Bg2: 237, Fg: 252, Dim: 60,
			Blue: 110, Purple: 139, Cyan: 110,
			Green: 150, Yellow: 222, Red: 174, Peach: 216,
		},
		"gruvbox": {
			Bg1: 236, Bg2: 238, Fg: 223, Dim: 245,
			Blue: 109, Purple: 175, Cyan: 108,
			Green: 142, Yellow: 214, Red: 167, Peach: 208,
		},
		"tactical": {
			Bg1: 233, Bg2: 235, Fg: 252, Dim: 60,
			Blue: 75, Purple: 141, Cyan: 44,
			Green: 77, Yellow: 214, Red: 196, Peach: 215,
		},
		"arctic": {
			Bg1: 255, Bg2: 254, Fg: 235, Dim: 249,
			Blue: 33, Purple: 98, Cyan: 37,
			Green: 34, Yellow: 178, Red: 160, Peach: 208,
		},
		"ghost": {
			Bg1: 234, Bg2: 236, Fg: 252, Dim: 242,
			Blue: 110, Purple: 139, Cyan: 116,
			Green: 150, Yellow: 222, Red: 174, Peach: 216,
		},
		"midnight": {
			Bg1: 234, Bg2: 236, Fg: 252, Dim: 242,
			Blue: 69, Purple: 135, Cyan: 44,
			Green: 78, Yellow: 220, Red: 196, Peach: 209,
		},
		"phosphor": {
			Bg1: 233, Bg2: 235, Fg: 46, Dim: 239,
			Blue: 46, Purple: 46, Cyan: 46,
			Green: 46, Yellow: 226, Red: 196, Peach: 208,
		},
		"starfield-dark": {
			Bg1: 234, Bg2: 236, Fg: 252, Dim: 242,
			Blue: 75, Purple: 141, Cyan: 80,
			Green: 114, Yellow: 220, Red: 203, Peach: 215,
		},
		"starfield-light": {
			Bg1: 255, Bg2: 254, Fg: 235, Dim: 249,
			Blue: 33, Purple: 98, Cyan: 30,
			Green: 28, Yellow: 172, Red: 160, Peach: 208,
		},
		"thermal": {
			Bg1: 233, Bg2: 235, Fg: 252, Dim: 241,
			Blue: 33, Purple: 135, Cyan: 44,
			Green: 46, Yellow: 226, Red: 196, Peach: 208,
		},
	}

	if t, ok := builtinThemes[themeName]; ok {
		return &t
	}
	return &defaultTermTheme
}

// --- Data Loaders ---

func loadSidecarForStatusline(dataDir string) slSidecar {
	return loadSidecarForStatuslineFor(dataDir, "")
}

// loadSidecarForStatuslineFor reads the sidecar belonging to sessionID. Falling
// back to the most recently modified file is only correct before this session
// has written one — with concurrent sessions the newest file is usually
// somebody else's.
func loadSidecarForStatuslineFor(dataDir, sessionID string) slSidecar {
	result := slSidecar{}

	// A known session reads its own file or nothing. Falling back to the newest
	// sidecar here would show another session's turns, tools and cache rate,
	// which is what every concurrent session did before it had written one.
	if sessionID != "" {
		p := filepath.Join(dataDir, sessionID+".json")
		info, err := os.Stat(p)
		if err != nil {
			return result
		}
		return readSidecarFile(p, info.ModTime())
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return result
	}

	// Find most recently modified session sidecar
	var latest os.DirEntry
	var latestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || sidecarExclude[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if latest == nil || info.ModTime().After(latestTime) {
			latest = e
			latestTime = info.ModTime()
		}
	}

	if latest == nil {
		return result
	}

	return readSidecarFile(filepath.Join(dataDir, latest.Name()), latestTime)
}

func readSidecarFile(path string, modTime time.Time) slSidecar {
	result := slSidecar{}
	data, err := os.ReadFile(path)
	if err != nil {
		return result
	}
	data = stripBOM(data)

	var state SidecarState
	if json.Unmarshal(data, &state) != nil || state.Cumulative == nil {
		return result
	}

	c := state.Cumulative
	result.Turns = c.AgentCalls + c.ToolCalls + c.ChatCalls
	result.Cost = c.Cost
	result.HasSidecar = true
	result.ModTime = modTime

	totalIn := c.Input + c.CacheRead
	if totalIn > 0 {
		result.CachePct = int(math.Round(float64(c.CacheRead) / float64(totalIn) * 100))
	}

	if state.LastTurn != nil && len(state.LastTurn.Tools) > 0 {
		toolCounts := map[string]int{}
		for _, t := range state.LastTurn.Tools {
			toolCounts[t]++
		}
		type tc struct {
			name  string
			count int
		}
		var sorted []tc
		for k, v := range toolCounts {
			sorted = append(sorted, tc{k, v})
		}
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].count > sorted[j].count })
		for _, t := range sorted {
			if t.count > 1 {
				result.Tools = append(result.Tools, fmt.Sprintf("%sx%d", t.name, t.count))
			} else {
				result.Tools = append(result.Tools, t.name)
			}
		}
	}

	return result
}

// slScoped is one per-model weekly cap as reported by the API's limits array.
type slScoped struct {
	Model string
	Pct   int
	Reset string
}

type slRates struct {
	Pct5hr      int
	PctWeekly   int
	PctSonnet   int
	Reset5hr    string
	ResetWeekly string
	ResetSonnet string
	Scoped      []slScoped
	FetchedAt   time.Time // when the polling loop wrote this cache
}

func loadRatesForStatusline(dataDir string) slRates {
	result := slRates{Pct5hr: -1, PctWeekly: -1, PctSonnet: -1}

	cachePath := filepath.Join(dataDir, "usage-api-cache.json")
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return result
	}
	data = stripBOM(data)

	var cache map[string]any
	if json.Unmarshal(data, &cache) != nil {
		return result
	}

	result.Pct5hr = forecast.IntOrDefault(cache["pct5hr"], -1)
	result.PctWeekly = forecast.IntOrDefault(cache["pctWeekly"], -1)
	result.PctSonnet = forecast.IntOrDefault(cache["pctSonnet"], -1)
	if v, ok := cache["fetched_at"].(float64); ok {
		result.FetchedAt = time.Unix(int64(v), 0)
	}
	if v, ok := cache["reset5hr"].(string); ok {
		result.Reset5hr = v
	}
	if v, ok := cache["resetWeekly"].(string); ok {
		result.ResetWeekly = v
	}
	if v, ok := cache["resetSonnet"].(string); ok {
		result.ResetSonnet = v
	}
	if raw, ok := cache["scoped"].([]any); ok {
		for _, e := range raw {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			name, _ := m["model"].(string)
			if name == "" {
				continue
			}
			reset, _ := m["reset"].(string)
			result.Scoped = append(result.Scoped, slScoped{
				Model: name,
				Pct:   forecast.IntOrDefault(m["pct"], -1),
				Reset: reset,
			})
		}
	}

	return result
}

// stateDir resolves the cost-state directory. PERISCOPE_STATE_DIR overrides it
// so the cache-write path can be exercised without touching live state.
func stateDir() string {
	if d := os.Getenv("PERISCOPE_STATE_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "hooks", "cost-state")
}

// --- Staleness ---

// The usage cache is polled roughly every 60s; the sidecar is written once
// per turn by the Stop hook, and a single turn can legitimately run long, so
// it gets a much looser bound.
const (
	usageCacheStaleAfter = 10 * time.Minute
	sidecarStaleAfter    = 60 * time.Minute
)

// staleAt reports whether age is older than threshold. Strict: an age
// exactly equal to threshold is not yet stale.
func staleAt(age, threshold time.Duration) bool {
	return age > threshold
}

// isStale reports whether t is older than threshold. A zero t means the
// caller has no timestamp to judge by, so it's never treated as stale.
func isStale(t time.Time, threshold time.Duration) (bool, time.Duration) {
	if t.IsZero() {
		return false, 0
	}
	age := time.Since(t)
	return staleAt(age, threshold), age
}

// staleSuffix renders age as a compact tilde-prefixed suffix (~12m, ~3h,
// ~5d). Empty if age rounds below one minute.
func staleSuffix(age time.Duration) string {
	mins := math.Round(age.Minutes())
	if mins < 1 {
		return ""
	}
	if mins < 60 {
		return fmt.Sprintf("~%dm", int64(mins))
	}
	hrs := math.Round(age.Hours())
	if hrs < 24 {
		return fmt.Sprintf("~%dh", int64(hrs))
	}
	return fmt.Sprintf("~%dd", int64(math.Round(age.Hours()/24)))
}

// markStale dims seg and appends an age suffix when fetchedAt is older than
// threshold. A segment that is empty stays empty — staleness never creates
// output.
func markStale(seg segment, fetchedAt time.Time, threshold time.Duration, theme *TerminalTheme) segment {
	if seg.empty {
		return seg
	}
	stale, age := isStale(fetchedAt, threshold)
	if !stale {
		return seg
	}
	seg.color = theme.Dim
	seg.text += staleSuffix(age)
	return seg
}

// --- Segment Functions ---

func segDir(input *StatuslineInput, theme *TerminalTheme) segment {
	dir := ""
	if input.Workspace != nil {
		dir = input.Workspace.CurrentDir
	}
	if dir == "" {
		return segment{empty: true}
	}
	// Abbreviate home directory
	home, _ := os.UserHomeDir()
	if home != "" && strings.HasPrefix(dir, home) {
		dir = "~" + dir[len(home):]
	}
	return segment{text: " \uf07b " + dir, color: theme.Blue}
}

func segGit(input *StatuslineInput, theme *TerminalTheme) segment {
	if input.Workspace == nil || input.Workspace.CurrentDir == "" {
		return segment{empty: true}
	}

	dir := input.Workspace.CurrentDir

	// Try to get git branch — fall back to project_dir if cwd isn't a repo
	branch := gitBranch(dir)
	if branch == "" && input.Workspace.ProjectDir != "" && input.Workspace.ProjectDir != dir {
		dir = input.Workspace.ProjectDir
		branch = gitBranch(dir)
	}
	if branch == "" {
		return segment{empty: true}
	}

	dirty := gitDirty(dir)
	if dirty {
		branch += "*"
	}
	return segment{text: " \ue0a0 " + branch, color: theme.Purple}
}

func gitBranch(dir string) string {
	// Read .git/HEAD for speed instead of shelling out
	gitDir := filepath.Join(dir, ".git")
	headPath := filepath.Join(gitDir, "HEAD")
	data, err := os.ReadFile(headPath)
	if err != nil {
		// Walk up to find .git
		for d := dir; ; {
			parent := filepath.Dir(d)
			if parent == d {
				return ""
			}
			headPath = filepath.Join(parent, ".git", "HEAD")
			data, err = os.ReadFile(headPath)
			if err == nil {
				break
			}
			d = parent
		}
	}
	head := strings.TrimSpace(string(data))
	if strings.HasPrefix(head, "ref: refs/heads/") {
		return strings.TrimPrefix(head, "ref: refs/heads/")
	}
	if len(head) >= 8 {
		return head[:8] // Detached HEAD
	}
	return ""
}

func gitDirty(dir string) bool {
	// Quick check: does the index differ from HEAD?
	// For speed, check if .git/index was modified recently vs .git/COMMIT_EDITMSG
	// This is a heuristic — not 100% accurate but fast
	gitDir := findGitDir(dir)
	if gitDir == "" {
		return false
	}
	indexInfo, err := os.Stat(filepath.Join(gitDir, "index"))
	if err != nil {
		return false
	}
	commitInfo, _ := os.Stat(filepath.Join(gitDir, "COMMIT_EDITMSG"))
	if commitInfo == nil {
		return false
	}
	return indexInfo.ModTime().After(commitInfo.ModTime())
}

func findGitDir(dir string) string {
	for d := dir; ; {
		gd := filepath.Join(d, ".git")
		if info, err := os.Stat(gd); err == nil && info.IsDir() {
			return gd
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

func segModel(input *StatuslineInput, theme *TerminalTheme) segment {
	if input.Model == nil || (input.Model.ModelID == "" && input.Model.DisplayName == "") {
		return segment{empty: true}
	}
	// Prefer display_name (e.g. "Opus 4.6") — has version info
	model := input.Model.DisplayName
	if model == "" {
		model = input.Model.ModelID
	}
	return segment{text: " \U000f09a1 " + model, color: theme.Cyan}
}

func segTurns(sc slSidecar, theme *TerminalTheme) segment {
	if sc.Turns <= 0 {
		return segment{empty: true}
	}
	seg := segment{text: fmt.Sprintf(" \uf021 t:%d", sc.Turns), color: theme.Cyan}
	return markStale(seg, sc.ModTime, sidecarStaleAfter, theme)
}

func segRate5hr(rates slRates, theme *TerminalTheme) segment {
	if rates.Pct5hr < 0 {
		return segment{empty: true}
	}
	col := rateColor(rates.Pct5hr, theme)
	seg := segment{text: fmt.Sprintf(" 5h:%d%%", rates.Pct5hr), color: col}
	return markStale(seg, rates.FetchedAt, usageCacheStaleAfter, theme)
}

func segRateWeekly(rates slRates, theme *TerminalTheme) segment {
	if rates.PctWeekly < 0 {
		return segment{empty: true}
	}
	col := rateColor(rates.PctWeekly, theme)
	seg := segment{text: fmt.Sprintf(" wk:%d%%", rates.PctWeekly), color: col}
	return markStale(seg, rates.FetchedAt, usageCacheStaleAfter, theme)
}

var scopedAbbrev = map[string]string{
	"fable":  "fb",
	"sonnet": "sn",
	"opus":   "op",
	"haiku":  "hk",
}

func scopedLabel(model string) string {
	m := strings.ToLower(model)
	if a, ok := scopedAbbrev[m]; ok {
		return a
	}
	if len(m) > 2 {
		return m[:2]
	}
	return m
}

// segRateScoped renders every per-model weekly cap the API reports, e.g.
// " fb:10%". Colored by the highest of them.
func segRateScoped(rates slRates, theme *TerminalTheme) segment {
	var parts []string
	worst := -1
	for _, s := range rates.Scoped {
		if s.Pct < 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s:%d%%", scopedLabel(s.Model), s.Pct))
		if s.Pct > worst {
			worst = s.Pct
		}
	}
	if len(parts) == 0 {
		return segment{empty: true}
	}
	seg := segment{text: " " + strings.Join(parts, " "), color: rateColor(worst, theme)}
	return markStale(seg, rates.FetchedAt, usageCacheStaleAfter, theme)
}

func segCost(input *StatuslineInput, sc slSidecar, theme *TerminalTheme) segment {
	cost := sc.Cost
	fromSidecar := true
	if input != nil && input.Cost != nil && input.Cost.TotalCostUSD > 0 {
		cost = input.Cost.TotalCostUSD
		fromSidecar = false
	}
	if cost <= 0 {
		return segment{empty: true}
	}
	val := math.Round(cost*100) / 100
	seg := segment{text: fmt.Sprintf(" $%.2f", val), color: theme.Yellow}
	if fromSidecar {
		seg = markStale(seg, sc.ModTime, sidecarStaleAfter, theme)
	}
	return seg
}

func segBurn(sc slSidecar, theme *TerminalTheme) segment {
	if !sc.BurnOK || sc.BurnRate <= 0 {
		return segment{empty: true}
	}
	return segment{text: fmt.Sprintf(" $%.2f/hr", sc.BurnRate), color: theme.Yellow}
}

func segReset(rates slRates, theme *TerminalTheme) segment {
	now := time.Now().UTC()
	var nearest float64
	candidates := []string{rates.Reset5hr, rates.ResetWeekly, rates.ResetSonnet}
	for _, s := range rates.Scoped {
		candidates = append(candidates, s.Reset)
	}
	for _, r := range candidates {
		if r == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, r)
		if err != nil {
			continue
		}
		diff := t.Sub(now).Minutes()
		if diff > 0 && (nearest == 0 || diff < nearest) {
			nearest = diff
		}
	}
	if nearest == 0 {
		return segment{empty: true}
	}
	hrs := int(nearest / 60)
	mins := int(math.Round(math.Mod(nearest, 60)))
	display := ""
	if hrs > 0 {
		display = fmt.Sprintf("%dh%dm", hrs, mins)
	} else {
		display = fmt.Sprintf("%dm", mins)
	}
	seg := segment{text: " rst:" + display, color: theme.Cyan}
	return markStale(seg, rates.FetchedAt, usageCacheStaleAfter, theme)
}

func segProj(rates slRates, dataDir string, theme *TerminalTheme) segment {
	if rates.Pct5hr < 0 || rates.Reset5hr == "" {
		return segment{empty: true}
	}
	proj, _, ok := forecast.Project5hr(dataDir, rates.Pct5hr, rates.Reset5hr)
	if !ok {
		return segment{empty: true}
	}
	col := theme.Green
	if proj >= 80 {
		col = theme.Red
	} else if proj >= 50 {
		col = theme.Yellow
	}
	seg := segment{text: fmt.Sprintf(" pj:%d%%", proj), color: col}
	return markStale(seg, rates.FetchedAt, usageCacheStaleAfter, theme)
}

func segCache(input *StatuslineInput, sc slSidecar, theme *TerminalTheme) segment {
	if input != nil && input.ContextWindow != nil && input.ContextWindow.CurrentUsage != nil {
		cu := input.ContextWindow.CurrentUsage
		total := cu.InputTokens + cu.CacheReadInputTokens
		pct := 0
		if total > 0 {
			pct = int(math.Round(float64(cu.CacheReadInputTokens) / float64(total) * 100))
		}
		return segment{text: fmt.Sprintf(" \uf0e7%d%%", pct), color: theme.Green}
	}
	if !sc.HasSidecar {
		return segment{empty: true}
	}
	seg := segment{text: fmt.Sprintf(" \uf0e7%d%%", sc.CachePct), color: theme.Green}
	return markStale(seg, sc.ModTime, sidecarStaleAfter, theme)
}

func segTools(sc slSidecar, theme *TerminalTheme) segment {
	if len(sc.Tools) == 0 {
		return segment{empty: true}
	}
	list := strings.Join(sc.Tools, " ")
	seg := segment{text: fmt.Sprintf(" [%s]", list), color: theme.Peach}
	return markStale(seg, sc.ModTime, sidecarStaleAfter, theme)
}

func segFast(input *StatuslineInput, theme *TerminalTheme) segment {
	if input == nil || !input.FastMode {
		return segment{empty: true}
	}
	return segment{text: " fast", color: theme.Peach}
}

func segContext(input *StatuslineInput, opts StatuslineOptions, theme *TerminalTheme) segment {
	ctxPct := 0
	if input.ContextWindow != nil {
		ctxPct = int(math.Round(input.ContextWindow.UsedPercentage))
	}
	barW := opts.ContextBarWidth
	if barW <= 0 {
		barW = 15
	}
	filled := barW * ctxPct / 100
	if filled > barW {
		filled = barW
	}
	emptyW := barW - filled
	barCol := rateColor(ctxPct, theme)
	filledStr := strings.Repeat("\u2588", filled)
	emptyBarStr := strings.Repeat("\u2591", emptyW)

	return segment{
		text:      fmt.Sprintf(" ctx:%s%s %d%%", filledStr, emptyBarStr, ctxPct),
		color:     barCol,
		barCol:    barCol,
		dimCol:    theme.Dim,
		filledStr: filledStr,
		emptyStr:  emptyBarStr,
		pct:       ctxPct,
	}
}

// segDash renders a clickable link to the dashboard. The label is deliberately
// tiny — the value is the click target, not the text — and an unknown URL
// yields no segment rather than a link that goes nowhere.
func segDash(url string, theme *TerminalTheme) segment {
	if url == "" {
		return segment{empty: true}
	}
	return segment{
		text:  osc8(url, " ↗ dashboard"),
		color: theme.Blue,
	}
}

func segVim(input *StatuslineInput, theme *TerminalTheme) segment {
	if input.VimMode == nil || input.VimMode.Mode == "" {
		return segment{empty: true}
	}
	modeText := strings.ToUpper(input.VimMode.Mode)
	col := theme.Yellow
	if modeText == "INSERT" {
		col = theme.Green
	}
	return segment{text: " " + modeText, color: col}
}

// --- Segment Dispatcher ---

// defaultPriority ranks segments for width-based truncation: lower survives
// longer. Without this every segment shared one priority and the truncation
// loop, which scans for a strictly greater value from index 0, always dropped
// the leftmost segment first regardless of its worth.
func defaultPriority(name string) int {
	switch name {
	case "dir", "model", "rate-5hr", "rate-weekly":
		return 2
	case "git", "cost", "context":
		return 3
	case "effort", "fast", "rate-scoped":
		return 4
	case "reset":
		return 5
	case "turns", "burn", "vim", "cache", "proj":
		return 6
	case "tools":
		return 7
	}
	return 5
}

func getSegment(name string, input *StatuslineInput, sc slSidecar, rates slRates, dataDir string, opts StatuslineOptions, theme *TerminalTheme) segment {
	switch name {
	case "dir":
		return segDir(input, theme)
	case "git":
		return segGit(input, theme)
	case "model":
		return segModel(input, theme)
	case "effort":
		return segEffort(input, theme)
	case "turns":
		return segTurns(sc, theme)
	case "rate-5hr":
		return segRate5hr(rates, theme)
	case "rate-weekly":
		return segRateWeekly(rates, theme)
	case "rate-scoped":
		return segRateScoped(rates, theme)
	case "cost":
		return segCost(input, sc, theme)
	case "reset":
		return segReset(rates, theme)
	case "proj":
		return segProj(rates, dataDir, theme)
	case "cache":
		return segCache(input, sc, theme)
	case "tools":
		return segTools(sc, theme)
	case "context":
		return segContext(input, opts, theme)
	case "dash":
		return segDash(opts.DashboardURL, theme)
	case "vim":
		return segVim(input, theme)
	case "burn":
		return segBurn(sc, theme)
	case "fast":
		return segFast(input, theme)
	default:
		return segment{empty: true}
	}
}

// segEffort renders the live effort level passed by Claude Code in the
// statusline stdin payload. Color-coded so xhigh stands out — that's the
// expensive setting and you usually want to know when you're on it.
//
// Also flags drift between the in-flight slider value and the persisted
// default in ~/.claude/settings.json (issue #30726: slider doesn't write
// settings.json). When they disagree, render `eff:xhigh!medium` so the
// statusline shows what's actually being applied AND the ignored default.
func segEffort(input *StatuslineInput, theme *TerminalTheme) segment {
	if input == nil || input.Effort == nil || input.Effort.Level == "" {
		return segment{empty: true}
	}
	level := strings.ToLower(input.Effort.Level)
	col := theme.Cyan
	switch level {
	case "low":
		col = theme.Green
	case "medium":
		col = theme.Cyan
	case "high":
		col = theme.Yellow
	case "xhigh":
		col = theme.Peach
	case "max":
		col = theme.Red
	}

	// Compare against the persisted default; show drift if they differ.
	settingsLevel := readSettingsEffort()
	text := " eff:" + level
	if settingsLevel != "" && !strings.EqualFold(settingsLevel, level) {
		text = " eff:" + level + "!" + strings.ToLower(settingsLevel)
		col = theme.Peach
	}
	return segment{text: text, color: col}
}

// readSettingsEffort returns the persisted effortLevel from
// ~/.claude/settings.json, or "" if missing.
func readSettingsEffort() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		return ""
	}
	var s struct {
		EffortLevel string `json:"effortLevel"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s.EffortLevel
}

// --- Renderers ---

func renderPowerline(segs []segment, theme *TerminalTheme) string {
	var out strings.Builder
	sep := "\ue0b0" // Powerline arrow
	for i, seg := range segs {
		out.WriteString(bg(seg.bg))
		out.WriteString(fg(seg.color))
		out.WriteString(seg.text)
		out.WriteString(" ")
		out.WriteString(reset)
		if i < len(segs)-1 {
			out.WriteString(fg(seg.bg))
			out.WriteString(bg(segs[i+1].bg))
			out.WriteString(sep)
			out.WriteString(reset)
		} else {
			out.WriteString(fg(seg.bg))
			out.WriteString(sep)
			out.WriteString(reset)
		}
	}
	return out.String()
}

func renderPlain(segs []segment, theme *TerminalTheme) string {
	var parts []string
	for _, seg := range segs {
		if seg.filledStr != "" {
			parts = append(parts, fmt.Sprintf("%s%s%s%s%s %s%d%%%s",
				fg(seg.barCol), seg.filledStr, fg(seg.dimCol), seg.emptyStr, reset,
				fg(seg.barCol), seg.pct, reset))
		} else {
			parts = append(parts, fmt.Sprintf("%s%s%s", fg(seg.color), seg.text, reset))
		}
	}
	pipeSep := fmt.Sprintf(" %s|%s ", fg(theme.Dim), reset)
	return strings.Join(parts, pipeSep)
}

func renderMinimal(segs []segment, theme *TerminalTheme) string {
	var parts []string
	for _, seg := range segs {
		if seg.filledStr != "" {
			parts = append(parts, fmt.Sprintf("%s%d%%%s", fg(seg.barCol), seg.pct, reset))
		} else {
			parts = append(parts, fmt.Sprintf("%s%s%s", fg(seg.color), seg.text, reset))
		}
	}
	return strings.Join(parts, " ")
}

// --- Terminal Width Detection ---

var ansiEscRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// OSC 8 hyperlinks: ESC ] 8 ; params ; URI ST, where ST is BEL or ESC \.
// These render as zero glyphs, so they must be stripped before measuring
// width — otherwise a link's URL counts as visible columns and the
// truncation loop drops segments that would have fit.
var osc8Re = regexp.MustCompile(`\x1b\]8;[^;]*;[^\x07\x1b]*(?:\x07|\x1b\\)`)

// osc8 wraps label in a terminal hyperlink pointing at uri. Terminals that
// don't support OSC 8 ignore the sequence and print the label unchanged, so
// this is safe to emit unconditionally.
func osc8(uri, label string) string {
	if uri == "" {
		return label
	}
	return "\x1b]8;;" + uri + "\x1b\\" + label + "\x1b]8;;\x1b\\"
}

func getTerminalWidth() int {
	if cols := os.Getenv("COLUMNS"); cols != "" {
		if n, err := strconv.Atoi(cols); err == nil && n > 0 {
			return n
		}
	}
	// Try stderr (stays connected to terminal even when stdin/stdout are piped)
	if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && w > 0 {
		return w
	}
	// Try stdout
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	return 120 // sensible default when detection fails
}

func visibleLen(s string) int {
	stripped := osc8Re.ReplaceAllString(s, "")
	stripped = ansiEscRe.ReplaceAllString(stripped, "")
	// Count runes (display characters), not bytes — Unicode icons/bars are multi-byte
	return len([]rune(stripped))
}

// --- Main Statusline Command ---

func cmdStatusline() {
	// Read JSON input from stdin
	stdinData, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}

	var input StatuslineInput
	if len(stdinData) > 0 {
		json.Unmarshal(stdinData, &input)
	}

	// Persist live effort + rate-limit data captured from the Claude Code
	// statusline payload. Lets the Stop hook read the *current* effort level
	// (settings.json gets cleared by the slider per issue #30726) and lets
	// the polling loop opportunistically use Anthropic's authoritative
	// rate-limit numbers without burning a /v1/messages ping.
	if input.SessionID != "" && input.Effort != nil && input.Effort.Level != "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir := filepath.Join(home, ".periscope", "effort")
			os.MkdirAll(dir, 0755)
			payload, _ := json.Marshal(map[string]any{
				"sessionId": input.SessionID,
				"level":     input.Effort.Level,
				"updatedAt": time.Now().UTC().Format(time.RFC3339),
			})
			writeFileAtomic(filepath.Join(dir, input.SessionID+".json"), payload, 0644)
		}
	}
	// Write native rate-limit data from Claude Code into the polling cache,
	// merging so we don't clobber sonnet/opus/extra_usage keys. The server's
	// polling loop owns the same file, so the merge is atomic and a no-op
	// write is skipped entirely.
	dataDir := stateDir()
	writeRateLimitHint(dataDir, input.SessionID, input.RateLimits)

	home, _ := os.UserHomeDir()
	periscopeDir := filepath.Join(home, ".periscope")
	pluginDir := filepath.Join(periscopeDir, "plugins")
	claudeDir := filepath.Join(home, ".claude")

	// Load config
	cfg := StatuslineConfig{
		Theme: "catppuccin-mocha",
		Style: "powerline",
		Options: StatuslineOptions{
			ContextBarWidth:  15,
			CompactThreshold: 100,
			MinWidth:         60,
		},
	}

	// Try periscope config location first, fall back to claude statusline config
	cfgPaths := []string{
		filepath.Join(periscopeDir, "statusline-config.json"),
		filepath.Join(claudeDir, "statusline", "statusline-config.json"),
	}
	for _, cfgPath := range cfgPaths {
		if data, err := os.ReadFile(cfgPath); err == nil {
			data = stripBOM(data)
			json.Unmarshal(data, &cfg)
			break
		}
	}
	if cfg.Options.ContextBarWidth == 0 {
		cfg.Options.ContextBarWidth = 15
	}
	if cfg.Options.CompactThreshold == 0 {
		cfg.Options.CompactThreshold = 100
	}
	if cfg.Options.MinWidth == 0 {
		cfg.Options.MinWidth = 60
	}
	if cfg.Options.DashboardURL == "" {
		cfg.Options.DashboardURL = resolveDashboardURL(periscopeDir)
	}

	// Load theme
	theme := loadTerminalTheme(pluginDir, cfg.Theme)

	// Load data
	sidecar := loadSidecarForStatuslineFor(dataDir, input.SessionID)
	sidecar.BurnRate, sidecar.BurnOK = forecast.LocalBurnRate(dataDir, time.Hour)
	rates := loadRatesForStatusline(dataDir)

	// Default row assignments: 1=top (work), 2=bottom (rates)
	defaultRow := map[string]int{
		"dir": 1, "git": 1, "model": 1, "effort": 1, "fast": 1, "turns": 1, "cost": 1, "burn": 1, "tools": 1, "vim": 1,
		"rate-5hr": 2, "rate-weekly": 2, "rate-scoped": 2, "reset": 2, "proj": 2, "cache": 2, "context": 2, "dash": 2,
	}

	// Segment order — use config order if set, else default
	defaultOrder := []string{"dir", "git", "model", "effort", "fast", "turns", "cost", "burn", "tools", "vim",
		"rate-5hr", "rate-weekly", "rate-scoped", "reset", "proj", "cache", "context", "dash"}
	segOrder := defaultOrder
	if len(cfg.Order) > 0 {
		seen := map[string]bool{}
		for _, n := range cfg.Order {
			seen[n] = true
		}
		segOrder = append([]string{}, cfg.Order...)
		for _, n := range defaultOrder {
			if !seen[n] {
				segOrder = append(segOrder, n)
			}
		}
	}

	// Terminal width
	termWidth := getTerminalWidth()

	// Resolve row for a segment
	rowFor := func(name string) int {
		if sc, ok := cfg.Segments[name]; ok && sc.Row > 0 {
			return sc.Row
		}
		if r, ok := defaultRow[name]; ok {
			return r
		}
		return 1
	}

	// Build segments split by row
	row1 := []segment{}
	row2 := []segment{}
	bgToggle1 := false
	bgToggle2 := false

	for _, name := range segOrder {
		enabled := true
		priority := defaultPriority(name)
		if sc, ok := cfg.Segments[name]; ok {
			if sc.Enabled != nil {
				enabled = *sc.Enabled
			}
			if sc.Priority > 0 {
				priority = sc.Priority
			}
		}
		if !enabled {
			continue
		}

		// Priority filtering by width
		if termWidth < cfg.Options.MinWidth && priority > 3 {
			continue
		}
		if termWidth < cfg.Options.CompactThreshold && priority > 6 {
			continue
		}

		seg := getSegment(name, &input, sidecar, rates, dataDir, cfg.Options, theme)
		if seg.empty {
			continue
		}
		seg.priority = priority
		seg.name = name

		row := rowFor(name)
		if row == 2 {
			if bgToggle2 {
				seg.bg = theme.Bg2
			} else {
				seg.bg = theme.Bg1
			}
			row2 = append(row2, seg)
			bgToggle2 = !bgToggle2
		} else {
			if bgToggle1 {
				seg.bg = theme.Bg2
			} else {
				seg.bg = theme.Bg1
			}
			row1 = append(row1, seg)
			bgToggle1 = !bgToggle1
		}
	}

	// Renderer
	renderWith := func(segs []segment) string {
		switch cfg.Style {
		case "plain":
			return renderPlain(segs, theme)
		case "minimal":
			return renderMinimal(segs, theme)
		default:
			return renderPowerline(segs, theme)
		}
	}

	// Progressive truncation per row
	truncateRow := func(segs []segment) ([]segment, string) {
		output := renderWith(segs)
		for visibleLen(output) > termWidth && len(segs) > 1 {
			worst := 0
			for i := 1; i < len(segs); i++ {
				if segs[i].priority > segs[worst].priority {
					worst = i
				}
			}
			segs = append(segs[:worst], segs[worst+1:]...)
			toggle := false
			for i := range segs {
				if toggle {
					segs[i].bg = theme.Bg2
				} else {
					segs[i].bg = theme.Bg1
				}
				toggle = !toggle
			}
			output = renderWith(segs)
		}
		return segs, output
	}

	_, line1 := truncateRow(row1)
	_, line2 := truncateRow(row2)

	// Output: two lines if both have content, else single
	if len(row1) > 0 && len(row2) > 0 {
		fmt.Print(line1 + "\n" + line2)
	} else if len(row1) > 0 {
		fmt.Print(line1)
	} else {
		fmt.Print(line2)
	}
}

// resolveDashboardURL derives the dashboard address from the server config, so
// the statusline link follows a host/port change without separate wiring.
// Returns "" when no config exists — better no link than one that 404s.
func resolveDashboardURL(periscopeDir string) string {
	data, err := os.ReadFile(filepath.Join(periscopeDir, "config.toml"))
	if err != nil {
		return ""
	}
	var cfg AppConfig
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return ""
	}
	port := cfg.Server.Port
	if port == 0 {
		port = 8384
	}
	return fmt.Sprintf("http://%s:%d", probeHost(cfg.Server.Host), port)
}
