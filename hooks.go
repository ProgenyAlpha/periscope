package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// --- Hook Payload (from Claude's hook system via stdin) ---

type HookPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

// --- Transcript Entry ---

type TranscriptEntry struct {
	Type    string           `json:"type"`
	Message *TranscriptMsg   `json:"message,omitempty"`
}

type TranscriptMsg struct {
	Model   string          `json:"model"`
	Usage   *TokenUsage     `json:"usage,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
}

type TokenUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

// --- Sidecar State ---

type SidecarState struct {
	LastOffset int64       `json:"lastOffset"`
	Project    string      `json:"project,omitempty"`
	Cumulative *Cumulative `json:"cumulative"`
	LastTurn   *LastTurn   `json:"lastTurn"`
}

type Cumulative struct {
	Input      int64              `json:"input"`
	CacheRead  int64              `json:"cache_read"`
	CacheWrite int64              `json:"cache_write"`
	Output     int64              `json:"output"`
	Cost       float64            `json:"cost"`
	AgentCost  float64            `json:"agent_cost"`
	ToolCost   float64            `json:"tool_cost"`
	ChatCost   float64            `json:"chat_cost"`
	AgentCalls int                `json:"agent_calls"`
	ToolCalls  int                `json:"tool_calls"`
	ChatCalls  int                `json:"chat_calls"`
	Tools      map[string]*ToolStat `json:"tools"`
}

type ToolStat struct {
	Calls    int     `json:"calls"`
	Weighted float64 `json:"weighted"`
}

type LastTurn struct {
	Cost       float64  `json:"cost"`
	Type       string   `json:"type"`
	Model      string   `json:"model"`
	Input      int64    `json:"input"`
	CacheRead  int64    `json:"cache_read"`
	CacheWrite int64    `json:"cache_write"`
	Output     int64    `json:"output"`
	Tools      []string `json:"tools"`
}

// --- Model Pricing ---

type ModelRates struct {
	Input      float64
	CacheRead  float64
	CacheWrite float64
	Output     float64
}

var modelPricing = map[string]ModelRates{
	"claude-opus-4-6":            {5, 0.50, 6.25, 25},
	"claude-opus-4-5":            {5, 0.50, 6.25, 25},
	"claude-opus-4-1":            {15, 1.50, 18.75, 75},
	"claude-sonnet-4-5-20250929": {3, 0.30, 3.75, 15},
	"claude-haiku-4-5-20251001":  {1, 0.10, 1.25, 5},
	"claude-haiku-3-5":           {0.80, 0.08, 1.00, 4},
}

// Token weights for rate limit calculation
var tokenWeights = struct {
	Input, CacheRead, CacheWrite, Output float64
}{1.0, 0, 1.0, 5.0}

func getModelRates(model string) ModelRates {
	for prefix, rates := range modelPricing {
		if strings.HasPrefix(model, prefix) {
			return rates
		}
	}
	return modelPricing["claude-opus-4-6"] // default
}

// --- Hook: Stop ---

func hookStop() {
	payload := readHookPayload()
	if payload == nil || payload.TranscriptPath == "" || payload.SessionID == "" {
		return
	}

	home, _ := os.UserHomeDir()
	stateDir := filepath.Join(home, ".claude", "hooks", "cost-state")
	os.MkdirAll(stateDir, 0755)
	statePath := filepath.Join(stateDir, payload.SessionID+".json")

	// Extract project slug from transcript path
	projectSlug := ""
	parentDir := filepath.Dir(payload.TranscriptPath)
	parentName := filepath.Base(parentDir)
	if parentName != "cost-state" {
		projectSlug = parentName
	}

	// Load or init state
	state := loadOrInitState(statePath)

	// Check for compaction (file got smaller)
	fi, err := os.Stat(payload.TranscriptPath)
	if err != nil {
		return
	}
	if state.LastOffset > fi.Size() {
		state.LastOffset = 0
		state.Cumulative = newCumulative()
	}

	// Read new bytes from transcript
	f, err := os.Open(payload.TranscriptPath)
	if err != nil {
		return
	}
	defer f.Close()

	f.Seek(state.LastOffset, io.SeekStart)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024) // 10MB line buffer

	var entries []TranscriptEntry
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry TranscriptEntry
		if json.Unmarshal([]byte(line), &entry) == nil {
			entries = append(entries, entry)
		}
	}

	newOffset, _ := f.Seek(0, io.SeekCurrent)

	// Reset last turn
	turn := &LastTurn{Type: "chat"}

	for _, entry := range entries {
		if entry.Type != "assistant" || entry.Message == nil || entry.Message.Usage == nil {
			continue
		}

		usage := entry.Message.Usage
		model := entry.Message.Model
		rates := getModelRates(model)

		cost := float64(usage.InputTokens)*rates.Input/1e6 +
			float64(usage.CacheReadInputTokens)*rates.CacheRead/1e6 +
			float64(usage.CacheCreationInputTokens)*rates.CacheWrite/1e6 +
			float64(usage.OutputTokens)*rates.Output/1e6

		turnInfo := getTurnInfo(entry.Message.Content)

		// Weighted tokens
		weighted := float64(usage.InputTokens)*tokenWeights.Input +
			float64(usage.CacheReadInputTokens)*tokenWeights.CacheRead +
			float64(usage.CacheCreationInputTokens)*tokenWeights.CacheWrite +
			float64(usage.OutputTokens)*tokenWeights.Output

		// Accumulate
		state.Cumulative.Input += usage.InputTokens
		state.Cumulative.CacheRead += usage.CacheReadInputTokens
		state.Cumulative.CacheWrite += usage.CacheCreationInputTokens
		state.Cumulative.Output += usage.OutputTokens
		state.Cumulative.Cost += cost

		switch turnInfo.turnType {
		case "agent":
			state.Cumulative.AgentCost += cost
			state.Cumulative.AgentCalls++
		case "tool":
			state.Cumulative.ToolCost += cost
			state.Cumulative.ToolCalls++
		default:
			state.Cumulative.ChatCost += cost
			state.Cumulative.ChatCalls++
		}

		// Per-tool tracking
		seen := map[string]bool{}
		for _, toolName := range turnInfo.tools {
			tKey := toolName
			if toolName == "Task" && len(turnInfo.agents) > 0 {
				tKey = "Task/" + turnInfo.agents[0]
			}
			if state.Cumulative.Tools[tKey] == nil {
				state.Cumulative.Tools[tKey] = &ToolStat{}
			}
			state.Cumulative.Tools[tKey].Calls++
			seen[tKey] = true
		}
		if len(seen) > 0 {
			perTool := weighted / float64(len(seen))
			for tKey := range seen {
				state.Cumulative.Tools[tKey].Weighted += perTool
			}
		}

		// This turn
		turn.Cost += cost
		turn.Input += usage.InputTokens
		turn.CacheRead += usage.CacheReadInputTokens
		turn.CacheWrite += usage.CacheCreationInputTokens
		turn.Output += usage.OutputTokens
		turn.Tools = append(turn.Tools, turnInfo.tools...)
		turn.Model = model
		if turnInfo.turnType == "agent" {
			turn.Type = "agent"
		} else if turnInfo.turnType == "tool" && turn.Type != "agent" {
			turn.Type = "tool"
		}
	}

	// Update state
	state.LastOffset = newOffset
	state.LastTurn = turn
	if projectSlug != "" {
		state.Project = projectSlug
	}

	// Write sidecar
	data, _ := json.Marshal(state)
	os.WriteFile(statePath, data, 0644)

	// Append to usage-history.jsonl
	totalCalls := state.Cumulative.AgentCalls + state.Cumulative.ToolCalls + state.Cumulative.ChatCalls
	shortSid := payload.SessionID
	if len(shortSid) > 8 {
		shortSid = shortSid[:8]
	}

	historyEntry := map[string]any{
		"ts":    time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"sid":   shortSid,
		"input": state.Cumulative.Input,
		"cr":    state.Cumulative.CacheRead,
		"cw":    state.Cumulative.CacheWrite,
		"out":   state.Cumulative.Output,
		"cost":  math.Round(state.Cumulative.Cost*100) / 100,
		"turns": totalCalls,
	}
	line, _ := json.Marshal(historyEntry)

	histPath := filepath.Join(stateDir, "usage-history.jsonl")
	if hf, err := os.OpenFile(histPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
		hf.Write(append(line, '\n'))
		hf.Close()
	}
}

// --- Hook: Display ---

func hookDisplay() {
	payload := readHookPayload()
	if payload == nil || payload.SessionID == "" {
		return
	}

	home, _ := os.UserHomeDir()
	stateDir := filepath.Join(home, ".claude", "hooks", "cost-state")
	statePath := filepath.Join(stateDir, payload.SessionID+".json")

	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		output := map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":    "UserPromptSubmit",
				"additionalContext": "TELEMETRY - First turn - tracking starts next message.",
			},
		}
		data, _ := json.Marshal(output)
		fmt.Print(string(data))
		return
	}

	stateData, err := os.ReadFile(statePath)
	if err != nil {
		return
	}
	stateData = stripBOM(stateData)

	var state SidecarState
	if json.Unmarshal(stateData, &state) != nil || state.Cumulative == nil {
		return
	}

	c := state.Cumulative
	totalCalls := c.AgentCalls + c.ToolCalls + c.ChatCalls

	// Cache hit rate
	totalIn := c.Input + c.CacheRead
	cacheHit := 0.0
	if totalIn > 0 {
		cacheHit = float64(c.CacheRead) / float64(totalIn) * 100
	}
	cacheStr := fmt.Sprintf("%.1f%%", cacheHit)
	if cacheHit < 95 {
		cacheStr = fmt.Sprintf("%.0f%%", cacheHit)
	}

	// Top tools
	toolStr := ""
	if len(c.Tools) > 0 {
		type toolEntry struct {
			name     string
			calls    int
			weighted float64
		}
		var tools []toolEntry
		for name, stat := range c.Tools {
			tools = append(tools, toolEntry{name, stat.Calls, stat.Weighted})
		}
		// Sort by weighted descending (simple selection)
		for i := 0; i < len(tools); i++ {
			for j := i + 1; j < len(tools); j++ {
				if tools[j].weighted > tools[i].weighted {
					tools[i], tools[j] = tools[j], tools[i]
				}
			}
		}
		var parts []string
		limit := 5
		if len(tools) < limit {
			limit = len(tools)
		}
		for _, t := range tools[:limit] {
			w := fmtTokens(t.weighted)
			parts = append(parts, fmt.Sprintf("%s:%dx(%s)", t.name, t.calls, w))
		}
		toolStr = strings.Join(parts, " | ")
	}

	// Rate limits
	rateStr := ""
	cacheFile := filepath.Join(stateDir, "usage-api-cache.json")
	if data, err := os.ReadFile(cacheFile); err == nil {
		data = stripBOM(data)
		var usage map[string]any
		if json.Unmarshal(data, &usage) == nil {
			pct5hr := intOrDefault(usage["pct5hr"], -1)
			pctWk := intOrDefault(usage["pctWeekly"], -1)
			if pct5hr >= 0 {
				rateStr = fmt.Sprintf("5hr [%s] %d%% | Weekly [%s] %d%%",
					progressBar(pct5hr, 20), pct5hr, progressBar(pctWk, 20), pctWk)
			}
		}
	}

	// Build output
	line1 := fmt.Sprintf("TELEMETRY: %d calls (agent:%d tool:%d chat:%d) | cache:%s",
		totalCalls, c.AgentCalls, c.ToolCalls, c.ChatCalls, cacheStr)
	lines := []string{line1}
	if toolStr != "" {
		lines = append(lines, "Tools: "+toolStr)
	}
	if rateStr != "" {
		lines = append(lines, "Rate limits: "+rateStr)
	}

	output := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":    "UserPromptSubmit",
			"additionalContext": strings.Join(lines, "\n"),
		},
	}
	data, _ := json.Marshal(output)
	fmt.Print(string(data))
}

// --- Helpers ---

func readHookPayload() *HookPayload {
	data, err := io.ReadAll(os.Stdin)
	if err != nil || len(data) == 0 {
		return nil
	}
	var p HookPayload
	if json.Unmarshal(data, &p) != nil {
		return nil
	}
	return &p
}

func loadOrInitState(path string) *SidecarState {
	if data, err := os.ReadFile(path); err == nil {
		data = stripBOM(data)
		var state SidecarState
		if json.Unmarshal(data, &state) == nil {
			if state.Cumulative == nil {
				state.Cumulative = newCumulative()
			}
			if state.Cumulative.Tools == nil {
				state.Cumulative.Tools = map[string]*ToolStat{}
			}
			return &state
		}
	}
	return &SidecarState{
		Cumulative: newCumulative(),
		LastTurn:   &LastTurn{Type: "chat"},
	}
}

func newCumulative() *Cumulative {
	return &Cumulative{Tools: map[string]*ToolStat{}}
}

type turnInfo struct {
	turnType string
	tools    []string
	agents   []string
}

func getTurnInfo(content json.RawMessage) turnInfo {
	info := turnInfo{turnType: "chat"}
	if content == nil {
		return info
	}
	var blocks []struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Input *struct {
			SubagentType string `json:"subagent_type"`
		} `json:"input,omitempty"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return info
	}
	for _, b := range blocks {
		if b.Type == "tool_use" {
			info.tools = append(info.tools, b.Name)
			if b.Name == "Task" {
				info.turnType = "agent"
				if b.Input != nil && b.Input.SubagentType != "" {
					info.agents = append(info.agents, b.Input.SubagentType)
				}
			} else if info.turnType != "agent" {
				info.turnType = "tool"
			}
		}
	}
	return info
}

func fmtTokens(v float64) string {
	if v >= 1e9 {
		return fmt.Sprintf("%.1fB", v/1e9)
	}
	if v >= 1e6 {
		return fmt.Sprintf("%.1fM", v/1e6)
	}
	if v >= 1e3 {
		return fmt.Sprintf("%.0fK", v/1e3)
	}
	return fmt.Sprintf("%.0f", v)
}

func progressBar(pct, width int) string {
	if pct < 0 {
		pct = 0
	}
	filled := pct * width / 100
	if filled > width {
		filled = width
	}
	return strings.Repeat("#", filled) + strings.Repeat("-", width-filled)
}

func intOrDefault(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return def
}
