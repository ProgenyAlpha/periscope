# telemetry-dashboard.ps1 — Browser-based telemetry dashboard with live config editing
# Usage: powershell -File telemetry-dashboard.ps1
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$stateDir = "$env:USERPROFILE\.claude\hooks\cost-state"
$statuslineDir = "$env:USERPROFILE\.claude\statusline"
$configPath = Join-Path $stateDir "usage-config.json"
$historyPath = Join-Path $stateDir "usage-history.jsonl"
$slConfigPath = Join-Path $statuslineDir "statusline-config.json"
$limitHistPath = Join-Path $stateDir "limit-history.jsonl"

# ══════════════════════════════════════════════════════════════════════════════
# DATA COLLECTION
# ══════════════════════════════════════════════════════════════════════════════
function Build-DashboardData {
    $data = @{
        generatedAt = [System.DateTimeOffset]::UtcNow.ToString('o')
        usageConfig = $null
        statuslineConfig = $null
        sessions = @()
        history = @()
        sidecars = @()
        liveUsage = $null
        profile = $null
        sessionMeta = @{}
        limitHistory = @()
    }

    # Load usage-config.json
    if (Test-Path $configPath) {
        try { $data.usageConfig = Get-Content $configPath -Raw | ConvertFrom-Json -ErrorAction Stop }
        catch {}
    }

    # Load statusline-config.json
    if (Test-Path $slConfigPath) {
        try { $data.statuslineConfig = Get-Content $slConfigPath -Raw | ConvertFrom-Json -ErrorAction Stop }
        catch {}
    }

    # Load usage-history.jsonl
    if (Test-Path $historyPath) {
        $lines = @()
        foreach ($line in [System.IO.File]::ReadLines($historyPath)) {
            if ($line.Trim().Length -eq 0) { continue }
            try {
                $entry = $line | ConvertFrom-Json -ErrorAction Stop
                $lines += $entry
            } catch { continue }
        }
        $data.history = $lines
    }

    # Load all session sidecars
    $sidecarFiles = Get-ChildItem $stateDir -Filter "*.json" -ErrorAction SilentlyContinue |
        Where-Object { $_.Name -ne 'usage-config.json' -and $_.Name -ne 'usage-api-cache.json' -and $_.Name -ne 'profile-cache.json' }
    foreach ($f in $sidecarFiles) {
        try {
            $sc = Get-Content $f.FullName -Raw | ConvertFrom-Json -ErrorAction Stop
            $sid = $f.BaseName
            $data.sidecars += @{ id = $sid; data = $sc }
        } catch { continue }
    }

    # Load live usage from API cache (populated by statusline/display hook)
    $usageCachePath = Join-Path $stateDir "usage-api-cache.json"
    if (Test-Path $usageCachePath) {
        try {
            $cache = Get-Content $usageCachePath -Raw | ConvertFrom-Json -ErrorAction Stop
            $data.liveUsage = $cache
        } catch {}
    }

    # Load profile from API (cached locally for dashboard lifetime)
    $profileCachePath = Join-Path $stateDir "profile-cache.json"
    $profileStale = $true
    if (Test-Path $profileCachePath) {
        try {
            $pc = Get-Content $profileCachePath -Raw | ConvertFrom-Json -ErrorAction Stop
            $age = ([DateTimeOffset]::UtcNow.ToUnixTimeSeconds()) - $pc.fetched_at
            if ($age -lt 300) { $data.profile = $pc; $profileStale = $false }
        } catch {}
    }
    if ($profileStale) {
        $credsPath = "$env:USERPROFILE\.claude\.credentials.json"
        if (Test-Path $credsPath) {
            try {
                $creds = Get-Content $credsPath -Raw | ConvertFrom-Json -ErrorAction Stop
                $token = $creds.claudeAiOauth.accessToken
                if ($token) {
                    $headers = @{
                        "Authorization" = "Bearer $token"
                        "User-Agent" = "periscope-telemetry"
                        "Accept" = "application/json"
                        "anthropic-version" = "2023-06-01"
                        "anthropic-beta" = "oauth-2025-04-20"
                    }
                    $prof = Invoke-RestMethod -Uri "https://api.anthropic.com/api/oauth/profile" -Headers $headers -Method Get -TimeoutSec 5
                    $profObj = @{
                        name = $prof.account.full_name
                        email = $prof.account.email
                        subscription = $prof.organization.organization_type
                        tier = $prof.organization.rate_limit_tier
                        org = $prof.organization.name
                        status = $prof.organization.subscription_status
                        fetched_at = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
                    }
                    $profObj | ConvertTo-Json | Set-Content $profileCachePath -Force
                    $data.profile = $profObj
                }
            } catch {}
        }
    }

    # Load session metadata from all sessions-index.json files
    $projectsDir = "$env:USERPROFILE\.claude\projects"
    if (Test-Path $projectsDir) {
        foreach ($projDir in Get-ChildItem $projectsDir -Directory -ErrorAction SilentlyContinue) {
            $idxPath = Join-Path $projDir.FullName "sessions-index.json"
            if (Test-Path $idxPath) {
                try {
                    $idx = Get-Content $idxPath -Raw | ConvertFrom-Json -ErrorAction Stop
                    foreach ($entry in $idx.entries) {
                        $sid = $entry.sessionId
                        if ($sid) {
                            $data.sessionMeta[$sid] = @{
                                customTitle = $entry.customTitle
                                summary = $entry.summary
                                firstPrompt = if ($entry.firstPrompt.Length -gt 120) { $entry.firstPrompt.Substring(0,120) + '...' } else { $entry.firstPrompt }
                                messageCount = $entry.messageCount
                                created = $entry.created
                                modified = $entry.modified
                                projectPath = $entry.projectPath
                            }
                        }
                    }
                } catch { continue }
            }
        }
    }

    # Load limit history snapshots
    if (Test-Path $limitHistPath) {
        try {
            foreach ($line in [System.IO.File]::ReadLines($limitHistPath)) {
                if ($line.Trim().Length -eq 0) { continue }
                try { $data.limitHistory += ($line | ConvertFrom-Json -ErrorAction Stop) } catch {}
            }
        } catch {}
    }

    # Log a snapshot if liveUsage has data and enough time has passed (5min min interval)
    if ($data.liveUsage -and $data.liveUsage.pct5hr -ne $null) {
        $shouldLog = $true
        if ($data.limitHistory.Count -gt 0) {
            $lastEntry = $data.limitHistory[-1]
            if ($lastEntry.ts) {
                try {
                    $lastTime = [System.DateTimeOffset]::Parse($lastEntry.ts)
                    $elapsed = ([System.DateTimeOffset]::UtcNow - $lastTime).TotalMinutes
                    if ($elapsed -lt 5) { $shouldLog = $false }
                } catch {}
            }
        }
        if ($shouldLog) {
            try {
                $snapNow = [System.DateTimeOffset]::UtcNow
                $snap = @{
                    ts = $snapNow.ToString('o')
                    pct5hr = $data.liveUsage.pct5hr
                    pctWeekly = $data.liveUsage.pctWeekly
                    pctSonnet = $data.liveUsage.pctSonnet
                }
                # Add reset times for window boundary context
                if ($data.liveUsage.reset5hr) { $snap.reset5hr = $data.liveUsage.reset5hr }
                if ($data.liveUsage.resetWeekly) { $snap.resetWeekly = $data.liveUsage.resetWeekly }

                # Compute window-bounded weighted tokens from history
                $tw = if ($data.usageConfig -and $data.usageConfig.token_weights) { $data.usageConfig.token_weights } else { $null }
                if ($tw -and $data.history.Count -gt 0) {
                    $start5 = if ($data.liveUsage.reset5hr) { try { ([System.DateTimeOffset]::Parse($data.liveUsage.reset5hr)).AddHours(-5) } catch { $null } } else { $null }
                    $startW = if ($data.liveUsage.resetWeekly) { try { ([System.DateTimeOffset]::Parse($data.liveUsage.resetWeekly)).AddDays(-7) } catch { $null } } else { $null }
                    $wt5 = 0.0; $wtW = 0.0
                    foreach ($h in $data.history) {
                        try {
                            $hts = [System.DateTimeOffset]::Parse($h.ts)
                            $w = ($h.input * $tw.input) + ($h.cr * $tw.cache_read) + ($h.cw * $tw.cache_write) + ($h.out * $tw.output)
                            if ($start5 -and $hts -ge $start5) { $wt5 += $w }
                            if ($startW -and $hts -ge $startW) { $wtW += $w }
                        } catch {}
                    }
                    if ($wt5 -gt 0) { $snap.wt5hr = [math]::Round($wt5) }
                    if ($wtW -gt 0) { $snap.wtWeekly = [math]::Round($wtW) }
                }

                $snapJson = ($snap | ConvertTo-Json -Compress) + "`n"
                [System.IO.File]::AppendAllText($limitHistPath, $snapJson)
                $data.limitHistory += $snap
            } catch {}
        }
    }

    return ($data | ConvertTo-Json -Depth 10 -Compress)
}

# ══════════════════════════════════════════════════════════════════════════════
# HTTP SERVER
# ══════════════════════════════════════════════════════════════════════════════
$port = 8384

# Kill any existing listener on this port
$existing = Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue
if ($existing) {
    foreach ($conn in $existing) {
        $proc = Get-Process -Id $conn.OwningProcess -ErrorAction SilentlyContinue
        Write-Host "  Killing existing server (PID $($conn.OwningProcess) - $($proc.ProcessName))" -ForegroundColor DarkYellow
        Stop-Process -Id $conn.OwningProcess -Force -ErrorAction SilentlyContinue
    }
    Start-Sleep -Milliseconds 500
}

$listener = [System.Net.HttpListener]::new()

# Try binding to all interfaces first (requires admin or URL ACL), fall back to localhost
$lanOnly = $false
try {
    $listener.Prefixes.Add("http://+:$port/")
    $listener.Start()
} catch {
    $listener.Close()
    $listener = [System.Net.HttpListener]::new()
    try {
        $listener.Prefixes.Add("http://localhost:$port/")
        $listener.Start()
        $lanOnly = $true
    } catch {
        Write-Host "Port $port still in use after cleanup. Try again in a few seconds." -ForegroundColor Red
        exit 1
    }
}

# Detect LAN IP for display - skip virtual adapters (Hyper-V, WSL, vEthernet, Tailscale)
$lanIP = (Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue |
    Where-Object {
        $_.IPAddress -notlike '169.*' -and $_.IPAddress -ne '127.0.0.1' -and
        $_.PrefixOrigin -ne 'WellKnown' -and
        $_.InterfaceAlias -notmatch 'vEthernet|Hyper-V|WSL|Loopback|Tailscale'
    } |
    Select-Object -First 1).IPAddress

Start-Process "http://localhost:$port"
Write-Host ""
Write-Host "  PERISCOPE Telemetry Server" -ForegroundColor Cyan
Write-Host "  ==========================" -ForegroundColor DarkCyan
Write-Host ""
Write-Host "  Local:  " -NoNewline; Write-Host "http://localhost:$port" -ForegroundColor Green
if ($lanIP -and -not $lanOnly) {
    Write-Host "  LAN:    " -NoNewline; Write-Host "http://${lanIP}:$port" -ForegroundColor Green
} elseif ($lanOnly) {
    Write-Host "  LAN:    " -NoNewline; Write-Host "unavailable (run as admin for LAN access)" -ForegroundColor DarkYellow
}
Write-Host "  Status: " -NoNewline; Write-Host "SERVING" -ForegroundColor Green
Write-Host ""
Write-Host "  This window must stay open for the dashboard to work." -ForegroundColor Yellow
Write-Host "  Close this window or press Ctrl+C to stop the server." -ForegroundColor DarkGray
Write-Host ""

# ══════════════════════════════════════════════════════════════════════════════
# HTML TEMPLATE — loaded from external file on each request for hot-reload
# ══════════════════════════════════════════════════════════════════════════════
$htmlTemplatePath = Join-Path $stateDir "telemetry-dashboard.html"

# Legacy inline template removed — now served from telemetry-dashboard.html
# Edit that file and refresh browser to see changes instantly (no server restart)
$htmlTemplateFallback = @'
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>PERISCOPE</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;600&family=Inter:wght@400;500;600;700&display=swap" rel="stylesheet">
<script src="https://cdn.jsdelivr.net/npm/chart.js@4"></script>
<script src="https://cdn.jsdelivr.net/npm/chartjs-adapter-date-fns@3"></script>
<style>
:root {
  --bg-primary: #0e0a14;
  --bg-surface: #150f1e;
  --bg-elevated: #1e1529;
  --bg-card: #16101f;
  --accent-coral: #f472b6;
  --accent-cyan: #e879f9;
  --accent-blue: #a78bfa;
  --accent-purple: #c084fc;
  --accent-amber: #f9a8d4;
  --text-primary: #f5f0ff;
  --text-secondary: #a78bb0;
  --border: rgba(167,139,176,0.18);
  --glow-cyan: rgba(232,121,249,0.3);
}
*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
body {
  font-family: 'Inter', sans-serif;
  background: var(--bg-primary);
  color: var(--text-primary);
  min-height: 100vh;
  padding: 24px;
}
.grid {
  display: grid;
  grid-template-columns: repeat(12, 1fr);
  gap: 20px;
  max-width: 1440px;
  margin: 0 auto;
}
.card {
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: 12px;
  padding: 20px;
  position: relative;
  overflow: hidden;
}
.card::before {
  content: '';
  position: absolute;
  top: 0; left: 0; right: 0;
  height: 2px;
  background: linear-gradient(90deg, var(--accent-cyan), var(--accent-blue), var(--accent-purple));
}
.card h2 {
  font-family: 'Inter', sans-serif;
  font-weight: 700;
  font-size: 14px;
  text-transform: uppercase;
  letter-spacing: 1.5px;
  color: var(--text-secondary);
  margin-bottom: 16px;
}
.span-12 { grid-column: span 12; }
.span-8 { grid-column: span 8; }
.span-6 { grid-column: span 6; }
.span-4 { grid-column: span 4; }
.span-3 { grid-column: span 3; }

/* Header */
.header {
  grid-column: span 12;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 16px 24px;
}
.header h1 {
  font-family: 'Inter', sans-serif;
  font-weight: 700;
  font-size: 24px;
  background: linear-gradient(135deg, var(--accent-cyan), var(--accent-blue));
  -webkit-background-clip: text;
  -webkit-text-fill-color: transparent;
}
.header-badges { display: flex; gap: 10px; align-items: center; }
.badge {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 4px 12px;
  border-radius: 20px;
  font-size: 12px;
  font-family: 'JetBrains Mono', monospace;
  font-weight: 600;
  border: 1px solid var(--border);
  background: var(--bg-elevated);
}
.badge-green { color: #86efac; border-color: rgba(134,239,172,0.3); }
.badge-amber { color: var(--accent-amber); border-color: rgba(249,168,212,0.3); }
.badge-cyan { color: var(--accent-cyan); border-color: rgba(232,121,249,0.3); }
.btn {
  padding: 6px 16px;
  border-radius: 8px;
  border: 1px solid var(--border);
  background: var(--bg-elevated);
  color: var(--text-primary);
  font-family: 'JetBrains Mono', monospace;
  font-size: 12px;
  cursor: pointer;
  transition: all 0.2s;
}
.btn:hover { border-color: var(--accent-cyan); box-shadow: 0 0 12px var(--glow-cyan); }
.btn-danger { border-color: rgba(244,114,182,0.4); }
.btn-danger:hover { border-color: var(--accent-coral); box-shadow: 0 0 12px rgba(244,114,182,0.5); }
.btn-save {
  background: linear-gradient(135deg, var(--accent-cyan), var(--accent-blue));
  color: #050810;
  border: none;
  font-weight: 600;
  padding: 8px 24px;
}
.btn-save:hover { opacity: 0.9; }

/* Toggle group (chart controls) */
.toggle-group {
  display: inline-flex;
  border: 1px solid var(--border);
  border-radius: 6px;
  overflow: hidden;
}
.toggle-btn {
  padding: 4px 12px;
  border: none;
  background: var(--bg-surface);
  color: var(--text-secondary);
  font-family: 'JetBrains Mono', monospace;
  font-size: 11px;
  cursor: pointer;
  transition: all 0.2s;
}
.toggle-btn:not(:last-child) { border-right: 1px solid var(--border); }
.toggle-btn.active {
  background: var(--accent-cyan);
  color: var(--bg-primary);
  font-weight: 600;
}
.toggle-btn:hover:not(.active) { background: var(--bg-elevated); }

/* Empty state with penguin */
.empty-state {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  min-height: 224px;
  padding-top: 50px;
  gap: 8px;
}
.penguin-scene {
  position: relative;
  display: inline-block;
}
.penguin-ascii {
  font-family: 'JetBrains Mono', monospace;
  font-size: 10px;
  line-height: 1.15;
  color: var(--accent-purple);
  text-align: left;
  white-space: pre;
  margin: 0 auto;
  display: inline-block;
  position: relative;
  z-index: 2;
}
.data-stream {
  position: absolute;
  top: 0; left: -200px; right: 50%; bottom: 0;
  overflow: hidden;
  z-index: 1;
}
.data-particle {
  position: absolute;
  left: -60px;
  font-family: 'JetBrains Mono', monospace;
  font-size: 11px;
  color: var(--accent-cyan);
  opacity: 0;
  animation: flyIn 3s linear infinite;
}
@keyframes flyIn {
  0% { left: 0; opacity: 0; transform: scale(1); }
  10% { opacity: 0.9; }
  70% { opacity: 0.5; }
  100% { left: 90%; opacity: 0; transform: scale(0.2); }
}
.typing-line {
  font-family: 'JetBrains Mono', monospace;
  font-size: 13px;
  color: var(--text-secondary);
}
.typing-dots::after {
  content: '';
  animation: dots 1.5s steps(4, end) infinite;
}
@keyframes dots {
  0% { content: ''; }
  25% { content: '.'; }
  50% { content: '..'; }
  75% { content: '...'; }
}
.empty-msg {
  font-family: 'JetBrains Mono', monospace;
  font-size: 11px;
  color: var(--text-secondary);
  opacity: 0.6;
}

/* Dashboard theme selector */
.theme-select {
  background: var(--bg-surface);
  border: 1px solid var(--border);
  border-radius: 6px;
  color: var(--text-primary);
  padding: 4px 8px;
  font-family: 'JetBrains Mono', monospace;
  font-size: 11px;
  cursor: pointer;
}
.theme-select:focus { outline: none; border-color: var(--accent-cyan); }

/* Theme: Void */
[data-theme="void"] {
  --bg-primary: #020204;
  --bg-surface: #08080c;
  --bg-elevated: #0e0e14;
  --bg-card: #0a0a10;
  --accent-coral: #6b7280;
  --accent-cyan: #9ca3af;
  --accent-blue: #6b7280;
  --accent-purple: #7c8293;
  --accent-amber: #9ca3af;
  --text-primary: #d1d5db;
  --text-secondary: #4b5563;
  --border: rgba(75,85,99,0.2);
  --glow-cyan: rgba(156,163,175,0.15);
}
/* Theme: Matrix */
[data-theme="matrix"] {
  --bg-primary: #000800;
  --bg-surface: #001200;
  --bg-elevated: #001a00;
  --bg-card: #000f00;
  --accent-coral: #ff3333;
  --accent-cyan: #00ff41;
  --accent-blue: #00cc33;
  --accent-purple: #00ff41;
  --accent-amber: #33ff77;
  --text-primary: #00ff41;
  --text-secondary: #008f11;
  --border: rgba(0,255,65,0.12);
  --glow-cyan: rgba(0,255,65,0.25);
}
/* Theme: Abyss */
[data-theme="abyss"] {
  --bg-primary: #000000;
  --bg-surface: #050508;
  --bg-elevated: #0a0a10;
  --bg-card: #060609;
  --accent-coral: #e06c75;
  --accent-cyan: #56b6c2;
  --accent-blue: #61afef;
  --accent-purple: #c678dd;
  --accent-amber: #e5c07b;
  --text-primary: #abb2bf;
  --text-secondary: #5c6370;
  --border: rgba(92,99,112,0.15);
  --glow-cyan: rgba(86,182,194,0.2);
}
/* Theme: Ember */
[data-theme="ember"] {
  --bg-primary: #0c0604;
  --bg-surface: #140e0a;
  --bg-elevated: #1c1410;
  --bg-card: #120c08;
  --accent-coral: #ff6b35;
  --accent-cyan: #ff9f1c;
  --accent-blue: #e07a5f;
  --accent-purple: #f4845f;
  --accent-amber: #ffb627;
  --text-primary: #ffecd2;
  --text-secondary: #a0806a;
  --border: rgba(160,128,106,0.18);
  --glow-cyan: rgba(255,159,28,0.25);
}
/* Theme: Arctic */
[data-theme="arctic"] {
  --bg-primary: #060a10;
  --bg-surface: #0b1018;
  --bg-elevated: #111824;
  --bg-card: #0d121c;
  --accent-coral: #bf616a;
  --accent-cyan: #88c0d0;
  --accent-blue: #81a1c1;
  --accent-purple: #b48ead;
  --accent-amber: #ebcb8b;
  --text-primary: #eceff4;
  --text-secondary: #616e88;
  --border: rgba(97,110,136,0.18);
  --glow-cyan: rgba(136,192,208,0.2);
}

/* Gauge */
.gauge-container { display: flex; gap: 32px; justify-content: center; align-items: center; min-height: 180px; }
.gauge-item { text-align: center; }
.gauge-item canvas { display: block; margin: 0 auto 8px; }
.gauge-label { font-size: 12px; color: var(--text-secondary); font-family: 'JetBrains Mono', monospace; }
.gauge-value { font-size: 28px; font-weight: 700; font-family: 'JetBrains Mono', monospace; }

/* Metric cards */
.metric-grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 12px; }
.metric-card {
  background: var(--bg-surface);
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 16px;
}
.metric-card .label { font-size: 11px; color: var(--text-secondary); text-transform: uppercase; letter-spacing: 1px; margin-bottom: 4px; }
.metric-card .value { font-size: 22px; font-weight: 700; font-family: 'JetBrains Mono', monospace; }
.metric-card .sub { font-size: 11px; color: var(--text-secondary); margin-top: 2px; }

/* Charts */
.chart-wrap { position: relative; width: 100%; }
.chart-wrap { position: relative; height: 280px; }
.chart-wrap canvas { width: 100% !important; height: 100% !important; }
.chart-wrap-sm { position: relative; height: 220px; }
.chart-wrap-sm canvas { width: 100% !important; height: 100% !important; }

/* Tables */
.data-table { width: 100%; border-collapse: collapse; font-family: 'JetBrains Mono', monospace; font-size: 12px; }
.data-table th {
  text-align: left;
  padding: 8px 12px;
  font-size: 10px;
  text-transform: uppercase;
  letter-spacing: 1px;
  color: var(--text-secondary);
  border-bottom: 1px solid var(--border);
}
.data-table td {
  padding: 8px 12px;
  border-bottom: 1px solid rgba(136,146,176,0.07);
  color: var(--text-primary);
}
.data-table tr:hover td { background: rgba(232,121,249,0.03); }

/* Config section */
.config-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 10px 0;
  border-bottom: 1px solid rgba(136,146,176,0.07);
}
.config-row label { font-size: 13px; color: var(--text-secondary); }
.config-row select, .config-row input {
  background: var(--bg-surface);
  border: 1px solid var(--border);
  border-radius: 6px;
  color: var(--text-primary);
  padding: 6px 12px;
  font-family: 'JetBrains Mono', monospace;
  font-size: 12px;
}
.config-row select:focus, .config-row input:focus { outline: none; border-color: var(--accent-cyan); }
.segments-grid {
  display: grid;
  grid-template-columns: repeat(2, 1fr);
  gap: 8px;
  margin: 12px 0;
}
.seg-item {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px;
  background: var(--bg-surface);
  border-radius: 6px;
  font-size: 12px;
  font-family: 'JetBrains Mono', monospace;
}
.seg-item input[type="number"] { width: 40px; text-align: center; }

/* Toggle switch */
.toggle { position: relative; width: 36px; height: 20px; flex-shrink: 0; }
.toggle input { opacity: 0; width: 0; height: 0; }
.toggle .slider {
  position: absolute;
  cursor: pointer;
  top: 0; left: 0; right: 0; bottom: 0;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: 20px;
  transition: 0.2s;
}
.toggle .slider::before {
  content: '';
  position: absolute;
  height: 14px; width: 14px;
  left: 2px; bottom: 2px;
  background: var(--text-secondary);
  border-radius: 50%;
  transition: 0.2s;
}
.toggle input:checked + .slider { background: var(--accent-cyan); border-color: var(--accent-cyan); }
.toggle input:checked + .slider::before { transform: translateX(16px); background: #050810; }

/* Calibration timeline */
.cal-item {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 8px 0;
  border-bottom: 1px solid rgba(136,146,176,0.07);
  font-size: 12px;
  font-family: 'JetBrains Mono', monospace;
}
.cal-dot {
  width: 8px; height: 8px;
  border-radius: 50%;
  flex-shrink: 0;
}

/* Toast */
.toast {
  position: fixed;
  bottom: 24px;
  right: 24px;
  padding: 12px 20px;
  border-radius: 8px;
  font-size: 13px;
  font-family: 'JetBrains Mono', monospace;
  background: var(--bg-elevated);
  border: 1px solid var(--accent-cyan);
  color: var(--accent-cyan);
  box-shadow: 0 0 20px var(--glow-cyan);
  opacity: 0;
  transition: opacity 0.3s;
  z-index: 1000;
}
.toast.show { opacity: 1; }
.toast.error { border-color: var(--accent-coral); color: var(--accent-coral); box-shadow: 0 0 20px rgba(244,114,182,0.5); }

/* Modal */
.modal-overlay {
  display: none;
  position: fixed;
  top: 0; left: 0; right: 0; bottom: 0;
  background: rgba(5,8,16,0.85);
  z-index: 900;
  align-items: center;
  justify-content: center;
}
.modal-overlay.show { display: flex; }
.modal {
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: 12px;
  padding: 28px;
  width: 380px;
}
.modal h3 {
  font-size: 16px;
  font-weight: 700;
  margin-bottom: 16px;
  color: var(--accent-cyan);
}
.modal label { display: block; font-size: 13px; color: var(--text-secondary); margin-bottom: 4px; }
.modal input {
  width: 100%;
  background: var(--bg-surface);
  border: 1px solid var(--border);
  border-radius: 6px;
  color: var(--text-primary);
  padding: 8px 12px;
  font-family: 'JetBrains Mono', monospace;
  font-size: 14px;
  margin-bottom: 12px;
}
.modal input:focus { outline: none; border-color: var(--accent-cyan); }
.modal-actions { display: flex; gap: 10px; justify-content: flex-end; margin-top: 8px; }

/* Detail card */
.detail-row {
  display: flex;
  justify-content: space-between;
  padding: 6px 0;
  border-bottom: 1px solid rgba(136,146,176,0.07);
  font-size: 12px;
  font-family: 'JetBrains Mono', monospace;
}
.detail-row .key { color: var(--text-secondary); }
.detail-row .val { color: var(--text-primary); font-weight: 600; }

/* Responsive — tablet */
@media (max-width: 1024px) {
  .span-4, .span-3 { grid-column: span 6; }
  .span-8 { grid-column: span 12; }
  .metric-grid { grid-template-columns: repeat(2, 1fr); }
  body { padding: 16px; }
  .grid { gap: 14px; }
}
/* Responsive — mobile */
@media (max-width: 640px) {
  body { padding: 10px; }
  .grid { gap: 10px; }
  .card { padding: 14px; border-radius: 8px; }
  .span-12, .span-8, .span-6, .span-4, .span-3 { grid-column: span 12; }

  /* Header stacks vertically */
  .header { flex-direction: column; gap: 12px; padding: 14px; align-items: stretch; }
  .header h1 { font-size: 18px; text-align: center; }
  .header-badges { flex-wrap: wrap; justify-content: center; gap: 6px; }
  .badge { font-size: 10px; padding: 3px 8px; }
  .btn { font-size: 11px; padding: 5px 12px; }

  /* Gauges side by side but smaller */
  .gauge-container { gap: 16px; min-height: 140px; }
  .gauge-item canvas { width: 90px !important; height: 90px !important; }
  .gauge-value { font-size: 22px; }
  .gauge-label { font-size: 10px; }

  /* Metric cards single column */
  .metric-grid { grid-template-columns: 1fr 1fr; gap: 8px; }
  .metric-card { padding: 12px; }
  .metric-card .value { font-size: 18px; }
  .metric-card .label { font-size: 10px; }
  .metric-card .sub { font-size: 10px; }

  /* Chart controls: horizontal scroll */
  .chart-controls-wrap { overflow-x: auto; -webkit-overflow-scrolling: touch; padding-bottom: 4px; }
  .toggle-btn { padding: 4px 8px; font-size: 10px; white-space: nowrap; }
  .chart-wrap { height: 220px; }
  .chart-wrap-sm { height: 180px; }

  /* Table: already scrollable, reduce font */
  .data-table { font-size: 10px; }
  .data-table th, .data-table td { padding: 6px 8px; }

  /* Segments grid single column */
  .segments-grid { grid-template-columns: 1fr; }
  .seg-item { font-size: 11px; }

  /* Config rows */
  .config-row label { font-size: 12px; }
  .config-row select, .config-row input { font-size: 11px; padding: 5px 8px; }

  /* Modal: near full-width */
  .modal { width: calc(100vw - 32px); padding: 20px; }
  .modal h3 { font-size: 14px; }
  .modal input { font-size: 13px; padding: 6px 10px; }

  /* Detail rows */
  .detail-row { font-size: 11px; }

  /* Toast */
  .toast { left: 16px; right: 16px; bottom: 16px; font-size: 12px; text-align: center; }

  /* Card titles */
  .card h2 { font-size: 12px; letter-spacing: 1px; margin-bottom: 12px; }
}
/* Responsive — narrow mobile */
@media (max-width: 380px) {
  .metric-grid { grid-template-columns: 1fr; }
  .gauge-container { flex-direction: column; gap: 12px; }
  .header-badges .badge { flex: 0 0 auto; }
}
</style>
</head>
<body>

<div class="grid">
  <!-- Row 1: Header -->
  <div class="header card">
    <h1>PERI<span style="color:var(--accent)">SCOPE</span></h1>
    <div class="header-badges">
      <span class="badge badge-cyan" id="badge-model">--</span>
      <span class="badge badge-green" id="badge-sub">--</span>
      <span class="badge badge-green" id="badge-sessions">--</span>
      <span class="badge badge-amber" id="badge-cost">--</span>
      <select class="theme-select" onchange="setDashTheme(this.value)" id="dash-theme">
        <option value="pastel">Pastel</option>
        <option value="void">Void</option>
        <option value="matrix">Matrix</option>
        <option value="abyss">Abyss</option>
        <option value="ember">Ember</option>
        <option value="arctic">Arctic</option>
      </select>
      <button class="btn" onclick="refreshData()">Refresh</button>
      <button class="btn btn-danger" onclick="shutdownServer()">Stop Server</button>
    </div>
  </div>

  <!-- Row 2: Rate Gauges + Metric Cards -->
  <div class="card span-4" id="gauge-section">
    <h2>Rate Limits <span class="badge badge-green" id="live-badge" style="font-size:10px;padding:2px 8px;margin-left:8px;vertical-align:middle">LIVE</span></h2>
    <div class="gauge-container">
      <div class="gauge-item">
        <canvas id="gauge-5hr" width="120" height="120"></canvas>
        <div class="gauge-value" id="gauge-5hr-val">--%</div>
        <div class="gauge-label">5-HOUR</div>
        <div class="gauge-reset" id="gauge-5hr-reset" style="font-size:10px;color:var(--text-secondary);font-family:'JetBrains Mono',monospace;margin-top:2px"></div>
      </div>
      <div class="gauge-item">
        <canvas id="gauge-weekly" width="120" height="120"></canvas>
        <div class="gauge-value" id="gauge-weekly-val">--%</div>
        <div class="gauge-label">WEEKLY</div>
        <div class="gauge-reset" id="gauge-weekly-reset" style="font-size:10px;color:var(--text-secondary);font-family:'JetBrains Mono',monospace;margin-top:2px"></div>
      </div>
      <div class="gauge-item">
        <canvas id="gauge-sonnet" width="120" height="120"></canvas>
        <div class="gauge-value" id="gauge-sonnet-val">--%</div>
        <div class="gauge-label">SONNET</div>
        <div class="gauge-reset" id="gauge-sonnet-reset" style="font-size:10px;color:var(--text-secondary);font-family:'JetBrains Mono',monospace;margin-top:2px"></div>
      </div>
    </div>
    <div style="text-align:center;margin-top:12px">
      <button class="btn" onclick="refreshUsage()">Refresh</button>
      <span id="usage-age" style="font-size:10px;color:var(--text-secondary);margin-left:8px;font-family:'JetBrains Mono',monospace"></span>
    </div>
  </div>

  <div class="card span-8" id="metrics-section">
    <h2>Key Metrics</h2>
    <div class="metric-grid">
      <div class="metric-card">
        <div class="label">Total Cost</div>
        <div class="value" id="m-cost">--</div>
        <div class="sub" id="m-cost-sub"></div>
      </div>
      <div class="metric-card">
        <div class="label">Total Turns</div>
        <div class="value" id="m-turns">--</div>
        <div class="sub" id="m-turns-sub"></div>
      </div>
      <div class="metric-card">
        <div class="label">Cache Hit Rate</div>
        <div class="value" id="m-cache">--</div>
        <div class="sub" id="m-cache-sub"></div>
      </div>
      <div class="metric-card">
        <div class="label">Weighted Tokens</div>
        <div class="value" id="m-weighted">--</div>
        <div class="sub" id="m-weighted-sub"></div>
      </div>
    </div>
  </div>

  <!-- Row 3: Usage Over Time (unified) -->
  <div class="card span-12" id="usage-chart-section">
    <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:12px;flex-wrap:wrap;gap:8px">
      <h2 style="margin-bottom:0">Usage Over Time</h2>
      <div class="chart-controls-wrap" style="display:flex;gap:8px;align-items:center">
        <div class="toggle-group">
          <button class="toggle-btn active" data-metric="tokens" onclick="setChartMetric('tokens')">Tokens</button>
          <button class="toggle-btn" data-metric="cost" onclick="setChartMetric('cost')">Cost</button>
        </div>
        <div class="toggle-group">
          <button class="toggle-btn active" data-range="5h" onclick="setChartRange('5h')">5h</button>
          <button class="toggle-btn" data-range="24h" onclick="setChartRange('24h')">24h</button>
          <button class="toggle-btn" data-range="7d" onclick="setChartRange('7d')">7d</button>
          <button class="toggle-btn" data-range="30d" onclick="setChartRange('30d')">30d</button>
        </div>
        <div class="toggle-group">
          <button class="toggle-btn" onclick="chartNav(-1)" title="Back">&larr;</button>
          <button class="toggle-btn" onclick="chartNav(0)" title="Now" style="font-size:10px">Now</button>
          <button class="toggle-btn" onclick="chartNav(1)" title="Forward">&rarr;</button>
        </div>
      </div>
    </div>
    <div id="chart-timeframe" style="text-align:center;font-family:'JetBrains Mono',monospace;font-size:11px;color:var(--text-secondary);margin-bottom:8px"></div>
    <div class="chart-wrap"><canvas id="chart-usage"></canvas></div>
  </div>

  <!-- Row 4: Token Donut | Tool Bars | Cost Breakdown -->
  <div class="card span-4">
    <h2>Token Breakdown</h2>
    <div class="chart-wrap-sm"><canvas id="chart-tokens"></canvas></div>
  </div>
  <div class="card span-4">
    <h2>Tool Usage</h2>
    <div class="chart-wrap-sm"><canvas id="chart-tools"></canvas></div>
  </div>
  <div class="card span-4">
    <h2>Cost Breakdown</h2>
    <div class="chart-wrap-sm"><canvas id="chart-cost-breakdown"></canvas></div>
  </div>

  <!-- Row 5: Session History -->
  <div class="card span-12">
    <h2>Session History</h2>
    <div style="overflow-x:auto">
      <table class="data-table" id="session-table">
        <thead><tr>
          <th>Session</th><th>Summary</th><th>Turns</th><th>Cost</th>
          <th>Cache Read</th><th>Output</th>
          <th>First Seen</th><th>Last Seen</th>
        </tr></thead>
        <tbody id="session-tbody"></tbody>
      </table>
    </div>
  </div>

  <!-- Row 7: Statusline Config | Last Turn Detail -->
  <div class="card span-6" id="config-section">
    <h2>Statusline Config</h2>
    <div class="config-row">
      <label>Theme</label>
      <select id="cfg-theme">
        <option value="catppuccin-mocha">Catppuccin Mocha</option>
        <option value="dracula">Dracula</option>
        <option value="tokyo-night">Tokyo Night</option>
        <option value="nord">Nord</option>
        <option value="gruvbox">Gruvbox</option>
      </select>
    </div>
    <div class="config-row">
      <label>Style</label>
      <select id="cfg-style">
        <option value="powerline">Powerline</option>
        <option value="plain">Plain</option>
        <option value="minimal">Minimal</option>
      </select>
    </div>
    <div class="segments-grid" id="seg-grid"></div>
    <div style="display:flex;gap:10px;justify-content:flex-end;margin-top:12px">
      <button class="btn-save btn" onclick="saveConfig()">Save Config</button>
    </div>
  </div>

  <div class="card span-6" id="last-turn-section">
    <h2>Last Turn Detail</h2>
    <div id="last-turn-detail"></div>
  </div>
</div>

<!-- Account Info (hidden until data loads) -->
<div id="account-info" style="display:none"></div>

<!-- Toast -->
<div class="toast" id="toast"></div>

<script>
let D = null; // global data

// Dashboard theme (persisted to localStorage)
function setDashTheme(t) {
  if (t === 'pastel') {
    document.documentElement.removeAttribute('data-theme');
  } else {
    document.documentElement.setAttribute('data-theme', t);
  }
  localStorage.setItem('periscope-dash-theme', t);
  // Re-read CSS vars and re-render charts
  setTimeout(() => { COLORS = getColors(); render(); }, 50);
}
(function() {
  const saved = localStorage.getItem('periscope-dash-theme');
  if (saved && saved !== 'pastel') {
    document.documentElement.setAttribute('data-theme', saved);
    // Update dropdown after DOM ready
    requestAnimationFrame(() => {
      const sel = document.getElementById('dash-theme');
      if (sel) sel.value = saved;
    });
  }
})();

function getColors() {
  const s = getComputedStyle(document.documentElement);
  return {
    coral: s.getPropertyValue('--accent-coral').trim(),
    cyan: s.getPropertyValue('--accent-cyan').trim(),
    blue: s.getPropertyValue('--accent-blue').trim(),
    purple: s.getPropertyValue('--accent-purple').trim(),
    amber: s.getPropertyValue('--accent-amber').trim(),
    green: '#86efac',
    surface: s.getPropertyValue('--bg-surface').trim(),
    text2: s.getPropertyValue('--text-secondary').trim()
  };
}
let COLORS = getColors();

const SEGMENTS = ['dir','git','model','turns','rate-5hr','rate-weekly','cache','tools','context','vim'];

function fmt(n) { return typeof n === 'number' ? n.toLocaleString() : '--'; }
function fmtK(n) { return typeof n === 'number' ? (n >= 1e6 ? (n/1e6).toFixed(1)+'M' : (n >= 1e3 ? (n/1e3).toFixed(0)+'K' : n.toString())) : '--'; }
function fmtCost(n) { return typeof n === 'number' ? '$'+n.toFixed(2) : '--'; }
function shortSid(s) { return s ? s.substring(0,8) : '--'; }
function fmtTime(ts) {
  if (!ts) return '--';
  const d = new Date(ts);
  return d.toLocaleTimeString([], {hour:'2-digit',minute:'2-digit'}) + ' ' + d.toLocaleDateString([], {month:'short',day:'numeric'});
}

function showToast(msg, isError) {
  const t = document.getElementById('toast');
  t.textContent = msg;
  t.className = 'toast show' + (isError ? ' error' : '');
  setTimeout(() => t.className = 'toast', 3000);
}

// ────── Gauge drawing ──────
function drawGauge(canvasId, pct, color) {
  const c = document.getElementById(canvasId);
  if (!c) return;
  const ctx = c.getContext('2d');
  const w = c.width, h = c.height;
  const cx = w/2, cy = h/2, r = Math.min(w,h)/2 - 8;
  ctx.clearRect(0,0,w,h);

  // Background arc
  ctx.beginPath();
  ctx.arc(cx, cy, r, 0.75*Math.PI, 2.25*Math.PI);
  ctx.strokeStyle = '#251a30';
  ctx.lineWidth = 10;
  ctx.lineCap = 'round';
  ctx.stroke();

  // Value arc
  const sweep = (pct/100) * 1.5 * Math.PI;
  ctx.beginPath();
  ctx.arc(cx, cy, r, 0.75*Math.PI, 0.75*Math.PI + sweep);
  ctx.strokeStyle = color;
  ctx.lineWidth = 10;
  ctx.lineCap = 'round';
  ctx.stroke();
}

function gaugeColor(pct) {
  if (pct < 50) return COLORS.green;
  if (pct < 75) return COLORS.amber;
  return COLORS.coral;
}

// ────── Data rendering ──────
function render() {
  if (!D) return;

  // Pick sidecar with highest cost for model info
  let latest = null;
  if (D.sidecars && D.sidecars.length > 0) {
    for (const sc of D.sidecars) {
      const c = sc.data.cumulative;
      if (!latest || (c && c.cost > (latest.cumulative ? latest.cumulative.cost : 0))) {
        latest = sc.data;
      }
    }
  }

  const nSessions = D.sidecars ? D.sidecars.length : 0;

  // Header badges
  const model = latest && latest.lastTurn ? latest.lastTurn.model : 'unknown';
  document.getElementById('badge-model').textContent = model.replace('claude-','').replace('-',' ');
  document.getElementById('badge-sessions').textContent = nSessions + ' sessions';
  const totalCost = D.sidecars ? D.sidecars.reduce((s,sc) => s + (sc.data.cumulative ? sc.data.cumulative.cost : 0), 0) : 0;
  document.getElementById('badge-cost').textContent = fmtCost(totalCost);

  // Subscription badge from profile
  if (D.profile && D.profile.subscription) {
    const sub = D.profile.subscription;
    const tier = D.profile.tier || '';
    document.getElementById('badge-sub').textContent = sub.toUpperCase() + (tier.includes('20x') ? ' 20x' : '');
  }

  // Rate gauges — from live Anthropic API
  let pct5 = 0, pctW = 0, pctS = 0;
  let reset5 = '', resetW = '', resetS = '';
  if (D.liveUsage) {
    pct5 = D.liveUsage.pct5hr >= 0 ? D.liveUsage.pct5hr : 0;
    pctW = D.liveUsage.pctWeekly >= 0 ? D.liveUsage.pctWeekly : 0;
    pctS = D.liveUsage.pctSonnet >= 0 ? D.liveUsage.pctSonnet : 0;
    reset5 = D.liveUsage.reset5hr || '';
    resetW = D.liveUsage.resetWeekly || '';
    resetS = D.liveUsage.resetSonnet || '';
  }
  drawGauge('gauge-5hr', pct5, gaugeColor(pct5));
  drawGauge('gauge-weekly', pctW, gaugeColor(pctW));
  drawGauge('gauge-sonnet', pctS, gaugeColor(pctS));
  document.getElementById('gauge-5hr-val').textContent = Math.round(pct5) + '%';
  document.getElementById('gauge-weekly-val').textContent = Math.round(pctW) + '%';
  document.getElementById('gauge-sonnet-val').textContent = Math.round(pctS) + '%';

  // Reset times
  function fmtReset(iso) {
    if (!iso) return '';
    const d = new Date(iso);
    const now = Date.now();
    const diffMin = Math.round((d - now) / 60000);
    if (diffMin <= 0) return 'now';
    if (diffMin < 60) return diffMin + 'm';
    const hrs = Math.floor(diffMin / 60);
    const mins = diffMin % 60;
    return hrs + 'h' + (mins > 0 ? mins + 'm' : '');
  }
  document.getElementById('gauge-5hr-reset').textContent = reset5 ? 'resets ' + fmtReset(reset5) : '';
  document.getElementById('gauge-weekly-reset').textContent = resetW ? 'resets ' + fmtReset(resetW) : '';
  document.getElementById('gauge-sonnet-reset').textContent = resetS ? 'resets ' + fmtReset(resetS) : '';

  // Live badge + data age
  if (D.liveUsage && D.liveUsage.fetched_at) {
    const age = Math.round(Date.now()/1000 - D.liveUsage.fetched_at);
    document.getElementById('usage-age').textContent = age < 60 ? age + 's ago' : Math.round(age/60) + 'm ago';
    document.getElementById('live-badge').style.display = '';
  } else {
    document.getElementById('live-badge').style.display = 'none';
    document.getElementById('usage-age').textContent = 'no data';
  }

  // Metric cards — all lifetime (across all tracked sessions)
  document.getElementById('m-cost').textContent = fmtCost(totalCost);
  document.getElementById('m-cost-sub').textContent = 'lifetime \u00b7 ' + nSessions + ' sessions';

  const totalTurns = D.sidecars ? D.sidecars.reduce((s,sc) => {
    const c = sc.data.cumulative;
    return s + (c ? (c.agent_calls||0) + (c.tool_calls||0) + (c.chat_calls||0) : 0);
  }, 0) : 0;
  document.getElementById('m-turns').textContent = fmt(totalTurns);
  document.getElementById('m-turns-sub').textContent = 'lifetime \u00b7 all API calls';

  // Cache hit rate (lifetime aggregate)
  let totalIn = 0, totalCR = 0;
  if (D.sidecars) {
    for (const sc of D.sidecars) {
      const c = sc.data.cumulative;
      if (!c) continue;
      totalIn += (c.input||0) + (c.cache_read||0) + (c.cache_write||0);
      totalCR += c.cache_read||0;
    }
  }
  const cacheRate = totalIn > 0 ? Math.round((totalCR/totalIn)*100) : 0;
  document.getElementById('m-cache').textContent = cacheRate + '%';
  document.getElementById('m-cache-sub').textContent = 'lifetime \u00b7 ' + fmtK(totalCR) + ' cache reads';

  // Weighted tokens (lifetime)
  const tw = {input:1,cache_read:0,cache_write:1,output:5};
  let totalWeighted = 0;
  if (D.sidecars) {
    for (const sc of D.sidecars) {
      const c = sc.data.cumulative;
      if (!c) continue;
      totalWeighted += (c.input||0)*tw.input + (c.cache_read||0)*tw.cache_read +
                       (c.cache_write||0)*tw.cache_write + (c.output||0)*tw.output;
    }
  }
  document.getElementById('m-weighted').textContent = fmtK(totalWeighted);
  const limit5 = D.usageConfig && D.usageConfig.derived_limits ? D.usageConfig.derived_limits['5hr'] : null;
  document.getElementById('m-weighted-sub').textContent = 'lifetime' + (limit5 ? ' \u00b7 5hr limit: ' + fmtK(limit5.weighted_tokens) : '');

  // ─── Usage Over Time (unified chart) ───
  renderUsageChart();
  // ─── Token Donut ───
  renderTokenDonut();
  // ─── Tool Usage ───
  renderToolBars();
  // ─── Cost Breakdown ───
  renderCostBreakdown();
  // ─── Session History ───
  renderSessionTable();
  // ─── Statusline Config ───
  renderConfigCard();
  // ─── Last Turn Detail ───
  renderLastTurn();
}

// ────── Unified Usage Chart ──────
let chartUsage = null;
let chartMetric = 'tokens';
let chartRange = '5h';
let chartOffset = 0; // 0 = current, -1 = one chunk back, etc.

function setChartMetric(m) {
  chartMetric = m;
  document.querySelectorAll('[data-metric]').forEach(b => b.classList.toggle('active', b.dataset.metric === m));
  renderUsageChart();
}
function setChartRange(r) {
  chartRange = r;
  chartOffset = 0;
  document.querySelectorAll('[data-range]').forEach(b => b.classList.toggle('active', b.dataset.range === r));
  renderUsageChart();
}
function chartNav(dir) {
  if (dir === 0) { chartOffset = 0; }
  else { chartOffset += dir; }
  if (chartOffset > 0) chartOffset = 0; // can't go into future
  renderUsageChart();
}

function fmtDateRange(from, to) {
  const opts = { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' };
  const optsDay = { month: 'short', day: 'numeric' };
  if (to - from <= 25*3600e3) {
    return from.toLocaleDateString([], optsDay) + ' ' + from.toLocaleTimeString([], {hour:'2-digit',minute:'2-digit'}) +
      '  \u2192  ' + to.toLocaleTimeString([], {hour:'2-digit',minute:'2-digit'});
  }
  return from.toLocaleDateString([], optsDay) + '  \u2192  ' + to.toLocaleDateString([], optsDay);
}

function renderUsageChart() {
  let canvas = document.getElementById('chart-usage');
  const tfEl = document.getElementById('chart-timeframe');
  if (!D.history || D.history.length === 0) {
    if (chartUsage) { chartUsage.destroy(); chartUsage = null; }
    tfEl.textContent = 'No history data';
    return;
  }

  // Ensure canvas exists (restore if empty state replaced it)
  const chartWrap = document.querySelector('#usage-chart-section .chart-wrap');
  if (!chartWrap.querySelector('canvas')) {
    chartWrap.innerHTML = '<canvas id="chart-usage"></canvas>';
  }
  canvas = document.getElementById('chart-usage');

  // Check minimum data span for 7d and 30d views
  const allTs = D.history.map(h => new Date(h.ts).getTime());
  const dataSpanDays = (Math.max(...allTs) - Math.min(...allTs)) / (24*3600e3);
  const minDays = { '5h': 0, '24h': 0, '7d': 4, '30d': 14 }[chartRange];
  if (dataSpanDays < minDays) {
    if (chartUsage) { chartUsage.destroy(); chartUsage = null; }
    tfEl.textContent = '';
    const chars = '01%$KMTB#@&+={}[]<>|~^'.split('');
    let particles = '';
    for (let p = 0; p < 30; p++) {
      const ch = chars[Math.floor(Math.random()*chars.length)];
      const top = 10 + Math.random()*60;
      const delay = (Math.random()*2.5).toFixed(1);
      const dur = (1.2 + Math.random()*1.8).toFixed(1);
      particles += '<span class="data-particle" style="top:'+top+'%;animation-delay:'+delay+'s;animation-duration:'+dur+'s">'+ch+'</span>';
    }
    chartWrap.innerHTML = '<div class="empty-state">' +
      '<div class="penguin-scene">' +
        '<div class="data-stream">' + particles + '</div>' +
        '<pre class="penguin-ascii">' +
          '                        a8888b.\n' +
          '                       d888888b.\n' +
          '                       8P"YP"Y88\n' +
          '                       8|o||o|88\n' +
          '                       8\'    .88\n' +
          '                       8`._.\' Y8.\n' +
          ' .---------------.   d/      `8b.\n' +
          ' |               |  dP   .    Y8b.\n' +
          ' |  >_           | d8:\'  "  `::88b\n' +
          ' |               | d8"         \'Y88b\n' +
          ' \'---------------\':8P    \'      :888\n' +
          '     /_|   |_\\    8a.   :     _a88P\n' +
          '      ========  ._/"Yaa_:   .| 88P|\n' +
          '                \\    YP"    `| 8P  `.\n' +
          '                /     \\.___.d|    .\'\n' +
          '                `--..__)8888P`._.\'\n' +
        '</pre>' +
      '</div>' +
      '<div class="typing-line">' +
        '<span class="typing-text">Collecting telemetry</span>' +
        '<span class="typing-dots"></span>' +
      '</div>' +
      '<div class="empty-msg">' + dataSpanDays.toFixed(1) + 'd of ' + minDays + 'd needed for ' + chartRange + ' view</div>' +
    '</div>';
    return;
  }
  const ctx = canvas.getContext('2d');
  const tw = {input:1,cache_read:0,cache_write:1,output:5};

  // Compute time window
  const rangeMs = { '5h': 5*3600e3, '24h': 24*3600e3, '7d': 7*24*3600e3, '30d': 30*24*3600e3 }[chartRange];
  const windowEnd = new Date(Date.now() + chartOffset * rangeMs);
  const windowStart = new Date(windowEnd - rangeMs);

  // Display time frame
  tfEl.textContent = fmtDateRange(windowStart, windowEnd);

  // Extract metric value from a history entry
  function metricVal(h) {
    if (chartMetric === 'cost') return h.cost || 0;
    return (h.input||0)*tw.input + (h.cr||0)*tw.cache_read +
           (h.cw||0)*tw.cache_write + (h.out||0)*tw.output;
  }

  // Filter history to window
  const inWindow = D.history.filter(h => {
    const t = new Date(h.ts);
    return t >= windowStart && t <= windowEnd;
  });

  let datasets = [];
  const palette = [COLORS.cyan, COLORS.blue, COLORS.purple, COLORS.amber, COLORS.coral, COLORS.green];

  if (chartRange === '5h') {
    // Raw data points per session
    const sessions = {};
    for (const h of inWindow) {
      if (!sessions[h.sid]) sessions[h.sid] = [];
      sessions[h.sid].push({ x: new Date(h.ts), y: metricVal(h) });
    }
    let i = 0;
    for (const [sid, points] of Object.entries(sessions)) {
      datasets.push({
        label: sid.substring(0,8),
        data: points,
        borderColor: palette[i % palette.length],
        backgroundColor: 'transparent',
        tension: 0.3, pointRadius: 2, borderWidth: 2
      });
      i++;
    }
  } else if (chartRange === '24h') {
    // Individual session lines (same as 5h but wider window)
    const sessions = {};
    for (const h of inWindow) {
      if (!sessions[h.sid]) sessions[h.sid] = [];
      sessions[h.sid].push({ x: new Date(h.ts), y: metricVal(h) });
    }
    let i = 0;
    for (const [sid, points] of Object.entries(sessions)) {
      datasets.push({
        label: sid.substring(0,8),
        data: points,
        borderColor: palette[i % palette.length],
        backgroundColor: palette[i % palette.length] + '18',
        tension: 0.3, pointRadius: 2, borderWidth: 2, fill: true
      });
      i++;
    }
  } else if (chartRange === '7d') {
    // Daily averages
    const buckets = {};
    for (const h of inWindow) {
      const d = new Date(h.ts);
      const key = d.getFullYear() + '-' + String(d.getMonth()+1).padStart(2,'0') + '-' + String(d.getDate()).padStart(2,'0');
      if (!buckets[key]) buckets[key] = { sum: 0, count: 0 };
      buckets[key].sum += metricVal(h);
      buckets[key].count++;
    }
    const points = Object.entries(buckets).map(([key, b]) => ({
      x: new Date(key + 'T12:00:00'),
      y: b.sum / b.count,
      count: b.count
    })).sort((a,b) => a.x - b.x);
    datasets.push({
      label: 'Daily Avg',
      data: points,
      borderColor: COLORS.purple,
      backgroundColor: COLORS.purple + '33',
      tension: 0.3, pointRadius: 5, borderWidth: 2, fill: true
    });
  } else if (chartRange === '30d') {
    // Weekly averages (Mon-Sun buckets)
    const buckets = {};
    for (const h of inWindow) {
      const d = new Date(h.ts);
      // Week number: ISO week start (Monday)
      const day = d.getDay() || 7;
      const monday = new Date(d);
      monday.setDate(d.getDate() - day + 1);
      const key = monday.getFullYear() + '-' + String(monday.getMonth()+1).padStart(2,'0') + '-' + String(monday.getDate()).padStart(2,'0');
      if (!buckets[key]) buckets[key] = { sum: 0, count: 0 };
      buckets[key].sum += metricVal(h);
      buckets[key].count++;
    }
    const points = Object.entries(buckets).map(([key, b]) => ({
      x: new Date(key + 'T12:00:00'),
      y: b.sum / b.count,
      count: b.count
    })).sort((a,b) => a.x - b.x);
    datasets.push({
      label: 'Weekly Avg',
      data: points,
      borderColor: COLORS.blue,
      backgroundColor: COLORS.blue + '33',
      tension: 0.3, pointRadius: 5, borderWidth: 2, fill: true
    });
  }

  // Add calibration points as scatter overlay
  if (D.usageConfig && D.usageConfig.calibrations) {
    const calPoints5 = [], calPointsW = [];
    for (const c of D.usageConfig.calibrations) {
      const t = new Date(c.timestamp);
      if (t < windowStart || t > windowEnd) continue;
      const pt = { x: t, y: c.reported_pct || 0 };
      if (c.window === '5hr') calPoints5.push(pt);
      else calPointsW.push(pt);
    }
    if (calPoints5.length > 0) {
      datasets.push({
        label: 'Cal 5hr', data: calPoints5, type: 'scatter',
        pointStyle: 'triangle', pointRadius: 7,
        borderColor: COLORS.cyan, backgroundColor: COLORS.cyan,
        showLine: false, yAxisID: 'yCal'
      });
    }
    if (calPointsW.length > 0) {
      datasets.push({
        label: 'Cal Weekly', data: calPointsW, type: 'scatter',
        pointStyle: 'triangle', pointRadius: 7,
        borderColor: COLORS.blue, backgroundColor: COLORS.blue,
        showLine: false, yAxisID: 'yCal'
      });
    }
  }

  const yFmt = chartMetric === 'cost' ? (v => '$'+v.toFixed(0)) : (v => fmtK(v));
  const hasCal = datasets.some(d => d.yAxisID === 'yCal');

  if (chartUsage) chartUsage.destroy();
  chartUsage = new Chart(ctx, {
    type: 'line',
    data: { datasets },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      interaction: { mode: 'nearest', intersect: false },
      scales: {
        x: { type: 'time', min: windowStart, max: windowEnd, ticks: { color: COLORS.text2, font: { family: 'JetBrains Mono', size: 10 } }, grid: { color: 'rgba(136,146,176,0.07)' } },
        y: { position: 'left', ticks: { color: COLORS.text2, callback: yFmt, font: { family: 'JetBrains Mono', size: 10 } }, grid: { color: 'rgba(136,146,176,0.07)' } },
        yCal: { position: 'right', display: hasCal, min: 0, max: 100, ticks: { color: COLORS.text2, callback: v => v+'%', font: { family: 'JetBrains Mono', size: 10 } }, grid: { display: false }, title: { display: true, text: 'Cal %', color: COLORS.text2, font: { family: 'JetBrains Mono', size: 10 } } }
      },
      plugins: {
        legend: { position: 'top', align: 'center', labels: { color: COLORS.text2, font: { family: 'JetBrains Mono', size: 11 }, padding: 16, usePointStyle: true, pointStyle: 'circle', boxWidth: 8 } },
        tooltip: {
          callbacks: {
            label: function(tip) {
              if (tip.dataset.yAxisID === 'yCal') return tip.dataset.label + ': ' + tip.raw.y + '%';
              const v = chartMetric === 'cost' ? '$'+tip.raw.y.toFixed(2) : fmtK(tip.raw.y);
              const extra = tip.raw.count ? ' (' + tip.raw.count + ' pts)' : '';
              const sid = tip.raw.sid ? ' [' + tip.raw.sid + ']' : '';
              return tip.dataset.label + ': ' + v + sid + extra;
            }
          }
        }
      }
    }
  });
}

let chartTokens = null;
function renderTokenDonut() {
  const ctx = document.getElementById('chart-tokens').getContext('2d');
  let tInput = 0, tCR = 0, tCW = 0, tOut = 0;
  if (D.sidecars) {
    for (const sc of D.sidecars) {
      const c = sc.data.cumulative;
      if (!c) continue;
      tInput += c.input||0;
      tCR += c.cache_read||0;
      tCW += c.cache_write||0;
      tOut += c.output||0;
    }
  }
  if (chartTokens) chartTokens.destroy();
  chartTokens = new Chart(ctx, {
    type: 'doughnut',
    data: {
      labels: ['Input', 'Cache Read', 'Cache Write', 'Output'],
      datasets: [{
        data: [tInput, tCR, tCW, tOut],
        backgroundColor: [COLORS.blue, COLORS.cyan, COLORS.purple, COLORS.coral],
        borderWidth: 0
      }]
    },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      cutout: '60%',
      plugins: { legend: { position: 'bottom', labels: { color: COLORS.text2, font: { family: 'JetBrains Mono', size: 11 }, padding: 12 } } }
    }
  });
}

let chartTools = null;
function renderToolBars() {
  const ctx = document.getElementById('chart-tools').getContext('2d');
  // Aggregate tools across all sidecars
  const toolMap = {};
  if (D.sidecars) {
    for (const sc of D.sidecars) {
      const c = sc.data.cumulative;
      if (!c || !c.tools) continue;
      for (const [name, info] of Object.entries(c.tools)) {
        if (!toolMap[name]) toolMap[name] = { calls: 0, weighted: 0 };
        toolMap[name].calls += info.calls||0;
        toolMap[name].weighted += info.weighted||0;
      }
    }
  }
  const sorted = Object.entries(toolMap).sort((a,b) => b[1].weighted - a[1].weighted);
  if (chartTools) chartTools.destroy();
  chartTools = new Chart(ctx, {
    type: 'bar',
    data: {
      labels: sorted.map(e => e[0]),
      datasets: [{
        label: 'Weighted Tokens',
        data: sorted.map(e => e[1].weighted),
        backgroundColor: sorted.map((_,i) => [COLORS.cyan, COLORS.blue, COLORS.purple, COLORS.amber, COLORS.coral, COLORS.green][i%6]),
        borderWidth: 0,
        borderRadius: 4
      }]
    },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      indexAxis: 'y',
      scales: {
        x: { ticks: { color: COLORS.text2, callback: v => fmtK(v), font: { family: 'JetBrains Mono', size: 10 } }, grid: { color: 'rgba(136,146,176,0.07)' } },
        y: { ticks: { color: COLORS.text2, font: { family: 'JetBrains Mono', size: 10 } }, grid: { display: false } }
      },
      plugins: { legend: { display: false } }
    }
  });
}

let chartCostBrkdwn = null;
function renderCostBreakdown() {
  const ctx = document.getElementById('chart-cost-breakdown').getContext('2d');
  const labels = [];
  const agentD = [], toolD = [], chatD = [];
  if (D.sidecars) {
    for (const sc of D.sidecars) {
      const c = sc.data.cumulative;
      if (!c) continue;
      labels.push(shortSid(sc.id));
      agentD.push(c.agent_cost||0);
      toolD.push(c.tool_cost||0);
      chatD.push(c.chat_cost||0);
    }
  }
  if (chartCostBrkdwn) chartCostBrkdwn.destroy();
  chartCostBrkdwn = new Chart(ctx, {
    type: 'bar',
    data: {
      labels,
      datasets: [
        { label: 'Agent', data: agentD, backgroundColor: COLORS.purple, borderRadius: 4 },
        { label: 'Tool', data: toolD, backgroundColor: COLORS.cyan, borderRadius: 4 },
        { label: 'Chat', data: chatD, backgroundColor: COLORS.blue, borderRadius: 4 }
      ]
    },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      scales: {
        x: { stacked: true, ticks: { color: COLORS.text2, font: { family: 'JetBrains Mono', size: 10 } }, grid: { display: false } },
        y: { stacked: true, ticks: { color: COLORS.text2, callback: v => '$'+v, font: { family: 'JetBrains Mono', size: 10 } }, grid: { color: 'rgba(136,146,176,0.07)' } }
      },
      plugins: { legend: { labels: { color: COLORS.text2, font: { family: 'JetBrains Mono', size: 11 } } } }
    }
  });
}


// Derive a session summary: project name + duration (fallback: tool heuristic)
function sessionSummary(sid, firstTs, lastTs) {
  let sc = null;
  let project = '';
  if (D.sidecars) {
    sc = D.sidecars.find(s => s.id.startsWith(sid) || sid.startsWith(s.id.substring(0,8)));
    if (sc && sc.data.project) {
      const slug = sc.data.project;
      const parts = slug.replace(/^[A-Z]--/, '').split('-');
      project = parts[parts.length - 1] || slug;
    }
  }

  // Fallback: infer activity from top tools when no project
  if (!project && sc && sc.data.cumulative && sc.data.cumulative.tools) {
    const tools = sc.data.cumulative.tools;
    const sorted = Object.entries(tools).sort((a,b) => (b[1].calls||0) - (a[1].calls||0));
    const top = sorted.slice(0,3).map(t => t[0]);
    const has = n => top.includes(n);
    if (has('Write') && has('Edit')) project = 'Build & Refactor';
    else if (has('Write') && has('Bash')) project = 'Build & Deploy';
    else if (has('Write') && has('Read')) project = 'Build Feature';
    else if (has('Edit') && has('Read')) project = 'Refactor';
    else if (has('Edit') && has('Bash')) project = 'Fix & Test';
    else if (has('Edit') && has('Grep')) project = 'Search & Fix';
    else if (has('WebSearch') || has('WebFetch')) project = 'Research';
    else if (has('Read') && has('Glob')) project = 'Code Review';
    else if (has('Read') && has('Grep')) project = 'Investigate';
    else if (has('Bash')) project = 'System Ops';
    else if (has('Read')) project = 'Analysis';
    else if (has('Edit')) project = 'Code Changes';
    else project = top.slice(0,2).join('+');
  }

  // Compute duration
  let duration = '';
  if (firstTs && lastTs) {
    const diffMs = new Date(lastTs) - new Date(firstTs);
    const mins = Math.round(diffMs / 60000);
    if (mins < 1) duration = '<1m';
    else if (mins < 60) duration = mins + 'm';
    else {
      const hrs = Math.floor(mins / 60);
      const rm = mins % 60;
      duration = hrs + 'h' + (rm > 0 ? rm + 'm' : '');
    }
  }

  if (project && duration) return project + ' \u00b7 ' + duration;
  if (project) return project;
  if (duration) return duration;
  return '';
}

function renderSessionTable() {
  const tbody = document.getElementById('session-tbody');
  if (!D.history || D.history.length === 0) {
    tbody.innerHTML = '<tr><td colspan="8" style="color:var(--text-secondary)">No history data</td></tr>';
    return;
  }
  // Aggregate by session
  const sessions = {};
  for (const h of D.history) {
    if (!sessions[h.sid]) sessions[h.sid] = { turns: 0, cost: 0, cr: 0, cw: 0, out: 0, first: h.ts, last: h.ts };
    const s = sessions[h.sid];
    s.turns = Math.max(s.turns, h.turns||0);
    s.cost = Math.max(s.cost, h.cost||0);
    s.cr = Math.max(s.cr, h.cr||0);
    s.cw = Math.max(s.cw, h.cw||0);
    s.out = Math.max(s.out, h.out||0);
    if (h.ts < s.first) s.first = h.ts;
    if (h.ts > s.last) s.last = h.ts;
  }
  tbody.innerHTML = Object.entries(sessions).map(([sid, s]) => {
    const summary = sessionSummary(sid, s.first, s.last);
    return '<tr><td>'+shortSid(sid)+'</td><td style="color:var(--accent-purple)">'+summary+'</td><td>'+fmt(s.turns)+'</td><td>'+fmtCost(s.cost)+'</td>' +
    '<td>'+fmtK(s.cr)+'</td><td>'+fmtK(s.out)+'</td>' +
    '<td>'+fmtTime(s.first)+'</td><td>'+fmtTime(s.last)+'</td></tr>';
  }).join('');
}

function renderConfigCard() {
  const cfg = D.statuslineConfig || { theme: 'catppuccin-mocha', style: 'powerline', segments: {} };
  document.getElementById('cfg-theme').value = cfg.theme || 'catppuccin-mocha';
  document.getElementById('cfg-style').value = cfg.style || 'powerline';

  const grid = document.getElementById('seg-grid');
  grid.innerHTML = SEGMENTS.map(name => {
    const seg = (cfg.segments && cfg.segments[name]) || { enabled: true, priority: 5 };
    const checked = seg.enabled !== false ? 'checked' : '';
    const pri = seg.priority || 5;
    return '<div class="seg-item">' +
      '<label class="toggle"><input type="checkbox" data-seg="'+name+'" '+checked+'><span class="slider"></span></label>' +
      '<span style="flex:1">'+name+'</span>' +
      '<input type="number" data-seg-pri="'+name+'" value="'+pri+'" min="1" max="10" title="Priority">' +
    '</div>';
  }).join('');
}

function renderLastTurn() {
  const el = document.getElementById('last-turn-detail');
  // Find sidecar with highest cost
  let latest = null;
  if (D.sidecars) {
    for (const sc of D.sidecars) {
      if (!latest || (sc.data.cumulative && sc.data.cumulative.cost > (latest.data.cumulative ? latest.data.cumulative.cost : 0))) {
        latest = sc;
      }
    }
  }
  if (!latest || !latest.data.lastTurn) {
    el.innerHTML = '<div style="color:var(--text-secondary);font-size:13px">No turn data</div>';
    return;
  }
  const lt = latest.data.lastTurn;
  const rows = [
    ['Session', shortSid(latest.id)],
    ['Type', lt.type || '--'],
    ['Model', lt.model || '--'],
    ['Cost', fmtCost(lt.cost)],
    ['Input', fmt(lt.input)],
    ['Cache Read', fmtK(lt.cache_read)],
    ['Cache Write', fmtK(lt.cache_write)],
    ['Output', fmt(lt.output)],
    ['Tools', lt.tools ? lt.tools.join(', ') : 'none']
  ];
  el.innerHTML = rows.map(([k,v]) =>
    '<div class="detail-row"><span class="key">'+k+'</span><span class="val">'+v+'</span></div>'
  ).join('');
}

// ────── Actions ──────
async function refreshData() {
  try {
    const res = await fetch('/api/data');
    D = await res.json();
    render();
    showToast('Data refreshed');
  } catch (e) {
    showToast('Failed to fetch data', true);
  }
}

async function saveConfig() {
  const cfg = {
    theme: document.getElementById('cfg-theme').value,
    style: document.getElementById('cfg-style').value,
    segments: {},
    options: D.statuslineConfig ? D.statuslineConfig.options : { contextBarWidth: 15, compactThreshold: 100, minWidth: 60 }
  };
  for (const name of SEGMENTS) {
    const toggle = document.querySelector('[data-seg="'+name+'"]');
    const pri = document.querySelector('[data-seg-pri="'+name+'"]');
    cfg.segments[name] = {
      enabled: toggle ? toggle.checked : true,
      priority: pri ? parseInt(pri.value) || 5 : 5
    };
  }
  try {
    const res = await fetch('/api/config', { method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify(cfg) });
    if (res.ok) {
      showToast('Saved. Takes effect next prompt.');
      D.statuslineConfig = cfg;
    } else {
      const err = await res.json().catch(() => ({}));
      showToast('Save failed: ' + (err.error || res.statusText), true);
    }
  } catch (e) {
    showToast('Save failed', true);
  }
}

async function refreshUsage() {
  try {
    const res = await fetch('/api/usage', { method: 'POST' });
    if (res.ok) {
      const data = await res.json();
      D.liveUsage = data;
      render();
      showToast('Rate limits refreshed');
    } else {
      showToast('Refresh failed', true);
    }
  } catch(e) {
    showToast('Refresh failed', true);
  }
}

async function shutdownServer() {
  try { await fetch('/api/shutdown', { method: 'POST' }); } catch(e) {}
  document.body.innerHTML = '<div style="display:flex;align-items:center;justify-content:center;height:100vh;font-family:JetBrains Mono,monospace;color:#8892b0">Server stopped. Close this tab.</div>';
}

// ────── Init ──────
refreshData();
</script>
</body>
</html>
'@

# ══════════════════════════════════════════════════════════════════════════════
# REQUEST HANDLER
# ══════════════════════════════════════════════════════════════════════════════
try {
    while ($listener.IsListening) {
        # Use async GetContext so Ctrl+C can interrupt
        $task = $listener.GetContextAsync()
        while (-not $task.AsyncWaitHandle.WaitOne(500)) {}
        $ctx = $task.GetAwaiter().GetResult()
        $req = $ctx.Request
        $res = $ctx.Response

        $route = "$($req.HttpMethod) $($req.Url.AbsolutePath)"

        switch ($route) {
            'GET /' {
                # Read HTML from external file for hot-reload, fallback to embedded
                $html = $htmlTemplateFallback
                if (Test-Path $htmlTemplatePath) {
                    try { $html = [System.IO.File]::ReadAllText($htmlTemplatePath) } catch {}
                }
                $buf = [System.Text.Encoding]::UTF8.GetBytes($html)
                $res.ContentType = 'text/html; charset=utf-8'
                $res.ContentLength64 = $buf.Length
                $res.OutputStream.Write($buf, 0, $buf.Length)
            }
            'GET /api/data' {
                $json = Build-DashboardData
                $buf = [System.Text.Encoding]::UTF8.GetBytes($json)
                $res.ContentType = 'application/json; charset=utf-8'
                $res.ContentLength64 = $buf.Length
                $res.OutputStream.Write($buf, 0, $buf.Length)
            }
            'POST /api/config' {
                try {
                    $reader = [System.IO.StreamReader]::new($req.InputStream, [System.Text.Encoding]::UTF8)
                    $body = $reader.ReadToEnd()
                    $reader.Close()
                    # Validate and re-serialize with indentation
                    $parsed = $body | ConvertFrom-Json -ErrorAction Stop
                    $pretty = $parsed | ConvertTo-Json -Depth 10
                    [System.IO.File]::WriteAllText($slConfigPath, $pretty, (New-Object System.Text.UTF8Encoding $false))
                    $buf = [System.Text.Encoding]::UTF8.GetBytes('{"ok":true}')
                    $res.ContentType = 'application/json'
                    $res.ContentLength64 = $buf.Length
                    $res.OutputStream.Write($buf, 0, $buf.Length)
                } catch {
                    $msg = $_.Exception.Message -replace '"', "'"
                    $res.StatusCode = 500
                    $errBuf = [System.Text.Encoding]::UTF8.GetBytes("{`"error`":`"$msg`"}")
                    $res.ContentType = 'application/json'
                    $res.ContentLength64 = $errBuf.Length
                    $res.OutputStream.Write($errBuf, 0, $errBuf.Length)
                }
            }
            'POST /api/usage' {
                try {
                    $credsPath = "$env:USERPROFILE\.claude\.credentials.json"
                    $creds = Get-Content $credsPath -Raw | ConvertFrom-Json -ErrorAction Stop
                    $token = $creds.claudeAiOauth.accessToken
                    $apiHeaders = @{
                        "Authorization" = "Bearer $token"
                        "User-Agent" = "periscope-telemetry"
                        "Accept" = "application/json"
                        "anthropic-version" = "2023-06-01"
                        "anthropic-beta" = "oauth-2025-04-20"
                    }
                    $resp = Invoke-RestMethod -Uri "https://api.anthropic.com/api/oauth/usage" -Headers $apiHeaders -Method Get -TimeoutSec 8

                    $result = @{
                        pct5hr = if ($resp.five_hour.utilization -ne $null) { [math]::Round($resp.five_hour.utilization) } else { -1 }
                        pctWeekly = if ($resp.seven_day.utilization -ne $null) { [math]::Round($resp.seven_day.utilization) } else { -1 }
                        pctSonnet = if ($resp.seven_day_sonnet.utilization -ne $null) { [math]::Round($resp.seven_day_sonnet.utilization) } else { -1 }
                        reset5hr = if ($resp.five_hour.resets_at) { $resp.five_hour.resets_at } else { '' }
                        resetWeekly = if ($resp.seven_day.resets_at) { $resp.seven_day.resets_at } else { '' }
                        resetSonnet = if ($resp.seven_day_sonnet.resets_at) { $resp.seven_day_sonnet.resets_at } else { '' }
                        fetched_at = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
                    }

                    # Update cache file for statusline/display hook
                    $cachePath = Join-Path $stateDir "usage-api-cache.json"
                    $result | ConvertTo-Json | Set-Content $cachePath -Force

                    $jsonOut = $result | ConvertTo-Json -Compress
                    $buf = [System.Text.Encoding]::UTF8.GetBytes($jsonOut)
                    $res.ContentType = 'application/json'
                    $res.ContentLength64 = $buf.Length
                    $res.OutputStream.Write($buf, 0, $buf.Length)
                } catch {
                    $res.StatusCode = 500
                    $errBuf = [System.Text.Encoding]::UTF8.GetBytes("{`"error`":`"$($_.Exception.Message)`"}")
                    $res.ContentType = 'application/json'
                    $res.ContentLength64 = $errBuf.Length
                    $res.OutputStream.Write($errBuf, 0, $errBuf.Length)
                }
            }
            'GET /api/pricing' {
                # Fetch Claude model pricing from LiteLLM (cached 24h)
                $pricingCachePath = Join-Path $stateDir "litellm-pricing-cache.json"
                $pricingJson = $null
                if (Test-Path $pricingCachePath) {
                    try {
                        $pc = Get-Content $pricingCachePath -Raw | ConvertFrom-Json -ErrorAction Stop
                        $age = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() - $pc.fetched_at
                        if ($age -lt 86400) { $pricingJson = $pc.data | ConvertTo-Json -Depth 5 -Compress }
                    } catch {}
                }
                if (-not $pricingJson) {
                    try {
                        $raw = Invoke-RestMethod -Uri "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json" -TimeoutSec 10
                        # Filter to anthropic claude models, extract pricing fields
                        $models = @{}
                        foreach ($prop in $raw.PSObject.Properties) {
                            $k = $prop.Name
                            if ($k -notmatch '^claude-' -or $k -match 'bedrock|vertex') { continue }
                            $v = $prop.Value
                            $models[$k] = @{
                                input = if ($v.input_cost_per_token) { [math]::Round([double]$v.input_cost_per_token * 1e6, 2) } else { 0 }
                                cache_read = if ($v.cache_read_input_token_cost) { [math]::Round([double]$v.cache_read_input_token_cost * 1e6, 2) } else { 0 }
                                cache_write = if ($v.cache_creation_input_token_cost) { [math]::Round([double]$v.cache_creation_input_token_cost * 1e6, 2) } else { 0 }
                                output = if ($v.output_cost_per_token) { [math]::Round([double]$v.output_cost_per_token * 1e6, 2) } else { 0 }
                                max_input = if ($v.max_input_tokens) { [int]$v.max_input_tokens } else { 0 }
                                max_output = if ($v.max_output_tokens) { [int]$v.max_output_tokens } else { 0 }
                            }
                        }
                        $cacheObj = @{ fetched_at = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds(); data = $models }
                        $cacheObj | ConvertTo-Json -Depth 5 | Set-Content $pricingCachePath -Force
                        $pricingJson = $models | ConvertTo-Json -Depth 5 -Compress
                    } catch {
                        # Fallback to stale cache
                        if (Test-Path $pricingCachePath) {
                            try {
                                $pc = Get-Content $pricingCachePath -Raw | ConvertFrom-Json -ErrorAction Stop
                                $pricingJson = $pc.data | ConvertTo-Json -Depth 5 -Compress
                            } catch {}
                        }
                        if (-not $pricingJson) { $pricingJson = '{}' }
                    }
                }
                $buf = [System.Text.Encoding]::UTF8.GetBytes($pricingJson)
                $res.ContentType = 'application/json; charset=utf-8'
                $res.ContentLength64 = $buf.Length
                $res.OutputStream.Write($buf, 0, $buf.Length)
            }
            'POST /api/shutdown' {
                $buf = [System.Text.Encoding]::UTF8.GetBytes('{"ok":true}')
                $res.ContentType = 'application/json'
                $res.ContentLength64 = $buf.Length
                $res.OutputStream.Write($buf, 0, $buf.Length)
                $res.Close()
                $listener.Stop()
                Write-Host "  Server stopped. You can close this window." -ForegroundColor DarkGray
                exit 0
            }
            default {
                $res.StatusCode = 404
                $buf = [System.Text.Encoding]::UTF8.GetBytes('Not Found')
                $res.ContentLength64 = $buf.Length
                $res.OutputStream.Write($buf, 0, $buf.Length)
            }
        }

        $res.Close()
    }
} finally {
    if ($listener.IsListening) { $listener.Stop() }
    $listener.Close()
    Write-Host "  Server shut down. You can close this window." -ForegroundColor DarkGray
}
