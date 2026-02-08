# cost-tracker-display.ps1 - UserPromptSubmit hook: inject telemetry context
# Fires before each turn. Outputs additionalContext for Claude to see.

$ErrorActionPreference = 'SilentlyContinue'

try {
    $stdinData = [System.Console]::In.ReadToEnd()
    $payload = $stdinData | ConvertFrom-Json -ErrorAction Stop
} catch { exit 0 }

$sessionId = $payload.session_id
if (-not $sessionId) { exit 0 }

$statePath = "$env:USERPROFILE\.claude\hooks\cost-state\$sessionId.json"
if (-not (Test-Path $statePath)) {
    $output = @{
        hookSpecificOutput = @{
            hookEventName = "UserPromptSubmit"
            additionalContext = "TELEMETRY - First turn - tracking starts next message."
        }
    } | ConvertTo-Json -Depth 5
    [System.Console]::Out.Write($output)
    exit 0
}

try {
    $state = Get-Content $statePath -Raw | ConvertFrom-Json -ErrorAction Stop
} catch { exit 0 }

$c = $state.cumulative

# Cache hit rate
$totalIn = $c.input + $c.cache_read + $c.cache_write
$cacheHitPct = if ($totalIn -gt 0) { [math]::Round(($c.cache_read / $totalIn) * 100, 1) } else { 0 }

$totalCalls = $c.agent_calls + $c.tool_calls + $c.chat_calls

# Line 1: Session summary + top tools
$toolSummary = ''
if ($c.tools) {
    # Sort tools by weighted tokens descending, show top 5
    $toolEntries = @()
    foreach ($prop in $c.tools.PSObject.Properties) {
        $toolEntries += @{ name = $prop.Name; calls = $prop.Value.calls; weighted = $prop.Value.weighted }
    }
    $sorted = $toolEntries | Sort-Object { $_.weighted } -Descending | Select-Object -First 5
    $parts = @()
    foreach ($t in $sorted) {
        $w = if ($t.weighted -ge 1000000) { "$([math]::Round($t.weighted/1000000,1))M" }
             elseif ($t.weighted -ge 1000) { "$([math]::Round($t.weighted/1000))K" }
             else { "$([math]::Round($t.weighted))" }
        $parts += "$($t.name):$($t.calls)x($w)"
    }
    if ($parts.Count -gt 0) { $toolSummary = "`nTools: $($parts -join ' | ')" }
}
$context = "TELEMETRY: $totalCalls calls (agent:$($c.agent_calls) tool:$($c.tool_calls) chat:$($c.chat_calls)) | cache:${cacheHitPct}%$toolSummary"

# Line 2: Rate limits — fetch from Anthropic API (cached 30s)
$cachePath = "$env:USERPROFILE\.claude\hooks\cost-state\usage-api-cache.json"
$credsPath = "$env:USERPROFILE\.claude\.credentials.json"
$p5 = -1; $pw = -1

# Try cache first
$needsFetch = $true
if (Test-Path $cachePath) {
    try {
        $cache = Get-Content $cachePath -Raw | ConvertFrom-Json -ErrorAction Stop
        $age = ([DateTimeOffset]::UtcNow.ToUnixTimeSeconds()) - $cache.fetched_at
        if ($age -lt 30) {
            $p5 = $cache.pct5hr; $pw = $cache.pctWeekly
            $needsFetch = $false
        }
    } catch {}
}

if ($needsFetch -and (Test-Path $credsPath)) {
    try {
        $creds = Get-Content $credsPath -Raw | ConvertFrom-Json -ErrorAction Stop
        $token = $creds.claudeAiOauth.accessToken
        if ($token) {
            $headers = @{
                "Authorization" = "Bearer $token"
                "User-Agent" = "claude-code-hook"
                "Accept" = "application/json"
                "anthropic-version" = "2023-06-01"
                "anthropic-beta" = "oauth-2025-04-20"
            }
            $resp = Invoke-RestMethod -Uri "https://api.anthropic.com/api/oauth/usage" -Headers $headers -Method Get -TimeoutSec 3
            $p5 = if ($null -ne $resp.five_hour.utilization) { [math]::Round($resp.five_hour.utilization) } else { -1 }
            $pw = if ($null -ne $resp.seven_day.utilization) { [math]::Round($resp.seven_day.utilization) } else { -1 }
            $ps = if ($null -ne $resp.seven_day_sonnet.utilization) { [math]::Round($resp.seven_day_sonnet.utilization) } else { -1 }
            @{
                pct5hr = $p5; pctWeekly = $pw; pctSonnet = $ps
                reset5hr = $(if ($resp.five_hour.resets_at) { $resp.five_hour.resets_at } else { '' })
                resetWeekly = $(if ($resp.seven_day.resets_at) { $resp.seven_day.resets_at } else { '' })
                fetched_at = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
            } | ConvertTo-Json | Set-Content $cachePath -Force
        }
    } catch {
        # Fallback to stale cache
        if (Test-Path $cachePath) {
            try {
                $cache = Get-Content $cachePath -Raw | ConvertFrom-Json -ErrorAction Stop
                $p5 = $cache.pct5hr; $pw = $cache.pctWeekly
            } catch {}
        }
    }
}

if ($p5 -ge 0 -and $pw -ge 0) {
    function Make-Bar($pct) {
        $filled = [math]::Round(20 * $pct / 100)
        $empty = 20 - $filled
        $f = if ($filled -gt 0) { '#' * $filled } else { '' }
        $e = if ($empty -gt 0) { '-' * $empty } else { '' }
        return "[$f$e]"
    }
    $context += "`nRate limits: 5hr $(Make-Bar $p5) $p5% | Weekly $(Make-Bar $pw) $pw%"

    # ── Rate Intelligence: Record snapshots + Extrapolate to reset ──
    $limitHistPath = "$env:USERPROFILE\.claude\hooks\cost-state\limit-history.jsonl"

    # Load reset times from cache
    $r5 = ''; $rw = ''
    try {
        $cd = Get-Content $cachePath -Raw | ConvertFrom-Json -ErrorAction Stop
        $r5 = $cd.reset5hr; $rw = $cd.resetWeekly
    } catch {}

    # Record snapshot (throttled to 5min intervals)
    $shouldSnap = $true
    if (Test-Path $limitHistPath) {
        try {
            $tailLine = Get-Content $limitHistPath -Tail 1 -ErrorAction Stop
            if ($tailLine -and $tailLine.Trim().Length -gt 0) {
                $le = $tailLine | ConvertFrom-Json -ErrorAction Stop
                if (([System.DateTimeOffset]::UtcNow - [System.DateTimeOffset]::Parse($le.ts)).TotalMinutes -lt 5) {
                    $shouldSnap = $false
                }
            }
        } catch {}
    }
    if ($shouldSnap) {
        try {
            (@{ ts=[System.DateTimeOffset]::UtcNow.ToString('o'); pct5hr=$p5; pctWeekly=$pw; reset5hr=$r5; resetWeekly=$rw } | ConvertTo-Json -Compress) |
                Add-Content $limitHistPath -Encoding UTF8
        } catch {}
    }

    # Extrapolate usage to reset time
    try {
        $histTail = @(Get-Content $limitHistPath -Tail 60 -ErrorAction Stop | Where-Object { $_.Trim().Length -gt 0 })
        $pts = @()
        foreach ($hl in $histTail) {
            try {
                $he = $hl | ConvertFrom-Json -ErrorAction Stop
                $pts += @{ ts=[System.DateTimeOffset]::Parse($he.ts); p5=[double]$he.pct5hr; pw=[double]$he.pctWeekly }
            } catch { continue }
        }

        if ($pts.Count -ge 3) {
            $now = [System.DateTimeOffset]::UtcNow
            $fcParts = @()

            foreach ($w in @(@{l='5h';cur=$p5;f='p5';rs=$r5}, @{l='wk';cur=$pw;f='pw';rs=$rw})) {
                # Hours until reset
                $hrsLeft = -1
                if ($w.rs) {
                    try { $hrsLeft = ([System.DateTimeOffset]::Parse($w.rs) - $now).TotalHours } catch {}
                }
                if ($hrsLeft -le 0) { continue }

                # Build series with reset detection (big drop = window rolled over)
                $series = @(); $prev = -1
                foreach ($pt in $pts) {
                    $v = $pt[$w.f]
                    if ($null -eq $v) { continue }
                    if ($prev -ge 0 -and $v -lt ($prev - 15)) { $series = @() }
                    $series += @{ ts=$pt.ts; pct=$v }
                    $prev = $v
                }
                if ($series.Count -lt 2) { continue }

                # Current rate (last 30min window)
                $cutoff = $now.AddMinutes(-30)
                $recent = @($series | Where-Object { $_.ts -ge $cutoff })
                $curRate = 0.0
                if ($recent.Count -ge 2) {
                    $dh = ($recent[-1].ts - $recent[0].ts).TotalHours
                    if ($dh -gt 0.01) { $curRate = ($recent[-1].pct - $recent[0].pct) / $dh }
                }

                # Average rate (full series since last reset)
                $dAll = ($series[-1].ts - $series[0].ts).TotalHours
                $avgR = if ($dAll -gt 0.05) { ($series[-1].pct - $series[0].pct) / $dAll } else { 0.0 }

                # Weighted blend: 60% current, 40% average (favors recent trajectory)
                $rate = if ($curRate -gt 0 -and $avgR -gt 0) { 0.6*$curRate + 0.4*$avgR }
                        elseif ($curRate -gt 0) { $curRate }
                        elseif ($avgR -gt 0) { $avgR }
                        else { 0.0 }

                $proj = [math]::Round($w.cur + ($rate * $hrsLeft))
                $tl = if ($hrsLeft -ge 24) { "$([math]::Round($hrsLeft/24,1))d" } else { "$([math]::Round($hrsLeft,1))h" }
                $rateStr = "$([math]::Round($rate,1))%/h"

                $verdict = if ($rate -le 0) { 'idle' }
                           elseif ($proj -gt 100) { 'OVER LIMIT' }
                           elseif ($proj -gt 90)  { 'SLOW DOWN' }
                           elseif ($proj -gt 70)  { 'monitor' }
                           else { 'OK' }

                $fcParts += "$($w.l):$($w.cur)%->~${proj}%(${tl} left, ${rateStr}) $verdict"
            }

            if ($fcParts.Count -gt 0) { $context += "`nForecast: $($fcParts -join ' | ')" }
        }
    } catch {}
}

$output = @{
    hookSpecificOutput = @{
        hookEventName = "UserPromptSubmit"
        additionalContext = $context
    }
} | ConvertTo-Json -Depth 5

[System.Console]::Out.Write($output)
exit 0
