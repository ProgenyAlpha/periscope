```
██████╗ ███████╗██████╗ ██╗███████╗ ██████╗ ██████╗ ██████╗ ███████╗
██╔══██╗██╔════╝██╔══██╗██║██╔════╝██╔════╝██╔═══██╗██╔══██╗██╔════╝
██████╔╝█████╗  ██████╔╝██║███████╗██║     ██║   ██║██████╔╝█████╗
██╔═══╝ ██╔══╝  ██╔══██╗██║╚════██║██║     ██║   ██║██╔═══╝ ██╔══╝
██║     ███████╗██║  ██║██║███████║╚██████╗╚██████╔╝██║     ███████╗
╚═╝     ╚══════╝╚═╝  ╚═╝╚═╝╚══════╝ ╚═════╝ ╚═════╝╚═╝     ╚══════╝
```

**Real-time telemetry for Claude Code.** Rate limits, cost tracking, burn rate intelligence, and duty-cycle-aware pacing — injected directly into the AI's context before every prompt.

Vanilla Claude has zero awareness of your rate limits. PERISCOPE gives the AI a fuel gauge before takeoff.

---

## What It Does

PERISCOPE is a hook-based telemetry system for [Claude Code](https://docs.anthropic.com/en/docs/claude-code) that:

- **Tracks token usage** per session with per-model pricing (Opus, Sonnet, Haiku)
- **Monitors rate limits** via the Anthropic OAuth API (real data, not estimates)
- **Calculates burn rate** with duty-cycle-aware pacing that accounts for sleep/idle time
- **Projects limit hits** before they happen with configurable alert thresholds
- **Injects telemetry into every prompt** so the AI knows its own resource state
- **Renders a statusline** showing rate limits, cost, and pace directly in your terminal

```
                 ┃o┃
                 ┃ ┃
    ▄▄▄▄▄▄▄▄▄▄▄▄▄┃ ┃▄▄▄▄▄▄▄▄▄▄▄▄▄
    █ PERISCOPE ░░░░░ TELEMETRY █
    ▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀
```

## Architecture

```
┌─────────────────────────────────────────────────────┐
│                   Claude Code                       │
│                                                     │
│  ┌──────────────┐  ┌──────────────┐  ┌───────────┐ │
│  │ UserPrompt   │  │ Stop Hook    │  │ Statusline│ │
│  │ Submit Hook  │  │              │  │           │ │
│  └──────┬───────┘  └──────┬───────┘  └─────┬─────┘ │
└─────────┼──────────────────┼────────────────┼───────┘
          │                  │                │
          ▼                  ▼                ▼
  ┌───────────────┐  ┌──────────────┐  ┌───────────────┐
  │ cost-tracker  │  │ cost-tracker │  │ statusline.ps1│
  │ -display.ps1  │  │ -stop.ps1    │  │               │
  │               │  │              │  │ Reads sidecar │
  │ Calls OAuth   │  │ Parses JSONL │  │ + API cache   │
  │ API, injects  │  │ transcript,  │  │ Renders rate  │
  │ telemetry as  │  │ writes per-  │  │ segments in   │
  │ system-       │  │ session      │  │ terminal      │
  │ reminder      │  │ sidecar JSON │  │               │
  └───────┬───────┘  └──────┬───────┘  └───────┬───────┘
          │                  │                  │
          ▼                  ▼                  ▼
  ┌─────────────────────────────────────────────────┐
  │              ~/.claude/hooks/cost-state/         │
  │                                                 │
  │  {session-id}.json    Sidecar (per-session)     │
  │  usage-history.jsonl  Cross-session log         │
  │  limit-history.jsonl  Rate limit snapshots      │
  │  usage-api-cache.json OAuth API cache (30s TTL) │
  │  profile-cache.json   Account tier cache        │
  └─────────────────────────────────────────────────┘
          │
          ▼
  ┌─────────────────────────────────────────────────┐
  │  telemetry-dashboard.ps1  →  localhost:8384     │
  │  telemetry-dashboard.html    Single-file SPA    │
  └─────────────────────────────────────────────────┘
```

## Components

### Hooks

| File | Hook Event | Purpose |
|------|-----------|---------|
| `cost-tracker-display.ps1` | `UserPromptSubmit` | Calls Anthropic OAuth API, caches response, injects rate limits + cost + forecast into the prompt as a `<system-reminder>` |
| `cost-tracker-stop.ps1` | `Stop` | Parses the session transcript JSONL, computes per-turn token usage and cost with model-specific pricing, writes cumulative stats to the session sidecar |

### Statusline

| File | Purpose |
|------|---------|
| `statusline/statusline.ps1` | Renders rate limit gauges, cost, pace, and reset timers in the Claude Code terminal statusline |
| `statusline/statusline-config.json` | Configures which segments to show, order, and thresholds |

### Dashboard

| File | Purpose |
|------|---------|
| `cost-state/telemetry-dashboard.ps1` | PowerShell HTTP server on `:8384`. Aggregates all sidecars + history files into a single JSON payload. Hot-reloads the HTML on every request. |
| `cost-state/telemetry-dashboard.html` | Single-file SPA (CSS + HTML + JS). All filtering/charting is client-side. Submarine command bridge aesthetic. |

### Data Files

| File | Format | Lifecycle |
|------|--------|-----------|
| `{session-id}.json` | JSON | Created per session, updated every turn |
| `usage-history.jsonl` | JSONL | Append-only, compacted weekly (>7 days pruned) |
| `limit-history.jsonl` | JSONL | Rate limit snapshots from OAuth API, compacted weekly |
| `usage-api-cache.json` | JSON | 30-second TTL cache for OAuth API responses |

## Key Concepts

### Token Weights (Rate Limit)

```
input:        1x    (counts toward ITPM)
cache_read:   0x    (FREE — excluded from ITPM on modern models)
cache_write:  1x    (counts toward ITPM)
output:       5x    (OTPM limits ~5x tighter than ITPM)
```

### Model Pricing ($ per million tokens)

| Model | Input | Cache Read | Cache Write | Output |
|-------|-------|------------|-------------|--------|
| Opus 4.6 / 4.5 | $5.00 | $0.50 | $6.25 | $25.00 |
| Sonnet 4.5 | $3.00 | $0.30 | $3.75 | $15.00 |
| Haiku 4.5 | $1.00 | $0.10 | $1.25 | $5.00 |

### Duty Cycle

PERISCOPE analyzes `usage-history.jsonl` to detect your active hours per day (excluding the current incomplete day). This prevents naive burn rate projections that assume 24/7 usage.

Example: If you work 10 hours/day, a weekly projection that shows "OVER LIMIT" based on calendar hours might actually be fine when adjusted for your duty cycle.

### Pace Calculation

```
sustainable_rate = remaining_budget / remaining_active_hours
pace = actual_active_rate / sustainable_rate

≤ 85%   → ON PACE    (green)
85-99%  → TIGHT      (amber)
≥ 100%  → OVER LIMIT (red)
```

### Heavy Burn Detection

Triggers a fire-animated overlay on the rate limit chart when:
- **Session**: 5hr burn rate exceeds 15%/h
- **Weekly**: Projected weekly usage exceeds 100%
- Scope shown: `[SESSION]`, `[WEEKLY]`, or `[SESSION + WEEKLY]`

## Dashboard Panels

The dashboard at `localhost:8384` shows:

1. **Rate Limit Intelligence** — Chart with 5hr/weekly utilization over time, projection lines, window boundaries, and a 3-column intel grid:
   - **Session** — Current 5hr pace with LCD readout (ON PACE / TIGHT / OVER LIMIT)
   - **Weekly** — Weekly pace with duty-cycle-aware projections
   - **Capacity** — Estimated token limits and cross-window capacity trends

2. **Key Metrics** — Session cost, turns, cache hit rate, tokens in/out

3. **Usage Timeline** — Stacked bar chart of token flow over time with range controls

4. **Deep Dive Panels** — Token breakdown, tool usage, cost by category, cache efficiency, activity heatmap, session log

## Setup

### Prerequisites

- [Claude Code](https://docs.anthropic.com/en/docs/claude-code) CLI
- PowerShell 5.1+ (Windows) or PowerShell Core (cross-platform)
- OAuth token with `user:profile` scope (auto-provisioned by Claude Code login)

### Installation

1. **Clone the repo:**
   ```bash
   git clone https://github.com/ProgenyAlpha/periscope.git
   cd periscope
   ```

2. **Copy files into your Claude Code config:**
   ```powershell
   # Hooks (token tracking + telemetry injection)
   Copy-Item hooks\cost-tracker-display.ps1 ~/.claude/hooks/
   Copy-Item hooks\cost-tracker-stop.ps1    ~/.claude/hooks/

   # Dashboard (web UI + server)
   New-Item -ItemType Directory -Force ~/.claude/hooks/cost-state/
   Copy-Item dashboard\telemetry-dashboard.html ~/.claude/hooks/cost-state/
   Copy-Item dashboard\telemetry-dashboard.ps1  ~/.claude/hooks/cost-state/

   # Statusline (terminal rate display)
   New-Item -ItemType Directory -Force ~/.claude/statusline/
   Copy-Item statusline\statusline.ps1         ~/.claude/statusline/
   Copy-Item statusline\statusline-config.json ~/.claude/statusline/
   ```

3. **Register hooks in `~/.claude/settings.json`:**
   ```json
   {
     "hooks": {
       "UserPromptSubmit": [{
         "matcher": "",
         "hooks": [{
           "type": "command",
           "command": "powershell.exe -NoProfile -ExecutionPolicy Bypass -File \"~/.claude/hooks/cost-tracker-display.ps1\"",
           "timeout": 5
         }]
       }],
       "Stop": [{
         "matcher": "",
         "hooks": [{
           "type": "command",
           "command": "powershell.exe -NoProfile -ExecutionPolicy Bypass -File \"~/.claude/hooks/cost-tracker-stop.ps1\"",
           "timeout": 10
         }]
       }]
     },
     "statusLine": {
       "type": "command",
       "command": "powershell.exe -NoProfile -ExecutionPolicy Bypass -File \"~/.claude/statusline/statusline.ps1\""
     }
   }
   ```

4. **Start the dashboard:**
   ```powershell
   powershell -File ~/.claude/hooks/cost-state/telemetry-dashboard.ps1
   ```
   Open `http://localhost:8384` in your browser.

### Repo Structure

```
periscope/
├── hooks/
│   ├── cost-tracker-display.ps1   # UserPromptSubmit hook — injects telemetry
│   └── cost-tracker-stop.ps1      # Stop hook — tracks token usage per turn
├── dashboard/
│   ├── telemetry-dashboard.ps1    # HTTP server on :8384
│   └── telemetry-dashboard.html   # Single-file SPA (CSS + HTML + JS)
├── statusline/
│   ├── statusline.ps1             # Terminal statusline renderer
│   └── statusline-config.json     # Segment order and thresholds
└── README.md
```

## How It Works

### The Injection Loop

Every time you send a message to Claude Code:

1. `UserPromptSubmit` hook fires → `cost-tracker-display.ps1` runs
2. Script reads OAuth token from `~/.claude/.credentials.json`
3. Calls `https://api.anthropic.com/api/oauth/usage` for real utilization percentages
4. Reads the latest session sidecar for cost/token data
5. Computes burn rates, projections, pace
6. Injects everything as a `<system-reminder>` block into the prompt
7. Claude sees its own rate limits before reading your message

After Claude responds:

8. `Stop` hook fires → `cost-tracker-stop.ps1` runs
9. Script reads new entries from the session transcript JSONL (incremental — seeks to last offset)
10. Computes per-turn cost using model-specific pricing
11. Updates the session sidecar with cumulative totals
12. Appends a snapshot to `usage-history.jsonl`

### Cache Hit Rate

```
cache_hit_rate = cache_read / (input + cache_read)
```

Cache writes are excluded from both numerator and denominator — they represent the cost of building the cache, not cache utilization.

## FAQ

**Q: Does this slow down Claude Code?**
A: The display hook has a 5-second timeout and typically completes in <1s. The OAuth API response is cached for 30 seconds.

**Q: Does this work on Mac/Linux?**
A: The PowerShell scripts use `$env:USERPROFILE` and Windows paths. Porting to bash/zsh would require rewriting the hooks but the architecture is the same.

**Q: Where does the OAuth token come from?**
A: Claude Code stores it at `~/.claude/.credentials.json` after you log in. PERISCOPE reads it — no additional auth setup needed.

**Q: What if the API is down?**
A: Every script has `$ErrorActionPreference = 'SilentlyContinue'` and graceful fallbacks. A failed API call means the statusline shows stale cached data. Claude Code itself is unaffected.

---

Built by overengineering a token counter until it became a submarine.
