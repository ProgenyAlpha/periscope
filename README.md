```
██████╗ ███████╗██████╗ ██╗███████╗ ██████╗ ██████╗ ██████╗ ███████╗
██╔══██╗██╔════╝██╔══██╗██║██╔════╝██╔════╝██╔═══██╗██╔══██╗██╔════╝
██████╔╝█████╗  ██████╔╝██║███████╗██║     ██║   ██║██████╔╝█████╗
██╔═══╝ ██╔══╝  ██╔══██╗██║╚════██║██║     ██║   ██║██╔═══╝ ██╔══╝
██║     ███████╗██║  ██║██║███████║╚██████╗╚██████╔╝██║     ███████╗
╚═╝     ╚══════╝╚═╝  ╚═╝╚═╝╚══════╝ ╚═════╝ ╚═════╝╚═╝     ╚══════╝
```

**Real-time telemetry for Claude Code.** Rate limits, cost tracking, burn rate forecasting, push notifications, and context injection — all from a single Go binary.

Claude Code has zero awareness of your rate limits. Periscope gives the AI a fuel gauge before every takeoff.

---

## Features

- **Context injection** — Compact telemetry line injected into every prompt so the AI knows its own resource state
- **Cost tracking** — Per-session, per-turn, per-model pricing with tool-level attribution
- **Rate limit monitoring** — 5-hour, weekly, Sonnet, Opus, OAuth, and cowork windows via Anthropic's OAuth API
- **Burn rate forecasting** — Duty-cycle-aware projections that stay stable across sleep/wake cycles
- **Push notifications** — Browser alerts at 80% and 90% rate limit thresholds, plus reset notifications
- **Phantom usage detection** — Identifies non-CLI usage (claude.ai, mobile) eating into your rate limits
- **Terminal statusline** — 14 configurable segments with powerline/plain/minimal styles and 9 themes
- **Real-time dashboard** — WebSocket-powered PWA with 14 drag-and-drop widgets and persistent layouts
- **Multi-agent tracking** — Team session monitoring with per-agent cost and status
- **Session titles** — Auto-generated via Haiku after 5 turns
- **Extra usage tracking** — Monthly credit monitoring when extra usage is enabled

## Quick Start

### Install

**Linux / macOS:**
```bash
curl -fsSL https://raw.githubusercontent.com/ProgenyAlpha/periscope/master/install.sh | sh
```

**Windows (PowerShell):**
```powershell
irm https://raw.githubusercontent.com/ProgenyAlpha/periscope/master/install.ps1 | iex
```

**From source:**
```bash
git clone https://github.com/ProgenyAlpha/periscope.git
cd periscope && go build .
```

### Set Up

```bash
periscope init
```

This creates `~/.periscope/` with themes, widgets, config, and database. It prints the hook configuration to add to `~/.claude/settings.json`:

```json
{
  "hooks": {
    "StopTurn": [{"matcher": "", "hooks": [{"type": "command", "command": "periscope hook stop", "timeout": 10}]}],
    "UserPromptSubmit": [{"matcher": "", "hooks": [{"type": "command", "command": "periscope hook display", "timeout": 5}]}]
  },
  "statusLine": {"type": "command", "command": "periscope statusline"}
}
```

### Launch

```bash
periscope serve
```

Dashboard at `http://localhost:8384`. Uses your existing Claude Code OAuth token — no additional auth setup.

## Commands

| Command | Purpose |
|---------|---------|
| `periscope init` | Create config, extract plugins, register hooks |
| `periscope serve` | Start dashboard server (default `127.0.0.1:8384`) |
| `periscope status` | Health check against running server |
| `periscope hook stop` | Process transcript after each turn (StopTurn) |
| `periscope hook display` | Inject telemetry into prompt (UserPromptSubmit) |
| `periscope statusline` | Render terminal statusline from stdin |
| `periscope uninstall` | Remove hooks, optionally delete `~/.periscope` |
| `periscope version` | Print version |

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                     Claude Code                          │
│                                                          │
│  UserPromptSubmit ──► periscope hook display              │
│  StopTurn ─────────► periscope hook stop                  │
│  Statusline ───────► periscope statusline                 │
└────────────┬─────────────────┬───────────────────────────┘
             │                 │
             ▼                 ▼
┌────────────────────┐  ┌──────────────────────────────────┐
│  Sidecar Files     │  │  periscope serve                  │
│                    │  │                                    │
│  {session}.json    │  │  HTTP API + WebSocket              │
│  usage-history     │  │  14 widgets, 9 themes              │
│  limit-history     │  │  PWA + push notifications          │
│  usage-api-cache   │  │  Live data every 30s               │
│  profile-cache     │  │  SQLite + file watcher             │
│                    │  │                                    │
│  ~/.claude/hooks/  │  │  http://localhost:8384              │
│  cost-state/       │  │                                    │
└────────────────────┘  └──────────────────────────────────┘
```

## Context Injection

Every prompt, Periscope injects a compact telemetry line into Claude's context as a `<system-reminder>`:

```
TELEMETRY: T:47(a12 t30 c5) cache:97.3% | 5h:40% wk:20% | 5h:40%→62%(3.2h@8.0%/h) | wk:20%→35%(4.3d@0.1%/ah) | EU:$2.50/$100 | Bash:8x Read:5x Grep:3x
```

| Field | Meaning |
|-------|---------|
| `T:47(a12 t30 c5)` | 47 total calls: 12 agent, 30 tool, 5 chat |
| `cache:97.3%` | Cache hit rate |
| `5h:40% wk:20%` | Current rate limit utilization |
| `5h:40%→62%(3.2h@8.0%/h)` | Forecast: will hit 62% in 3.2h at 8%/h burn rate |
| `EU:$2.50/$100` | Extra usage credits spent / monthly limit |
| `Bash:8x Read:5x` | Top tools by weighted token usage |

When usage is elevated (5hr >70% or weekly >50%), an advisory is appended prompting the AI to offer lighter alternatives before starting token-heavy operations.

### The Loop

1. You send a message
2. `UserPromptSubmit` fires → `periscope hook display` reads sidecar + cached rate limits → injects telemetry
3. Claude processes your message with telemetry in context
4. `StopTurn` fires → `periscope hook stop` reads new transcript entries → computes cost → updates sidecar
5. Dashboard picks up changes via file watcher → broadcasts to WebSocket clients

## Statusline

14 segments across two rows, rendered with ANSI color in your terminal:

**Row 1 (work info):** directory, git branch, model, turn count, session cost, last tools

**Row 2 (resource info):** 5hr limit %, weekly limit %, Sonnet limit %, time to reset, projected utilization, cache hit %, context window usage

Three render styles: `powerline` (backgrounds + arrow separators), `plain` (pipe-separated), `minimal` (spaces only).

Segments auto-truncate by priority when the terminal is narrow. Configure segment order, visibility, and row assignment in `~/.periscope/statusline-config.json`.

## Dashboard

A PWA served at `localhost:8384` with WebSocket push updates every 30 seconds.

### Widgets (14)

| Widget | What it shows |
|--------|---------------|
| Rate Limits | Gauge rings for all rate limit windows |
| Limit Timeline | Rate limit history chart with zone highlighting |
| Cost Overview | Session cost summary with turn breakdown |
| Cost Timeline | Cost over time chart |
| Usage Timeline | Token usage over time with filtering |
| Token Details | Input/output/cache breakdown |
| Cache Savings | Cache hit rate and estimated savings |
| Tool Usage | Per-tool call counts and rankings |
| Model Usage | Per-model token distribution |
| Session List | Session browser with titles, costs, projects |
| Activity Breakdown | Activity type classification |
| Agent Tracker | Multi-agent team monitoring |
| Last Turn | Details of the most recent turn |
| Statusline Settings | Configure statusline from the dashboard |

Widgets use GridStack for drag-and-drop layout. Positions persist across restarts.

### Themes (9)

catppuccin-mocha (default), tactical, arctic, ghost, midnight, phosphor, starfield-dark, starfield-light, thermal.

Each theme defines CSS variables for the dashboard and ANSI 256-color values for the statusline. Add custom themes as `.toml` files in `~/.periscope/plugins/themes/`.

## Rate Limits & Forecasting

### Windows Tracked

| Window | Description |
|--------|-------------|
| 5-hour | Rolling 5-hour token budget |
| Weekly | 7-day overall limit |
| Sonnet | 7-day Sonnet-specific limit |
| Opus | 7-day Opus-specific limit |
| OAuth Apps | 7-day OAuth app limit |
| Cowork | 7-day cowork limit |

### Forecasting

**5-hour:** Blends current 30-minute burn rate (60%) with session average (40%). Detects window resets automatically.

**Weekly:** Duty-cycle-adjusted. Calculates active hours per day from usage patterns, projects burn rate only across expected active hours. Stable regardless of sleep schedules.

**Verdicts:** >100% = `OVER LIMIT`, >90% = `SLOW DOWN`, >70% = `monitor`

### Push Notifications

Browser push alerts (no app required — works in any browser that supports Web Push):

- 80% of 5hr limit (warning)
- 90% of 5hr limit (critical)
- Rate limit window reset
- Server health changes

30-minute cooldown between repeated alerts. VAPID keys auto-generated on first use.

### Phantom Usage Detection

Detects rate limit growth during periods with no CLI activity. If your weekly limit increases while you're not using Claude Code, something else is — claude.ai, mobile, API calls. Periscope estimates the phantom cost and surfaces it in the dashboard.

## Pricing

Per-model cost tracking with live pricing updates from LiteLLM (24h cache).

| Model | Input | Cache Read | Cache Write | Output |
|-------|-------|------------|-------------|--------|
| Opus 4.6 / 4.5 | $5.00/M | $0.50/M | $6.25/M | $25.00/M |
| Sonnet 4.5 | $3.00/M | $0.30/M | $3.75/M | $15.00/M |
| Haiku 4.5 | $1.00/M | $0.10/M | $1.25/M | $5.00/M |

Token weights for rate limit calculation: input 1x, cache_read 0x (free), cache_write 1x, output 5x.

## Configuration

`~/.periscope/config.toml`:

```toml
[server]
host = "127.0.0.1"   # Bind address (warns on 0.0.0.0)
port = 8384           # Dashboard port
token = ""            # Bearer token for API auth (optional)

data_dir = ""         # Override sidecar directory (default: ~/.claude/hooks/cost-state)
```

The `PERISCOPE_TOKEN` environment variable overrides `config.toml` token.

## Project Structure

```
periscope/
├── main.go              # Subcommand router, app struct, config
├── hooks.go             # StopTurn + UserPromptSubmit hook handlers
├── statusline.go        # Terminal statusline renderer (14 segments)
├── server.go            # HTTP API, WebSocket hub, background polling
├── installer.go         # periscope init/uninstall
├── watcher.go           # fsnotify file watcher for live reload
├── push.go              # Web push notification system
├── embed.go             # go:embed for bundled assets
├── compat.go            # Adapter utilities
├── internal/
│   ├── analytics/       # Phantom usage detection
│   ├── anthropic/       # OAuth API client + session title generation
│   ├── forecast/        # Burn rate forecasting engine
│   ├── pricing/         # Model pricing (hardcoded + LiteLLM live)
│   └── store/           # SQLite database, file import pipeline
├── defaults/
│   ├── themes/          # 9 theme TOMLs
│   ├── widgets/         # 14 HTML widget panels
│   ├── pricing/         # Model pricing data
│   ├── forecasters/     # Forecast algorithm config
│   ├── vendor/          # GridStack library
│   ├── static/          # PWA icon + manifest + service worker
│   └── runtime.html     # Dashboard shell
└── .github/workflows/   # CI (vet + test + cross-compile) + Release
```

## FAQ

**Does this slow down Claude Code?**
Hooks are compiled Go — sub-100ms execution. Rate data is read from local cache, not fetched live. The display hook has a 5-second timeout.

**What platforms are supported?**
Windows (amd64), Linux (amd64, arm64), macOS (amd64, arm64). Pre-built binaries for all five targets on every release.

**Where does the OAuth token come from?**
Claude Code stores it at `~/.claude/.credentials.json` after login. Periscope reads it directly — no additional auth setup. Rate limit tracking requires `claude login`; everything else works without it.

**What if the API is down?**
All data paths have graceful fallbacks. A failed API call means stale cached data is shown, with exponential backoff (max 10 minutes). Claude Code itself is never affected.

**Can I run this on a remote server?**
Yes, but bind to `127.0.0.1` and use SSH tunneling or a reverse proxy. Setting `token` in config adds bearer auth. Periscope warns if you bind to `0.0.0.0`.

**How much disk does it use?**
The SQLite database and history files are compact. Limit history auto-compacts: full resolution for 24h, 5-minute intervals for 7 days, 60-minute for 30 days, 4-hour beyond that.

---

Built by overengineering a token counter until it became a submarine.
