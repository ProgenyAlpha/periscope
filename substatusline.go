package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type subagentTask struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Type              string `json:"type"`
	Status            string `json:"status"`
	Description       string `json:"description"`
	Label             string `json:"label"`
	StartTime         any    `json:"startTime"`
	Model             string `json:"model"`
	Effort            any    `json:"effort"`
	ContextWindowSize int64  `json:"contextWindowSize"`
	TokenCount        int64  `json:"tokenCount"`
	Cwd               string `json:"cwd"`
}

type subagentInput struct {
	Columns int            `json:"columns"`
	Tasks   []subagentTask `json:"tasks"`
}

// modelTag renders a resolved model id as a short family+version badge, e.g.
// claude-opus-5 -> O5, claude-haiku-4-5 -> H45. Returns "" when the model is
// absent or unrecognized so the caller can omit the field rather than guess.
func modelTag(model string) string {
	m := strings.TrimPrefix(strings.ToLower(model), "claude-")
	if m == "" {
		return ""
	}
	families := []struct {
		prefix string
		letter string
	}{
		{"opus-", "O"},
		{"sonnet-", "S"},
		{"fable-", "F"},
		{"mythos-", "M"},
		{"haiku-", "H"},
	}
	for _, f := range families {
		if !strings.HasPrefix(m, f.prefix) {
			continue
		}
		rest := strings.TrimPrefix(m, f.prefix)
		if i := strings.Index(rest, "-20"); i > 0 {
			rest = rest[:i]
		}
		return f.letter + strings.ReplaceAll(rest, "-", "")
	}
	return ""
}

func subagentElapsed(start any) string {
	var t time.Time
	switch v := start.(type) {
	case float64:
		ms := int64(v)
		if ms > 1e12 {
			t = time.UnixMilli(ms)
		} else {
			t = time.Unix(ms, 0)
		}
	case string:
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return ""
		}
		t = parsed
	default:
		return ""
	}
	d := time.Since(t)
	if d < 0 {
		return ""
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

func subagentEffort(v any) string {
	switch e := v.(type) {
	case string:
		return e
	case float64:
		return fmtTokens(e)
	}
	return ""
}

// subagentTokens matches the agent panel's own precision (105.7k, 1.2M).
func subagentTokens(v int64) string {
	f := float64(v)
	switch {
	case f >= 1e6:
		return fmt.Sprintf("%.1fM", f/1e6)
	case f >= 1e3:
		return fmt.Sprintf("%.1fk", f/1e3)
	}
	return fmt.Sprintf("%d", v)
}

func subagentRow(t subagentTask) string {
	var parts []string
	if s := subagentElapsed(t.StartTime); s != "" {
		parts = append(parts, s)
	}
	if t.TokenCount > 0 {
		parts = append(parts, "↓ "+subagentTokens(t.TokenCount))
	}
	if t.ContextWindowSize > 0 && t.TokenCount > 0 {
		pct := int(float64(t.TokenCount) / float64(t.ContextWindowSize) * 100)
		parts = append(parts, fmt.Sprintf("%d%% ctx", pct))
	}
	tag := modelTag(t.Model)
	if eff := subagentEffort(t.Effort); eff != "" {
		if tag != "" {
			tag += " " + eff
		} else {
			tag = eff
		}
	}
	if tag != "" {
		parts = append(parts, tag)
	}
	return strings.Join(parts, " · ")
}

// cmdSubStatusline renders the agent-panel row body for each visible subagent.
// Claude Code invokes it once per refresh tick with every visible row and
// applies one {"id","content"} JSON line per row we choose to override.
func cmdSubStatusline() {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil || len(raw) == 0 {
		return
	}
	if home, err := os.UserHomeDir(); err == nil {
		os.WriteFile(filepath.Join(home, ".periscope", "last-substatusline-stdin.json"), raw, 0644)
	}

	var in subagentInput
	if json.Unmarshal(stripBOM(raw), &in) != nil {
		return
	}
	slog.Debug("subagent statusline", "tasks", len(in.Tasks), "columns", in.Columns)

	enc := json.NewEncoder(os.Stdout)
	for _, t := range in.Tasks {
		if t.ID == "" {
			continue
		}
		stats := subagentRow(t)
		if stats == "" {
			continue
		}
		content := stats
		if t.Description != "" {
			budget := in.Columns - len(stats) - 3
			if budget > 12 {
				desc := strings.ReplaceAll(t.Description, "\n", " ")
				if len(desc) > budget {
					desc = desc[:budget-1] + "…"
				}
				content = desc + " · " + stats
			}
		}
		enc.Encode(map[string]string{"id": t.ID, "content": content})
	}
}
