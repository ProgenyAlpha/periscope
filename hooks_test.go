package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ProgenyAlpha/periscope/internal/forecast"
	"github.com/ProgenyAlpha/periscope/internal/pricing"
	"github.com/ProgenyAlpha/periscope/internal/store"
)

func TestCleanFirstPrompt(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "slash command stripped",
			in:   "/plan foo bar",
			want: "Foo bar",
		},
		{
			name: "agent mention stripped",
			in:   "@researcher do thing",
			want: "Do thing",
		},
		{
			name: "HTML tags stripped",
			in:   "<b>hello</b> world",
			want: "Hello world",
		},
		{
			name: "whitespace collapsed",
			in:   "  lots   of   spaces  ",
			want: "Lots of spaces",
		},
		{
			name: "long string truncated at word boundary",
			in:   "This is a really long string that definitely exceeds the fifty character limit we set",
			want: func() string {
				s := "This is a really long string that definitely exceeds the fifty character limit we set"
				// Should truncate around 50 chars on word boundary
				if len(s) <= 50 {
					return s
				}
				cut := 50
				for cut > 30 && s[cut] != ' ' {
					cut--
				}
				if s[cut] == ' ' {
					return s[:cut] + "..."
				}
				return s[:50] + "..."
			}(),
		},
		{
			name: "empty after strip returns original",
			in:   "/command",
			want: "/command",
		},
		{
			name: "unicode uppercase",
			in:   "\u00fcber cool",
			want: "\u00dcber cool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := store.CleanFirstPrompt(tt.in)
			if got != tt.want {
				t.Errorf("CleanFirstPrompt(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestGetTurnInfo(t *testing.T) {
	tests := []struct {
		name      string
		content   json.RawMessage
		wantType  string
		wantTool  []string
		wantAgent []string
	}{
		{
			name:     "nil content",
			content:  nil,
			wantType: "chat",
		},
		{
			name:     "text only",
			content:  json.RawMessage(`[{"type":"text","text":"hello"}]`),
			wantType: "chat",
		},
		{
			name:     "tool use",
			content:  json.RawMessage(`[{"type":"tool_use","name":"Read"}]`),
			wantType: "tool",
			wantTool: []string{"Read"},
		},
		{
			name:      "legacy Task tool with subagent",
			content:   json.RawMessage(`[{"type":"tool_use","name":"Task","input":{"subagent_type":"explore"}}]`),
			wantType:  "agent",
			wantTool:  []string{"Task"},
			wantAgent: []string{"explore"},
		},
		{
			name:      "current Agent tool with subagent",
			content:   json.RawMessage(`[{"type":"tool_use","name":"Agent","input":{"subagent_type":"explore"}}]`),
			wantType:  "agent",
			wantTool:  []string{"Agent"},
			wantAgent: []string{"explore"},
		},
		{
			name:      "mixed tool and agent, agent wins",
			content:   json.RawMessage(`[{"type":"tool_use","name":"Read"},{"type":"tool_use","name":"Task","input":{"subagent_type":"Bash"}}]`),
			wantType:  "agent",
			wantTool:  []string{"Read", "Task"},
			wantAgent: []string{"Bash"},
		},
		{
			name:     "invalid json",
			content:  json.RawMessage(`invalid json`),
			wantType: "chat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := getTurnInfo(tt.content)
			if info.turnType != tt.wantType {
				t.Errorf("turnType = %q, want %q", info.turnType, tt.wantType)
			}
			if !sliceEqual(info.tools, tt.wantTool) {
				t.Errorf("tools = %v, want %v", info.tools, tt.wantTool)
			}
			if !sliceEqual(info.agents, tt.wantAgent) {
				t.Errorf("agents = %v, want %v", info.agents, tt.wantAgent)
			}
		})
	}
}

func TestRecordTurn_CurrentAgentToolName(t *testing.T) {
	c := &Cumulative{Tools: map[string]*ToolStat{}}
	info := turnInfo{turnType: "agent", tools: []string{"Agent"}, agents: []string{"explore"}}

	recordTurn(c, info, 1.5, 10)

	if c.AgentCalls != 1 {
		t.Errorf("AgentCalls = %d, want 1", c.AgentCalls)
	}
	if c.AgentCost != 1.5 {
		t.Errorf("AgentCost = %v, want 1.5", c.AgentCost)
	}
	if c.ToolCalls != 0 || c.ChatCalls != 0 {
		t.Errorf("ToolCalls/ChatCalls = %d/%d, want 0/0", c.ToolCalls, c.ChatCalls)
	}
	stat := c.Tools["Task/explore"]
	if stat == nil || stat.Calls != 1 {
		t.Errorf(`Tools["Task/explore"] = %+v, want Calls=1 (canonical key, not "Agent/explore")`, stat)
	}
	if _, ok := c.Tools["Agent/explore"]; ok {
		t.Error(`Tools["Agent/explore"] present, want subagent spawns canonicalized under "Task/" only`)
	}
}

func TestRecordTurn_LegacyTaskToolName(t *testing.T) {
	c := &Cumulative{Tools: map[string]*ToolStat{}}
	info := turnInfo{turnType: "agent", tools: []string{"Task"}, agents: []string{"explore"}}

	recordTurn(c, info, 1.5, 10)

	if c.AgentCalls != 1 {
		t.Errorf("AgentCalls = %d, want 1", c.AgentCalls)
	}
	if c.AgentCost != 1.5 {
		t.Errorf("AgentCost = %v, want 1.5", c.AgentCost)
	}
	stat := c.Tools["Task/explore"]
	if stat == nil || stat.Calls != 1 {
		t.Errorf(`Tools["Task/explore"] = %+v, want Calls=1`, stat)
	}
}

func TestRecordTurn_LegacyAndCurrentShareOneCanonicalKey(t *testing.T) {
	c := &Cumulative{Tools: map[string]*ToolStat{}}

	recordTurn(c, turnInfo{turnType: "agent", tools: []string{"Task"}, agents: []string{"explore"}}, 1, 1)
	recordTurn(c, turnInfo{turnType: "agent", tools: []string{"Agent"}, agents: []string{"explore"}}, 1, 1)

	if len(c.Tools) != 1 {
		t.Errorf("Tools has %d keys, want 1 (legacy Task and current Agent must merge): %v", len(c.Tools), c.Tools)
	}
	if stat := c.Tools["Task/explore"]; stat == nil || stat.Calls != 2 {
		t.Errorf(`Tools["Task/explore"].Calls = %+v, want Calls=2`, stat)
	}
}

func TestRecordTurn_BucketsSumToCumulativeCost(t *testing.T) {
	c := &Cumulative{Tools: map[string]*ToolStat{}}
	turns := []struct {
		info turnInfo
		cost float64
	}{
		{turnInfo{turnType: "agent", tools: []string{"Agent"}, agents: []string{"explore"}}, 2.5},
		{turnInfo{turnType: "agent", tools: []string{"Task"}, agents: []string{"Bash"}}, 1.25},
		{turnInfo{turnType: "tool", tools: []string{"Read"}}, 0.10},
		{turnInfo{turnType: "chat"}, 0.05},
	}

	var total float64
	for _, tt := range turns {
		recordTurn(c, tt.info, tt.cost, tt.cost*5)
		total += tt.cost
	}

	sum := c.AgentCost + c.ToolCost + c.ChatCost
	if sum != total {
		t.Errorf("AgentCost+ToolCost+ChatCost = %v, want %v (sum of all turn costs)", sum, total)
	}
}

func sliceEqual(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	return strings.Join(a, ",") == strings.Join(b, ",")
}

func TestFmtTokens(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want string
	}{
		{"zero", 0, "0"},
		{"hundreds", 500, "500"},
		{"thousands", 1500, "2K"},
		{"millions", 1_500_000, "1.5M"},
		{"billions", 1_500_000_000, "1.5B"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fmtTokens(tt.in)
			if got != tt.want {
				t.Errorf("fmtTokens(%.0f) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestIntOrDefault(t *testing.T) {
	tests := []struct {
		name string
		in   any
		def  int
		want int
	}{
		{"float64", float64(42), -1, 42},
		{"int", int(7), -1, 7},
		{"string", "hello", -1, -1},
		{"nil", nil, -1, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := forecast.IntOrDefault(tt.in, tt.def)
			if got != tt.want {
				t.Errorf("IntOrDefault(%v, %d) = %d, want %d", tt.in, tt.def, got, tt.want)
			}
		})
	}
}

func TestTurnCost_CacheCreationBreakdown(t *testing.T) {
	rates := pricing.ModelRates{Input: 5, CacheRead: 0.5, CacheWrite: 6.25, CacheWrite1h: 10, Output: 25}

	cost1h, cw1h, cw5m := turnCost(&TokenUsage{
		CacheCreation: &CacheCreation{Ephemeral1hInputTokens: 1000},
	}, rates)
	want1h := 1000 * rates.CacheWrite1h / 1e6
	if cost1h != want1h {
		t.Errorf("1h-only cost = %v, want %v", cost1h, want1h)
	}
	if cw1h != 1000 || cw5m != 0 {
		t.Errorf("1h-only split = (%d,%d), want (1000,0)", cw1h, cw5m)
	}

	cost5m, cw1h, cw5m := turnCost(&TokenUsage{
		CacheCreation: &CacheCreation{Ephemeral5mInputTokens: 1000},
	}, rates)
	want5m := 1000 * rates.CacheWrite / 1e6
	if cost5m != want5m {
		t.Errorf("5m-only cost = %v, want %v", cost5m, want5m)
	}
	if cw1h != 0 || cw5m != 1000 {
		t.Errorf("5m-only split = (%d,%d), want (0,1000)", cw1h, cw5m)
	}

	// Same token count, 1h priced at 2.0x input vs 5m at 1.25x input —
	// assert the exact dollar difference between the two rates.
	gotDiff := cost1h - cost5m
	wantDiff := 1000 * (rates.CacheWrite1h - rates.CacheWrite) / 1e6
	if gotDiff != wantDiff {
		t.Errorf("1h-vs-5m cost diff = %v, want %v", gotDiff, wantDiff)
	}
}

func TestTurnCost_NoBreakdownFallsBackToFlatFieldAt5mRate(t *testing.T) {
	rates := pricing.ModelRates{Input: 5, CacheRead: 0.5, CacheWrite: 6.25, CacheWrite1h: 10, Output: 25}

	cost, cw1h, cw5m := turnCost(&TokenUsage{CacheCreationInputTokens: 2000}, rates)

	want := 2000 * rates.CacheWrite / 1e6
	if cost != want {
		t.Errorf("cost = %v, want %v (flat field at 5m rate)", cost, want)
	}
	if cw1h != 0 {
		t.Errorf("cacheWrite1h = %d, want 0", cw1h)
	}
	if cw5m != 2000 {
		t.Errorf("cacheWrite5m = %d, want 2000", cw5m)
	}
}

func TestTurnCost_BreakdownNeverDoubleCounts(t *testing.T) {
	rates := pricing.ModelRates{Input: 5, CacheRead: 0.5, CacheWrite: 6.25, CacheWrite1h: 10, Output: 25}

	// Real transcripts carry the flat field (N+M) alongside the breakdown.
	// Only the breakdown must be priced.
	cost, cw1h, cw5m := turnCost(&TokenUsage{
		CacheCreationInputTokens: 3000, // N+M, must be ignored when breakdown present
		CacheCreation:            &CacheCreation{Ephemeral1hInputTokens: 1000, Ephemeral5mInputTokens: 2000},
	}, rates)

	want := float64(1000)*rates.CacheWrite1h/1e6 + float64(2000)*rates.CacheWrite/1e6
	if cost != want {
		t.Errorf("cost = %v, want %v (breakdown only, no double count)", cost, want)
	}
	if cw1h != 1000 || cw5m != 2000 {
		t.Errorf("split = (%d,%d), want (1000,2000)", cw1h, cw5m)
	}
}

func TestFloatOrDefault(t *testing.T) {
	tests := []struct {
		name string
		in   any
		def  float64
		want float64
	}{
		{"float64", float64(3.14), -1, 3.14},
		{"int", int(7), -1, 7.0},
		{"string", "hello", -1, -1},
		{"nil", nil, -1, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := forecast.FloatOrDefault(tt.in, tt.def)
			if got != tt.want {
				t.Errorf("forecast.FloatOrDefault(%v, %.2f) = %.2f, want %.2f", tt.in, tt.def, got, tt.want)
			}
		})
	}
}
