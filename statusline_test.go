package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func intPtr(n int) *int { return &n }

// --- Task 2: segCost prefers input.Cost.TotalCostUSD (P0-4) ---

func TestSegCostPrefersInputCostOverSidecar(t *testing.T) {
	theme := &defaultTermTheme
	sc := slSidecar{Cost: 1.23}

	input := &StatuslineInput{Cost: &costField{TotalCostUSD: 5.99}}
	seg := segCost(input, sc, theme)
	if seg.empty {
		t.Fatal("segCost returned empty with a valid cost present")
	}
	if seg.text != " $5.99" {
		t.Errorf("segCost text = %q, want %q (should prefer input.Cost)", seg.text, " $5.99")
	}

	// Falls back to the sidecar when input.Cost is absent.
	seg = segCost(&StatuslineInput{}, sc, theme)
	if seg.text != " $1.23" {
		t.Errorf("segCost fallback text = %q, want %q", seg.text, " $1.23")
	}

	// Falls back to the sidecar when input.Cost is present but zero.
	seg = segCost(&StatuslineInput{Cost: &costField{TotalCostUSD: 0}}, sc, theme)
	if seg.text != " $1.23" {
		t.Errorf("segCost with zero input cost = %q, want sidecar fallback %q", seg.text, " $1.23")
	}
}

// --- Task 3: rate-sonnet removal (P2-12) ---

func TestSegRateScopedStillRendersFable(t *testing.T) {
	theme := &defaultTermTheme
	rates := slRates{Scoped: []slScoped{{Model: "fable", Pct: 10, Reset: "2026-08-15T00:00:00Z"}}}
	seg := segRateScoped(rates, theme)
	if seg.empty {
		t.Fatal("segRateScoped returned empty with a scoped entry present")
	}
	if seg.text != " fb:10%" {
		t.Errorf("segRateScoped text = %q, want %q", seg.text, " fb:10%")
	}
}

func TestGetSegmentRateSonnetDoesNotCrash(t *testing.T) {
	theme := &defaultTermTheme
	input := &StatuslineInput{}
	sc := slSidecar{}
	rates := slRates{PctSonnet: 22} // a stale non-negative value must still render nothing
	opts := StatuslineOptions{}

	seg := getSegment("rate-sonnet", input, sc, rates, t.TempDir(), opts, theme)
	if !seg.empty {
		t.Errorf("getSegment(%q) = %+v, want empty segment (dead segment, falls to default case)", "rate-sonnet", seg)
	}
}

// --- Task 4: segCache from context_window.current_usage, segFast ---

func TestSegCacheUsesCurrentUsageWhenPresent(t *testing.T) {
	theme := &defaultTermTheme
	input := &StatuslineInput{}
	input.ContextWindow = &struct {
		UsedPercentage    float64            `json:"used_percentage"`
		ContextWindowSize int                `json:"context_window_size"`
		TotalInputTokens  int                `json:"total_input_tokens"`
		TotalOutputTokens int                `json:"total_output_tokens"`
		CurrentUsage      *currentUsageField `json:"current_usage"`
	}{
		CurrentUsage: &currentUsageField{InputTokens: 2, CacheReadInputTokens: 98},
	}

	seg := segCache(input, slSidecar{HasSidecar: true, CachePct: 50}, theme)
	if seg.empty {
		t.Fatal("segCache returned empty with current_usage present")
	}
	if seg.text != " 98%" {
		t.Errorf("segCache text = %q, want %q (exact rate from current_usage, not the sidecar's 50%%)", seg.text, " 98%")
	}
}

func TestSegCacheFallsBackToSidecarWhenCurrentUsageNil(t *testing.T) {
	theme := &defaultTermTheme
	seg := segCache(&StatuslineInput{}, slSidecar{HasSidecar: true, CachePct: 33}, theme)
	if seg.text != " 33%" {
		t.Errorf("segCache fallback text = %q, want %q", seg.text, " 33%")
	}

	seg = segCache(&StatuslineInput{}, slSidecar{HasSidecar: false}, theme)
	if !seg.empty {
		t.Errorf("segCache = %+v, want empty with no sidecar and no current_usage", seg)
	}
}

func TestSegFast(t *testing.T) {
	theme := &defaultTermTheme

	seg := segFast(&StatuslineInput{FastMode: true}, theme)
	if seg.empty || seg.text != " fast" {
		t.Errorf("segFast(fast_mode=true) = %+v, want text %q", seg, " fast")
	}

	seg = segFast(&StatuslineInput{FastMode: false}, theme)
	if !seg.empty {
		t.Errorf("segFast(fast_mode=false) = %+v, want empty", seg)
	}
}

// --- Task 5: staleness guard ---

func TestStaleSuffixFormatting(t *testing.T) {
	cases := []struct {
		age  time.Duration
		want string
	}{
		{5 * time.Second, ""},
		{12 * time.Minute, "~12m"},
		{3 * time.Hour, "~3h"},
		{5 * 24 * time.Hour, "~5d"},
	}
	for _, c := range cases {
		if got := staleSuffix(c.age); got != c.want {
			t.Errorf("staleSuffix(%v) = %q, want %q", c.age, got, c.want)
		}
	}
}

func TestStaleAtBoundaryIsExclusive(t *testing.T) {
	// "Older than" the threshold is strict — an age exactly equal to it is
	// deliberately not yet stale.
	if staleAt(usageCacheStaleAfter, usageCacheStaleAfter) {
		t.Error("age exactly at threshold must not be stale")
	}
	if !staleAt(usageCacheStaleAfter+time.Nanosecond, usageCacheStaleAfter) {
		t.Error("age one nanosecond past threshold must be stale")
	}
}

func TestRateSegmentsDimAndSuffixWhenUsageCacheStale(t *testing.T) {
	theme := &defaultTermTheme
	rates := slRates{
		Pct5hr:    20,
		PctWeekly: 30,
		Scoped:    []slScoped{{Model: "fable", Pct: 10}},
		Reset5hr:  time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		FetchedAt: time.Now().Add(-12 * time.Minute),
	}

	segs := map[string]segment{
		"rate-5hr":    segRate5hr(rates, theme),
		"rate-weekly": segRateWeekly(rates, theme),
		"rate-scoped": segRateScoped(rates, theme),
		"reset":       segReset(rates, theme),
	}
	for name, seg := range segs {
		if seg.empty {
			t.Fatalf("%s: unexpectedly empty", name)
		}
		if seg.color != theme.Dim {
			t.Errorf("%s: color = %d, want theme.Dim (%d)", name, seg.color, theme.Dim)
		}
		if !strings.HasSuffix(seg.text, "~12m") {
			t.Errorf("%s: text = %q, want suffix ~12m", name, seg.text)
		}
	}
}

func TestRateSegmentsNotMarkedWhenUsageCacheFresh(t *testing.T) {
	theme := &defaultTermTheme
	rates := slRates{Pct5hr: 20, FetchedAt: time.Now().Add(-30 * time.Second)}

	seg := segRate5hr(rates, theme)
	if seg.empty {
		t.Fatal("segRate5hr unexpectedly empty")
	}
	if seg.color == theme.Dim {
		t.Error("segRate5hr dimmed with a 30s-old cache (false positive)")
	}
	if strings.Contains(seg.text, "~") {
		t.Errorf("segRate5hr text = %q, want no age suffix", seg.text)
	}
	if want := rateColor(20, theme); seg.color != want {
		t.Errorf("segRate5hr color = %d, want normal rateColor %d", seg.color, want)
	}
}

func TestSegProjDimsWhenUsageCacheStale(t *testing.T) {
	theme := &defaultTermTheme
	dir := t.TempDir()
	now := time.Now().UTC()
	hist := strings.Join([]string{
		fmt.Sprintf(`{"ts":%q,"pct5hr":10}`, now.Add(-20*time.Minute).Format(time.RFC3339)),
		fmt.Sprintf(`{"ts":%q,"pct5hr":20}`, now.Add(-10*time.Minute).Format(time.RFC3339)),
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "limit-history.jsonl"), []byte(hist), 0644); err != nil {
		t.Fatalf("write history: %v", err)
	}
	rates := slRates{
		Pct5hr:    25,
		Reset5hr:  now.Add(2 * time.Hour).Format(time.RFC3339),
		FetchedAt: time.Now().Add(-12 * time.Minute),
	}

	seg := segProj(rates, dir, theme)
	if seg.empty {
		t.Fatal("segProj unexpectedly empty")
	}
	if seg.color != theme.Dim {
		t.Errorf("segProj color = %d, want theme.Dim (%d)", seg.color, theme.Dim)
	}
	if !strings.HasSuffix(seg.text, "~12m") {
		t.Errorf("segProj text = %q, want suffix ~12m", seg.text)
	}
}

func TestSidecarSegmentsMarkedByOwnThreshold(t *testing.T) {
	theme := &defaultTermTheme

	// 20 minutes old — a long-running turn, must NOT be marked.
	fresh := slSidecar{Turns: 5, Tools: []string{"Read"}, ModTime: time.Now().Add(-20 * time.Minute)}
	if seg := segTurns(fresh, theme); seg.color == theme.Dim || strings.Contains(seg.text, "~") {
		t.Errorf("segTurns(20m old) = %+v, want unmarked", seg)
	}
	if seg := segTools(fresh, theme); seg.color == theme.Dim || strings.Contains(seg.text, "~") {
		t.Errorf("segTools(20m old) = %+v, want unmarked", seg)
	}

	// 3 hours old — past the 60-minute sidecar threshold.
	stale := slSidecar{Turns: 5, Tools: []string{"Read"}, ModTime: time.Now().Add(-3 * time.Hour)}
	if seg := segTurns(stale, theme); seg.color != theme.Dim || !strings.HasSuffix(seg.text, "~3h") {
		t.Errorf("segTurns(3h old) = %+v, want dimmed ~3h", seg)
	}
	if seg := segTools(stale, theme); seg.color != theme.Dim || !strings.HasSuffix(seg.text, "~3h") {
		t.Errorf("segTools(3h old) = %+v, want dimmed ~3h", seg)
	}
}

func TestSegCacheSidecarFallbackMarkedBySidecarThreshold(t *testing.T) {
	theme := &defaultTermTheme
	sc := slSidecar{HasSidecar: true, CachePct: 33, ModTime: time.Now().Add(-3 * time.Hour)}
	seg := segCache(&StatuslineInput{}, sc, theme)
	if seg.color != theme.Dim || !strings.HasSuffix(seg.text, "~3h") {
		t.Errorf("segCache sidecar-fallback(3h old) = %+v, want dimmed ~3h", seg)
	}

	// current_usage present — always fresh, never marked, regardless of sidecar age.
	input := &StatuslineInput{}
	input.ContextWindow = &struct {
		UsedPercentage    float64            `json:"used_percentage"`
		ContextWindowSize int                `json:"context_window_size"`
		TotalInputTokens  int                `json:"total_input_tokens"`
		TotalOutputTokens int                `json:"total_output_tokens"`
		CurrentUsage      *currentUsageField `json:"current_usage"`
	}{
		CurrentUsage: &currentUsageField{InputTokens: 2, CacheReadInputTokens: 98},
	}
	seg = segCache(input, sc, theme)
	if seg.color == theme.Dim || strings.Contains(seg.text, "~") {
		t.Errorf("segCache from current_usage = %+v, want never marked stale", seg)
	}
}

func TestSegCostFromTotalCostUSDNeverMarkedStale(t *testing.T) {
	theme := &defaultTermTheme
	input := &StatuslineInput{Cost: &costField{TotalCostUSD: 5.99}}
	sc := slSidecar{Cost: 1.23, ModTime: time.Now().Add(-90 * 24 * time.Hour)} // 3 months old

	seg := segCost(input, sc, theme)
	if seg.text != " $5.99" {
		t.Errorf("segCost text = %q, want %q", seg.text, " $5.99")
	}
	if seg.color == theme.Dim || strings.Contains(seg.text, "~") {
		t.Errorf("segCost(total_cost_usd) = %+v, want never marked stale despite ancient sidecar", seg)
	}
}

func TestSegCostSidecarFallbackMarkedBySidecarThreshold(t *testing.T) {
	theme := &defaultTermTheme
	sc := slSidecar{Cost: 1.23, ModTime: time.Now().Add(-3 * time.Hour)}
	seg := segCost(&StatuslineInput{}, sc, theme)
	if seg.color != theme.Dim || !strings.HasSuffix(seg.text, "~3h") {
		t.Errorf("segCost sidecar-fallback(3h old) = %+v, want dimmed ~3h", seg)
	}
}

func TestMarkStaleLeavesEmptySegmentEmpty(t *testing.T) {
	theme := &defaultTermTheme
	seg := segment{empty: true}
	got := markStale(seg, time.Now().Add(-3*time.Hour), sidecarStaleAfter, theme)
	if !got.empty {
		t.Error("markStale set empty=false on an already-empty segment")
	}
	if got.text != "" {
		t.Errorf("markStale added text %q to an empty segment", got.text)
	}
}

func TestEmptySegmentsStayEmptyWhenStale(t *testing.T) {
	theme := &defaultTermTheme

	rates := slRates{Pct5hr: -1, FetchedAt: time.Now().Add(-time.Hour)}
	if seg := segRate5hr(rates, theme); !seg.empty {
		t.Errorf("segRate5hr(no data, stale cache) = %+v, want empty", seg)
	}

	sc := slSidecar{Turns: 0, ModTime: time.Now().Add(-3 * time.Hour)}
	if seg := segTurns(sc, theme); !seg.empty {
		t.Errorf("segTurns(no turns, stale sidecar) = %+v, want empty", seg)
	}

	sc2 := slSidecar{HasSidecar: false, ModTime: time.Now().Add(-3 * time.Hour)}
	if seg := segCache(&StatuslineInput{}, sc2, theme); !seg.empty {
		t.Errorf("segCache(no sidecar, stale) = %+v, want empty", seg)
	}
}

func TestLoadRatesForStatuslineReadsFetchedAt(t *testing.T) {
	dir := t.TempDir()
	fetchedAt := time.Now().Add(-20 * time.Minute).Truncate(time.Second)
	cache, _ := json.Marshal(map[string]any{
		"pct5hr": 15, "fetched_at": fetchedAt.Unix(),
	})
	if err := os.WriteFile(filepath.Join(dir, "usage-api-cache.json"), cache, 0644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	rates := loadRatesForStatusline(dir)
	if !rates.FetchedAt.Equal(fetchedAt) {
		t.Errorf("FetchedAt = %v, want %v", rates.FetchedAt, fetchedAt)
	}

	seg := segRate5hr(rates, &defaultTermTheme)
	if seg.color != defaultTermTheme.Dim || !strings.HasSuffix(seg.text, "~20m") {
		t.Errorf("segRate5hr after load = %+v, want dimmed ~20m", seg)
	}
}

func TestLoadSidecarForStatuslineSetsModTime(t *testing.T) {
	dir := t.TempDir()
	state := SidecarState{
		Cumulative: &Cumulative{AgentCalls: 3, ToolCalls: 2, ChatCalls: 1, Cost: 4.5},
	}
	data, _ := json.Marshal(state)
	path := filepath.Join(dir, "sess1.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	mtime := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	sc := loadSidecarForStatusline(dir)
	if !sc.HasSidecar {
		t.Fatal("expected HasSidecar = true")
	}
	if !sc.ModTime.Equal(mtime) {
		t.Errorf("ModTime = %v, want %v", sc.ModTime, mtime)
	}

	seg := segTurns(sc, &defaultTermTheme)
	if seg.color != defaultTermTheme.Dim || !strings.HasSuffix(seg.text, "~3h") {
		t.Errorf("segTurns after load = %+v, want dimmed ~3h", seg)
	}
}

// --- Task 6: PERISCOPE_STATE_DIR isolation ---

func TestStateDirOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PERISCOPE_STATE_DIR", dir)
	if got := stateDir(); got != dir {
		t.Errorf("stateDir() = %q, want override %q", got, dir)
	}
}

func TestStateDirDefaultWhenUnset(t *testing.T) {
	t.Setenv("PERISCOPE_STATE_DIR", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir in this environment: %v", err)
	}
	want := filepath.Join(home, ".claude", "hooks", "cost-state")
	if got := stateDir(); got != want {
		t.Errorf("stateDir() = %q, want default %q", got, want)
	}
}

// An OSC 8 hyperlink is zero-width: the URL is control data, not glyphs.
// visibleLen only stripped SGR colour codes, so a link would have counted its
// whole URL toward the width and wrecked the truncation loop.
func TestVisibleLenIgnoresHyperlinks(t *testing.T) {
	plain := " dash"
	linked := osc8("http://100.115.109.120:8384", plain)

	if got, want := visibleLen(linked), visibleLen(plain); got != want {
		t.Errorf("visibleLen(linked) = %d, want %d (the URL must not count as width)", got, want)
	}
	if !strings.Contains(linked, "http://100.115.109.120:8384") {
		t.Errorf("hyperlink lost its target: %q", linked)
	}
	// Colour codes must still be stripped.
	if got := visibleLen("\x1b[31mabc\x1b[0m"); got != 3 {
		t.Errorf("visibleLen with colour = %d, want 3", got)
	}
}

func TestSegDashLinksToTheConfiguredDashboard(t *testing.T) {
	theme := loadTerminalTheme("", "catppuccin-mocha")

	seg := segDash("http://box:9999", theme)
	if seg.empty {
		t.Fatal("dash segment is empty, want a link")
	}
	if !strings.Contains(seg.text, "http://box:9999") {
		t.Errorf("segment does not carry the configured URL: %q", seg.text)
	}
	if visibleLen(seg.text) > 13 {
		t.Errorf("dash segment renders %d columns, want a compact label", visibleLen(seg.text))
	}
	// No URL configured: the segment disappears rather than linking nowhere.
	if !segDash("", theme).empty {
		t.Error("dash segment should be empty when no dashboard URL is known")
	}
}

func TestResolveDashboardURL(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte("[server]\nhost = \"100.115.109.120\"\nport = 8384\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got, want := resolveDashboardURL(dir), "http://100.115.109.120:8384"; got != want {
		t.Errorf("resolveDashboardURL = %q, want %q", got, want)
	}

	// A wildcard bind is not a reachable address; fall back to localhost.
	if err := os.WriteFile(cfg, []byte("[server]\nhost = \"0.0.0.0\"\nport = 9000\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got, want := resolveDashboardURL(dir), "http://localhost:9000"; got != want {
		t.Errorf("wildcard bind = %q, want %q", got, want)
	}

	// No config at all: no link rather than a wrong one.
	if got := resolveDashboardURL(t.TempDir()); got != "" {
		t.Errorf("missing config = %q, want empty", got)
	}
}
