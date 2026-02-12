```
██████╗ ███████╗██████╗ ██╗███████╗ ██████╗ ██████╗ ██████╗ ███████╗
██╔══██╗██╔════╝██╔══██╗██║██╔════╝██╔════╝██╔═══██╗██╔══██╗██╔════╝
██████╔╝█████╗  ██████╔╝██║███████╗██║     ██║   ██║██████╔╝█████╗
██╔═══╝ ██╔══╝  ██╔══██╗██║╚════██║██║     ██║   ██║██╔═══╝ ██╔══╝
██║     ███████╗██║  ██║██║███████║╚██████╗╚██████╔╝██║     ███████╗
╚═╝     ╚══════╝╚═╝  ╚═╝╚═╝╚══════╝ ╚═════╝ ╚═════╝╚═╝     ╚══════╝
```

**Real-time telemetry for Claude Code.** Rate limits, cost tracking, burn rate intelligence, extra usage monitoring, and duty-cycle-aware pacing — injected directly into the AI's context before every prompt.

Vanilla Claude has zero awareness of your rate limits. PERISCOPE gives the AI a fuel gauge before takeoff.

---

## What It Does

PERISCOPE is a single Go binary that hooks into [Claude Code](https://docs.anthropic.com/en/docs/claude-code) to:

- **Track token usage** per session with per-model pricing (Opus, Sonnet, Haiku)
- **Monitor rate limits** via the Anthropic OAuth API (5hr, weekly, sonnet, opus windows)
- **Track extra usage** credits and monthly limits when enabled
- **Calculate burn rate** with duty-cycle-aware pacing that accounts for sleep/idle time
- **Project limit hits** before they happen with configurable alert thresholds
- **Inject telemetry into every prompt** so the AI knows its own resource state
- **Render a statusline** showing rate limits, cost, and pace directly in your terminal
- **Serve a real-time dashboard** with WebSocket push, themeable plugin widgets, and limit history charts

## Architecture

```
┌─────────────────────────────────────────────────────┐
│                   Claude Code                       │
│                                                     │
│  ┌──────────────┐  ┌──────────────┐  ┌───────────┐ │
│  │ UserPrompt   │  │ StopTurn     │  │ Statusline│ │
│  │ Submit Hook  │  │ Hook         │  │           │ │
│  └──────┬───────┘  └──────┬───────┘  └─────┬─────┘ │
└─────────┼──────────────────┼────────────────┼───────┘
          │                  │                │
          ▼                  ▼                ▼
┌─────────────────────────────────────────────────────┐
│              periscope (single Go binary)            │
│                                                      │
│  periscope hook display  │  periscope hook stop      │
│  Reads sidecar + API     │  Parses transcript,       │
│  cache, injects          │  computes cost, writes    │
│  telemetry as system-    │  per-session sidecar      │
│  reminder                │                           │
│──────────────────────────┤──────────────────────────│
│  periscope statusline    │  periscope serve          │
│  Reads sidecar + API     │  HTTP + WebSocket server  │
│  cache, renders ANSI     │  Plugin runtime, themes,  │
│  powerline segments      │  widgets, live data push  │
└──────────────────────────┴──────────────────────────┘
          │
          ▼
┌─────────────────────────────────────────────────────┐
│              ~/.claude/hooks/cost-state/             │
│                                                     │
│  {session-id}.json    Sidecar (per-session)         │
│  usage-history.jsonl  Cross-session log             │
│  limit-history.jsonl  Rate limit snapshots          │
│  usage-api-cache.json OAuth API cache (30s TTL)     │
│  profile-cache.json   Account + extra usage cache   │
└─────────────────────────────────────────────────────┘
          │
          ▼
┌─────────────────────────────────────────────────────┐
│  periscope serve  →  localhost:8384                  │
│  Plugin runtime with themeable widgets               │
│  WebSocket push for live rate limit updates          │
└─────────────────────────────────────────────────────┘
```

## Setup

### Prerequisites

- [Claude Code](https://docs.anthropic.com/en/docs/claude-code) CLI
- OAuth token with `user:profile` scope (auto-provisioned by Claude Code login)

### Installation

1. **Build the binary:**
   ```bash
   git clone https://github.com/ProgenyAlpha/periscope.git
   cd periscope
   go build .
   ```

2. **Initialize plugins and config:**
   ```bash
   periscope init
   ```
   This creates `~/.periscope/` with themes, widgets, and a default config.

3. **Register hooks in `~/.claude/settings.json`:**
   ```json
   {
     "hooks": {
       "StopTurn": [{"matcher": "", "hooks": [{"type": "command", "command": "periscope hook stop", "timeout": 10}]}],
       "UserPromptSubmit": [{"matcher": "", "hooks": [{"type": "command", "command": "periscope hook display", "timeout": 5}]}]
     },
     "statusLine": {"type": "command", "command": "periscope statusline"}
   }
   ```
   Replace `periscope` with the full path to the binary if it's not on your PATH.

4. **Start the dashboard:**
   ```bash
   periscope serve
   ```
   Open `http://localhost:8384` in your browser.

### Commands

| Command | Purpose |
|---------|---------|
| `periscope init` | Set up plugins, database, and hooks |
| `periscope serve` | Start the dashboard server |
| `periscope status` | Check if server is running |
| `periscope hook stop` | Process transcript (StopTurn hook) |
| `periscope hook display` | Output telemetry context (UserPromptSubmit hook) |
| `periscope statusline` | Render terminal statusline (reads JSON from stdin) |
| `periscope uninstall` | Remove hooks and clean up |
| `periscope version` | Print version |

### Repo Structure

```
periscope/
├── main.go                 # Subcommand router + app initialization
├── store.go                # Database, file import, API clients
├── hooks.go                # Hook implementations (stop, display)
├── statusline.go           # Terminal statusline renderer
├── server.go               # HTTP server, WebSocket hub
├── installer.go            # Installation + hook registration
├── watcher.go              # File watcher for live reload
├── embed.go                # Embedded asset manifest
├── defaults/
│   ├── themes/             # Theme TOMLs (colors + terminal ANSI)
│   ├── widgets/            # HTML widget panels
│   ├── pricing/            # Model pricing data
│   ├── forecasters/        # Forecast algorithm configs
│   └── runtime.html        # Plugin runtime shell
├── statusline/
│   └── statusline-config.json  # Segment order and thresholds
└── legacy/                 # Original PowerShell implementation (reference)
    ├── hooks/
    ├── dashboard/
    └── statusline/
```

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

### Rate Limit Windows

| Window | Field | Description |
|--------|-------|-------------|
| 5-hour | `pct5hr` | Rolling 5-hour token budget utilization |
| Weekly | `pctWeekly` | 7-day overall limit |
| Sonnet | `pctSonnet` | 7-day Sonnet-specific limit |
| Opus | `pctOpus` | 7-day Opus-specific limit (when available) |
| OAuth Apps | `pctOauthApps` | 7-day OAuth app limit (when available) |
| Cowork | `pctCowork` | 7-day cowork limit (when available) |

### Extra Usage

When enabled on your account, periscope tracks:
- `is_enabled` — whether extra usage is active
- `monthly_limit` — your spending cap (e.g. $50)
- `used_credits` — credits consumed this month
- `utilization` — percentage of monthly limit used

The display hook shows this as: `Extra usage: ON ($0.00/$50.00)`

### Statusline Segments

14 configurable segments rendered in your terminal:

| Segment | Shows | Priority |
|---------|-------|----------|
| `dir` | Current directory | 1 |
| `git` | Branch + dirty state | 2 |
| `model` | Active model (opus/sonnet/haiku) | 3 |
| `turns` | Turn count this session | 4 |
| `rate-5hr` | 5-hour rate limit % | 5 |
| `rate-weekly` | Weekly rate limit % | 5 |
| `rate-sonnet` | Sonnet rate limit % | 5 |
| `cost` | Session cost in USD | 6 |
| `reset` | Time until nearest limit reset | 6 |
| `proj` | Projected 5hr utilization at current pace | 6 |
| `cache` | Cache hit rate % | 7 |
| `tools` | Tools used in last turn | 8 |
| `context` | Context window usage bar | 8 |
| `vim` | Vim mode indicator | 9 |

Segments are filtered by terminal width using priority thresholds. Configure in `statusline-config.json`.

Three render styles: **powerline** (ANSI backgrounds + arrow separators), **plain** (pipe-separated), **minimal** (space-separated).

Themes are loaded from `~/.periscope/plugins/themes/*.toml` — each theme has a `[terminal]` section with ANSI 256-color values.

### Themes

Each theme TOML has three sections:
- `[colors]` — CSS hex values for the dashboard
- `[terminal]` — ANSI 256-color codes for the statusline
- `[brand]` — Brand accent colors

Available themes: catppuccin-mocha, tactical, arctic, ghost, midnight, phosphor, starfield-dark, starfield-light, thermal.

## How It Works

### The Injection Loop

Every time you send a message to Claude Code:

1. `UserPromptSubmit` hook fires → `periscope hook display` runs
2. Reads the latest session sidecar for cost/token data
3. Reads cached rate limits from `usage-api-cache.json`
4. Injects telemetry as a `<system-reminder>` block into the prompt
5. Claude sees its own rate limits before reading your message

After Claude responds:

6. `StopTurn` hook fires → `periscope hook stop` runs
7. Reads new entries from the session transcript (incremental seek)
8. Computes per-turn cost using model-specific pricing
9. Updates the session sidecar with cumulative totals
10. Appends a snapshot to `usage-history.jsonl`

Meanwhile, `periscope serve` fetches fresh rate limits from the Anthropic OAuth API every 30 seconds and pushes updates via WebSocket to the dashboard.

### Cache Hit Rate

```
cache_hit_rate = cache_read / (input + cache_read)
```

Cache writes are excluded — they represent the cost of building the cache, not utilization.

## FAQ

**Q: Does this slow down Claude Code?**
A: The hooks are compiled Go — sub-100ms execution. The display hook has a 5-second timeout. Rate data is read from a local cache file, not fetched live.

**Q: Does this work on Mac/Linux?**
A: Yes. The Go binary is cross-platform. Build with `GOOS=linux go build .` or `GOOS=darwin go build .`.

**Q: Where does the OAuth token come from?**
A: Claude Code stores it at `~/.claude/.credentials.json` after you log in. PERISCOPE reads it — no additional auth setup needed.

**Q: What if the API is down?**
A: All data paths have graceful fallbacks. A failed API call means stale cached data is shown. Claude Code itself is unaffected.

---

Built by overengineering a token counter until it became a submarine.
