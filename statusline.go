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

type StatuslineInput struct {
	Workspace *struct {
		CurrentDir string `json:"current_dir"`
		ProjectDir string `json:"project_dir"`
	} `json:"workspace"`
	Model *struct {
		ModelID     string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"model"`
	ContextWindow *struct {
		UsedPercentage float64 `json:"used_percentage"`
	} `json:"context_window"`
	VimMode *struct {
		Mode string `json:"mode"`
	} `json:"vim_mode"`
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
	ContextBarWidth  int `json:"contextBarWidth"`
	CompactThreshold int `json:"compactThreshold"`
	MinWidth         int `json:"minWidth"`
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
	result := slSidecar{}

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

	data, err := os.ReadFile(filepath.Join(dataDir, latest.Name()))
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

type slRates struct {
	Pct5hr      int
	PctWeekly   int
	PctSonnet   int
	Reset5hr    string
	ResetWeekly string
	ResetSonnet string
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
	if v, ok := cache["reset5hr"].(string); ok {
		result.Reset5hr = v
	}
	if v, ok := cache["resetWeekly"].(string); ok {
		result.ResetWeekly = v
	}
	if v, ok := cache["resetSonnet"].(string); ok {
		result.ResetSonnet = v
	}

	return result
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
	return segment{text: fmt.Sprintf(" \uf021 t:%d", sc.Turns), color: theme.Cyan}
}

func segRate5hr(rates slRates, theme *TerminalTheme) segment {
	if rates.Pct5hr < 0 {
		return segment{empty: true}
	}
	col := rateColor(rates.Pct5hr, theme)
	return segment{text: fmt.Sprintf(" 5h:%d%%", rates.Pct5hr), color: col}
}

func segRateWeekly(rates slRates, theme *TerminalTheme) segment {
	if rates.PctWeekly < 0 {
		return segment{empty: true}
	}
	col := rateColor(rates.PctWeekly, theme)
	return segment{text: fmt.Sprintf(" wk:%d%%", rates.PctWeekly), color: col}
}

func segRateSonnet(rates slRates, theme *TerminalTheme) segment {
	if rates.PctSonnet < 0 {
		return segment{empty: true}
	}
	col := rateColor(rates.PctSonnet, theme)
	return segment{text: fmt.Sprintf(" sn:%d%%", rates.PctSonnet), color: col}
}

func segCost(sc slSidecar, theme *TerminalTheme) segment {
	if sc.Cost <= 0 {
		return segment{empty: true}
	}
	val := math.Round(sc.Cost*100) / 100
	return segment{text: fmt.Sprintf(" $%.2f", val), color: theme.Yellow}
}

func segReset(rates slRates, theme *TerminalTheme) segment {
	now := time.Now().UTC()
	var nearest float64
	for _, r := range []string{rates.Reset5hr, rates.ResetWeekly, rates.ResetSonnet} {
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
	return segment{text: " rst:" + display, color: theme.Cyan}
}

func segProj(rates slRates, theme *TerminalTheme) segment {
	if rates.Pct5hr < 0 || rates.Reset5hr == "" {
		return segment{empty: true}
	}
	now := time.Now().UTC()
	resetDt, err := time.Parse(time.RFC3339, rates.Reset5hr)
	if err != nil {
		return segment{empty: true}
	}
	windowStart := resetDt.Add(-5 * time.Hour)
	elapsed := now.Sub(windowStart).Hours()
	if elapsed <= 0.05 {
		return segment{empty: true}
	}
	remaining := resetDt.Sub(now).Hours()
	if remaining <= 0 {
		return segment{empty: true}
	}
	rate := float64(rates.Pct5hr) / elapsed
	projected := int(math.Round(float64(rates.Pct5hr) + rate*remaining))
	col := theme.Green
	if projected >= 80 {
		col = theme.Red
	} else if projected >= 50 {
		col = theme.Yellow
	}
	return segment{text: fmt.Sprintf(" pj:%d%%", projected), color: col}
}

func segCache(sc slSidecar, theme *TerminalTheme) segment {
	if !sc.HasSidecar {
		return segment{empty: true}
	}
	return segment{text: fmt.Sprintf(" \uf0e7%d%%", sc.CachePct), color: theme.Green}
}

func segTools(sc slSidecar, theme *TerminalTheme) segment {
	if len(sc.Tools) == 0 {
		return segment{empty: true}
	}
	list := strings.Join(sc.Tools, " ")
	return segment{text: fmt.Sprintf(" [%s]", list), color: theme.Peach}
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

func getSegment(name string, input *StatuslineInput, sc slSidecar, rates slRates, opts StatuslineOptions, theme *TerminalTheme) segment {
	switch name {
	case "dir":
		return segDir(input, theme)
	case "git":
		return segGit(input, theme)
	case "model":
		return segModel(input, theme)
	case "turns":
		return segTurns(sc, theme)
	case "rate-5hr":
		return segRate5hr(rates, theme)
	case "rate-weekly":
		return segRateWeekly(rates, theme)
	case "rate-sonnet":
		return segRateSonnet(rates, theme)
	case "cost":
		return segCost(sc, theme)
	case "reset":
		return segReset(rates, theme)
	case "proj":
		return segProj(rates, theme)
	case "cache":
		return segCache(sc, theme)
	case "tools":
		return segTools(sc, theme)
	case "context":
		return segContext(input, opts, theme)
	case "vim":
		return segVim(input, theme)
	default:
		return segment{empty: true}
	}
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
	stripped := ansiEscRe.ReplaceAllString(s, "")
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

	home, _ := os.UserHomeDir()
	periscopeDir := filepath.Join(home, ".periscope")
	pluginDir := filepath.Join(periscopeDir, "plugins")
	dataDir := filepath.Join(home, ".claude", "hooks", "cost-state")
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

	// Load theme
	theme := loadTerminalTheme(pluginDir, cfg.Theme)

	// Load data
	sidecar := loadSidecarForStatusline(dataDir)
	rates := loadRatesForStatusline(dataDir)

	// Default row assignments: 1=top (work), 2=bottom (rates)
	defaultRow := map[string]int{
		"dir": 1, "git": 1, "model": 1, "turns": 1, "cost": 1, "tools": 1,
		"rate-5hr": 2, "rate-weekly": 2, "rate-sonnet": 2, "reset": 2, "proj": 2, "cache": 2, "context": 2,
	}

	// Segment order — use config order if set, else default
	defaultOrder := []string{"dir", "git", "model", "turns", "cost", "tools",
		"rate-5hr", "rate-weekly", "rate-sonnet", "reset", "proj", "cache", "context"}
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
		priority := 5
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

		seg := getSegment(name, &input, sidecar, rates, cfg.Options, theme)
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
