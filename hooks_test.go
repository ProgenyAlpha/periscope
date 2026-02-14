package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shawnwakeman/periscope/internal/store"
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
		name     string
		content  json.RawMessage
		wantType string
		wantTool []string
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
			name:     "agent task with subagent",
			content:  json.RawMessage(`[{"type":"tool_use","name":"Task","input":{"subagent_type":"explore"}}]`),
			wantType: "agent",
			wantTool: []string{"Task"},
			wantAgent: []string{"explore"},
		},
		{
			name:     "mixed tool and agent, agent wins",
			content:  json.RawMessage(`[{"type":"tool_use","name":"Read"},{"type":"tool_use","name":"Task","input":{"subagent_type":"Bash"}}]`),
			wantType: "agent",
			wantTool: []string{"Read", "Task"},
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

func TestProgressBar(t *testing.T) {
	tests := []struct {
		name string
		pct  int
		w    int
		want string
	}{
		{"zero", 0, 20, "--------------------"},
		{"half", 50, 20, "##########----------"},
		{"full", 100, 20, "####################"},
		{"over", 150, 20, "####################"},
		{"negative", -5, 20, "--------------------"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := progressBar(tt.pct, tt.w)
			if got != tt.want {
				t.Errorf("progressBar(%d, %d) = %q, want %q", tt.pct, tt.w, got, tt.want)
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
			got := intOrDefault(tt.in, tt.def)
			if got != tt.want {
				t.Errorf("intOrDefault(%v, %d) = %d, want %d", tt.in, tt.def, got, tt.want)
			}
		})
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
			got := floatOrDefault(tt.in, tt.def)
			if got != tt.want {
				t.Errorf("floatOrDefault(%v, %.2f) = %.2f, want %.2f", tt.in, tt.def, got, tt.want)
			}
		})
	}
}
