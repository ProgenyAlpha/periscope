package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ProgenyAlpha/periscope/internal/anthropic"
	"github.com/ProgenyAlpha/periscope/internal/forecast"
	"github.com/ProgenyAlpha/periscope/internal/pricing"
	"github.com/ProgenyAlpha/periscope/internal/store"
)

// --- Hook Payload (from Claude's hook system via stdin) ---

type HookPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

// --- Transcript Entry ---

type TranscriptEntry struct {
	Type    string         `json:"type"`
	Message *TranscriptMsg `json:"message,omitempty"`
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
	LastOffset     int64          `json:"lastOffset"`
	Project        string         `json:"project,omitempty"`
	EffortLevel    string         `json:"effortLevel,omitempty"`
	FirstPrompt    string         `json:"firstPrompt,omitempty"`
	GeneratedTitle string         `json:"generatedTitle,omitempty"`
	Cumulative     *Cumulative    `json:"cumulative"`
	LastTurn       *LastTurn      `json:"lastTurn"`
	Models         map[string]int `json:"models,omitempty"`
}

type Cumulative struct {
	Input      int64                `json:"input"`
	CacheRead  int64                `json:"cache_read"`
	CacheWrite int64                `json:"cache_write"`
	Output     int64                `json:"output"`
	Cost       float64              `json:"cost"`
	AgentCost  float64              `json:"agent_cost"`
	ToolCost   float64              `json:"tool_cost"`
	ChatCost   float64              `json:"chat_cost"`
	AgentCalls int                  `json:"agent_calls"`
	ToolCalls  int                  `json:"tool_calls"`
	ChatCalls  int                  `json:"chat_calls"`
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

// --- Hook: Stop ---

func extractFirstPrompt(transcriptPath string) string {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if entry.Type != "human" && entry.Type != "user" {
			continue
		}
		content := entry.Message.Content
		if len(content) == 0 {
			continue
		}
		var text string
		if content[0] == '"' {
			json.Unmarshal(content, &text)
		} else if content[0] == '[' {
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(content, &blocks) == nil {
				for _, b := range blocks {
					if b.Type == "text" && b.Text != "" {
						text = b.Text
						break
					}
				}
			}
		}
		text = strings.TrimSpace(text)
		if text == "" || strings.HasPrefix(text, "[Request interrupted") {
			continue
		}
		if len(text) > 120 {
			text = text[:117] + "..."
		}
		return text
	}
	return ""
}

// extractUserPrompts collects up to n human/user messages from a transcript.
func extractUserPrompts(transcriptPath string, n int) []string {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var prompts []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)
	for scanner.Scan() && len(prompts) < n {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if entry.Type != "human" && entry.Type != "user" {
			continue
		}
		content := entry.Message.Content
		if len(content) == 0 {
			continue
		}
		var text string
		if content[0] == '"' {
			json.Unmarshal(content, &text)
		} else if content[0] == '[' {
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(content, &blocks) == nil {
				for _, b := range blocks {
					if b.Type == "text" && b.Text != "" {
						text = b.Text
						break
					}
				}
			}
		}
		text = strings.TrimSpace(text)
		if text == "" || strings.HasPrefix(text, "[Request interrupted") {
			continue
		}
		if len(text) > 200 {
			text = text[:197] + "..."
		}
		prompts = append(prompts, text)
	}
	return prompts
}

// generateSessionTitle calls Haiku to generate a short dashboard title,
// then writes it back to the sidecar JSON. Runs synchronously.
func generateSessionTitle(statePath string, project string, prompts []string) {
	slog.Info("generating session title", "file", filepath.Base(statePath), "prompts", len(prompts), "project", project)

	home, _ := os.UserHomeDir()
	claudeDir := filepath.Join(home, ".claude")
	client, err := anthropic.NewClientFromDisk(claudeDir)
	if err != nil {
		slog.Warn("title gen: no OAuth token", "err", err)
		return
	}

	title, err := anthropic.GenerateTitle(client, project, prompts)
	if err != nil {
		slog.Warn("title gen failed", "err", err)
		return
	}

	// Read-modify-write the sidecar
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		slog.Warn("title gen: cannot read sidecar", "err", err)
		return
	}
	var state SidecarState
	if json.Unmarshal(stateData, &state) != nil {
		slog.Warn("title gen: cannot parse sidecar")
		return
	}
	state.GeneratedTitle = title
	data, _ := json.Marshal(state)
	if err := os.WriteFile(statePath, data, 0644); err != nil {
		slog.Error("title gen: write sidecar failed", "err", err)
		return
	}

	slog.Info("title generated", "title", title, "file", filepath.Base(statePath))
}

func hookStop() {
	slog.Info("stop hook triggered")
	payload := readHookPayload()
	if payload == nil || payload.TranscriptPath == "" || payload.SessionID == "" {
		slog.Warn("stop hook: invalid or missing payload")
		return
	}

	slog.Info("stop hook: processing", "sid", payload.SessionID[:8])

	home, err := os.UserHomeDir()
	if err != nil {
		slog.Error("cannot determine home directory", "err", err)
		os.Exit(1)
	}
	stateDir := filepath.Join(home, ".claude", "hooks", "cost-state")
	os.MkdirAll(stateDir, 0755)
	statePath := filepath.Join(stateDir, payload.SessionID+".json")

	projectSlug := ""
	parentDir := filepath.Dir(payload.TranscriptPath)
	parentName := filepath.Base(parentDir)
	if parentName != "cost-state" {
		projectSlug = parentName
	}

	state := loadOrInitState(statePath)

	fi, err := os.Stat(payload.TranscriptPath)
	if err != nil {
		slog.Error("stop hook: cannot stat transcript", "path", payload.TranscriptPath, "err", err)
		return
	}
	if state.LastOffset > fi.Size() {
		slog.Warn("stop hook: transcript compacted, resetting", "was", state.LastOffset, "now", fi.Size())
		state.LastOffset = 0
		state.Cumulative = newCumulative()
	}

	f, err := os.Open(payload.TranscriptPath)
	if err != nil {
		slog.Error("stop hook: cannot open transcript", "err", err)
		return
	}
	defer f.Close()

	if _, err := f.Seek(state.LastOffset, io.SeekStart); err != nil {
		slog.Warn("stop hook: seek failed, reading from start", "offset", state.LastOffset, "err", err)
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)

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

	newOffset, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		slog.Warn("stop hook: offset seek failed", "err", err)
	}

	slog.Info("stop hook: parsed entries", "count", len(entries))

	turn := &LastTurn{Type: "chat"}

	for _, entry := range entries {
		if entry.Type != "assistant" || entry.Message == nil || entry.Message.Usage == nil {
			continue
		}

		usage := entry.Message.Usage
		model := entry.Message.Model
		rates := pricing.GetRates(model)

		cost := float64(usage.InputTokens)*rates.Input/1e6 +
			float64(usage.CacheReadInputTokens)*rates.CacheRead/1e6 +
			float64(usage.CacheCreationInputTokens)*rates.CacheWrite/1e6 +
			float64(usage.OutputTokens)*rates.Output/1e6

		turnInfo := getTurnInfo(entry.Message.Content)

		weighted := float64(usage.InputTokens)*pricing.TokenWeights.Input +
			float64(usage.CacheReadInputTokens)*pricing.TokenWeights.CacheRead +
			float64(usage.CacheCreationInputTokens)*pricing.TokenWeights.CacheWrite +
			float64(usage.OutputTokens)*pricing.TokenWeights.Output

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

		mShort := model
		mShort = strings.TrimPrefix(mShort, "claude-")
		if idx := strings.LastIndex(mShort, "-20"); idx > 0 && len(mShort)-idx <= 9 {
			mShort = mShort[:idx]
		}
		if mShort != "" {
			if state.Models == nil {
				state.Models = map[string]int{}
			}
			state.Models[mShort]++
		}

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

	state.LastOffset = newOffset
	state.LastTurn = turn
	if projectSlug != "" {
		state.Project = projectSlug
	}

	if state.FirstPrompt == "" {
		if raw := extractFirstPrompt(payload.TranscriptPath); raw != "" {
			state.FirstPrompt = store.CleanFirstPrompt(raw)
		}
	}

	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if raw, err := os.ReadFile(settingsPath); err == nil {
		var settings struct {
			EffortLevel string `json:"effortLevel"`
		}
		if json.Unmarshal(raw, &settings) == nil && settings.EffortLevel != "" {
			state.EffortLevel = settings.EffortLevel
		}
	}

	data, _ := json.Marshal(state)
	if err := os.WriteFile(statePath, data, 0644); err != nil {
		slog.Error("stop hook: write sidecar failed", "path", statePath, "err", err)
	} else {
		totalCalls := state.Cumulative.AgentCalls + state.Cumulative.ToolCalls + state.Cumulative.ChatCalls
		slog.Info("stop hook: sidecar saved", "cost", state.Cumulative.Cost, "calls", totalCalls)
	}

	totalCalls := state.Cumulative.AgentCalls + state.Cumulative.ToolCalls + state.Cumulative.ChatCalls
	if state.GeneratedTitle == "" && totalCalls >= 5 {
		prompts := extractUserPrompts(payload.TranscriptPath, 3)
		if len(prompts) >= 2 {
			generateSessionTitle(statePath, state.Project, prompts)
		}
	}

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
	if state.EffortLevel != "" {
		historyEntry["effort"] = state.EffortLevel
	}
	line, _ := json.Marshal(historyEntry)

	histPath := filepath.Join(stateDir, "usage-history.jsonl")
	if hf, err := os.OpenFile(histPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
		defer hf.Close()
		if _, err := hf.Write(append(line, '\n')); err != nil {
			slog.Error("stop hook: history write failed", "err", err)
		} else {
			slog.Debug("stop hook: history appended")
		}
	} else {
		slog.Error("stop hook: history append failed", "err", err)
	}
}

// --- Hook: Display ---

func hookDisplay() {
	slog.Info("display hook triggered")
	payload := readHookPayload()
	if payload == nil || payload.SessionID == "" {
		slog.Warn("display hook: invalid payload")
		return
	}

	slog.Info("display hook: processing", "sid", payload.SessionID[:8])

	home, err := os.UserHomeDir()
	if err != nil {
		slog.Error("cannot determine home directory", "err", err)
		os.Exit(1)
	}
	stateDir := filepath.Join(home, ".claude", "hooks", "cost-state")
	claudeDir := filepath.Join(home, ".claude")
	statePath := filepath.Join(stateDir, payload.SessionID+".json")

	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		slog.Info("display hook: first turn, no sidecar")
		output := map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "UserPromptSubmit",
				"additionalContext": "TELEMETRY - First turn - tracking starts next message.",
			},
		}
		data, _ := json.Marshal(output)
		fmt.Print(string(data))
		return
	}

	stateData, err := os.ReadFile(statePath)
	if err != nil {
		slog.Error("display hook: read sidecar failed", "err", err)
		return
	}
	stateData = stripBOM(stateData)

	var state SidecarState
	if json.Unmarshal(stateData, &state) != nil || state.Cumulative == nil {
		slog.Warn("display hook: invalid sidecar data")
		return
	}

	c := state.Cumulative
	totalCalls := c.AgentCalls + c.ToolCalls + c.ChatCalls
	slog.Info("display hook: loaded sidecar", "cost", c.Cost, "calls", totalCalls)

	// Append history entry
	shortSid := payload.SessionID
	if len(shortSid) > 8 {
		shortSid = shortSid[:8]
	}
	historyEntry := map[string]any{
		"ts":    time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"sid":   shortSid,
		"input": c.Input,
		"cr":    c.CacheRead,
		"cw":    c.CacheWrite,
		"out":   c.Output,
		"cost":  math.Round(c.Cost*100) / 100,
		"turns": totalCalls,
	}
	histLine, _ := json.Marshal(historyEntry)
	histPath := filepath.Join(stateDir, "usage-history.jsonl")
	if hf, err := os.OpenFile(histPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
		hf.Write(append(histLine, '\n'))
		hf.Close()
	} else {
		slog.Error("display hook: history append failed", "err", err)
	}

	// Cache hit rate
	totalIn := c.Input + c.CacheRead
	cacheHit := 0.0
	if totalIn > 0 {
		cacheHit = float64(c.CacheRead) / float64(totalIn) * 100
	}
	cacheStr := fmt.Sprintf("%.3f%%", cacheHit)
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

	// Refresh usage cache
	usage := hookRefreshUsage(stateDir, claudeDir)

	rateStr := ""
	extraStr := ""
	pct5hr := -1
	pctWk := -1
	if usage != nil {
		pct5hr = forecast.IntOrDefault(usage["pct5hr"], -1)
		pctWk = forecast.IntOrDefault(usage["pctWeekly"], -1)
		if pct5hr >= 0 {
			rateStr = fmt.Sprintf("5hr [%s] %d%% | Weekly [%s] %d%%",
				progressBar(pct5hr, 20), pct5hr, progressBar(pctWk, 20), pctWk)
		}
		if eu, ok := usage["extra_usage"].(map[string]any); ok {
			enabled, _ := eu["is_enabled"].(bool)
			if enabled {
				used, _ := eu["used_credits"].(float64)
				lim, _ := eu["monthly_limit"].(float64)
				extraStr = fmt.Sprintf("Extra usage: ON ($%.2f/$%.2f)", used, lim)
			} else {
				extraStr = "Extra usage: OFF"
			}
		}
	}

	if usage != nil && pct5hr >= 0 {
		slog.Debug("display hook: recording snapshot", "pct5hr", pct5hr, "pctWk", pctWk)
		hookRecordSnapshot(stateDir, usage)
	}

	forecastStr := ""
	if usage != nil && pct5hr >= 0 {
		forecastStr = forecast.BuildForecast(stateDir, usage)
	}

	line1 := fmt.Sprintf("TELEMETRY: %d calls (agent:%d tool:%d chat:%d) | cache:%s",
		totalCalls, c.AgentCalls, c.ToolCalls, c.ChatCalls, cacheStr)
	lines := []string{line1}
	if toolStr != "" {
		lines = append(lines, "Tools: "+toolStr)
	}
	if rateStr != "" {
		lines = append(lines, "Rate limits: "+rateStr)
	}
	if forecastStr != "" {
		lines = append(lines, "Forecast: "+forecastStr)
	}
	if extraStr != "" {
		lines = append(lines, extraStr)
	}

	output := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": strings.Join(lines, "\n"),
		},
	}
	data, _ := json.Marshal(output)
	fmt.Print(string(data))
	slog.Debug("display hook: output sent")
}

// hookRefreshUsage returns current usage data, refreshing from the Anthropic API if the
// cache is stale (>30s).
func hookRefreshUsage(stateDir, claudeDir string) map[string]any {
	cachePath := filepath.Join(stateDir, "usage-api-cache.json")

	cached := readUsageCache(cachePath)
	if cached != nil {
		if fetched, ok := cached["fetched_at"].(float64); ok {
			age := time.Since(time.Unix(int64(fetched), 0))
			if age < 30*time.Second {
				slog.Debug("usage cache fresh", "age_s", int(age.Seconds()))
				return cached
			}
			slog.Debug("usage cache stale", "age_s", int(age.Seconds()))
		}
	}

	client, err := anthropic.NewClientFromDisk(claudeDir)
	if err != nil {
		slog.Warn("cannot create API client, using stale cache", "err", err)
		return cached
	}

	resp, err := client.FetchUsage()
	if err != nil {
		slog.Warn("API request failed, using stale cache", "err", err)
		return cached
	}

	slog.Debug("usage data fetched from API")
	usage := anthropic.TransformUsage(resp)

	result, _ := json.Marshal(usage)
	if err := os.WriteFile(cachePath, result, 0644); err != nil {
		slog.Error("failed to write usage cache", "err", err)
	}

	return usage
}

func readUsageCache(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	data = stripBOM(data)
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	return m
}

// hookRecordSnapshot appends a limit-history entry if >5min since the last one.
func hookRecordSnapshot(stateDir string, usage map[string]any) {
	histPath := filepath.Join(stateDir, "limit-history.jsonl")

	if f, err := os.Open(histPath); err == nil {
		fi, _ := f.Stat()
		offset := fi.Size() - 512
		if offset < 0 {
			offset = 0
		}
		f.Seek(offset, io.SeekStart)
		scanner := bufio.NewScanner(f)
		var lastLine string
		for scanner.Scan() {
			if l := strings.TrimSpace(scanner.Text()); l != "" {
				lastLine = l
			}
		}
		f.Close()

		if lastLine != "" {
			var entry map[string]any
			if json.Unmarshal([]byte(lastLine), &entry) == nil {
				if ts, ok := entry["ts"].(string); ok {
					if t, err := time.Parse(time.RFC3339, ts); err == nil {
						if time.Since(t) < 5*time.Minute {
							return
						}
					}
				}
			}
		}
	}

	snap := map[string]any{
		"ts":        time.Now().UTC().Format(time.RFC3339),
		"pct5hr":    forecast.IntOrDefault(usage["pct5hr"], -1),
		"pctWeekly": forecast.IntOrDefault(usage["pctWeekly"], -1),
	}
	if v, ok := usage["reset5hr"].(string); ok {
		snap["reset5hr"] = v
	}
	if v, ok := usage["resetWeekly"].(string); ok {
		snap["resetWeekly"] = v
	}

	line, _ := json.Marshal(snap)
	if f, err := os.OpenFile(histPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
		f.Write(append(line, '\n'))
		f.Close()
	}
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
