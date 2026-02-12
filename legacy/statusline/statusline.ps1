# statusline.ps1 — Modular segment-based statusline with themes and styles
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$jsonInput = [Console]::In.ReadToEnd() | ConvertFrom-Json

# ══════════════════════════════════════════════════════════════════════════════
# THEMES — ANSI 256-color numbers (used as 38;5;N fg or 48;5;N bg)
# bg1/bg2: alternating dark backgrounds for powerline arrow visibility
# ══════════════════════════════════════════════════════════════════════════════
$Themes = @{
    'catppuccin-mocha' = @{
        bg1 = 235; bg2 = 237; fg = 255; dim = 60
        blue = 117; purple = 183; cyan = 117
        green = 150; yellow = 222; red = 210; peach = 216
    }
    'dracula' = @{
        bg1 = 235; bg2 = 237; fg = 255; dim = 60
        blue = 117; purple = 141; cyan = 159
        green = 120; yellow = 228; red = 210; peach = 212
    }
    'tokyo-night' = @{
        bg1 = 235; bg2 = 237; fg = 255; dim = 60
        blue = 111; purple = 141; cyan = 116
        green = 158; yellow = 222; red = 210; peach = 216
    }
    'nord' = @{
        bg1 = 235; bg2 = 237; fg = 252; dim = 60
        blue = 110; purple = 139; cyan = 110
        green = 150; yellow = 222; red = 174; peach = 216
    }
    'gruvbox' = @{
        bg1 = 236; bg2 = 238; fg = 223; dim = 245
        blue = 109; purple = 175; cyan = 108
        green = 142; yellow = 214; red = 167; peach = 208
    }
}

# ══════════════════════════════════════════════════════════════════════════════
# ANSI HELPERS
# ══════════════════════════════════════════════════════════════════════════════
$e = [char]27
$r = "$e[0m"

function Fg($n) { return "$e[38;5;${n}m" }
function Bg($n) { return "$e[48;5;${n}m" }

function Rate-Color($pct, $theme) {
    if ($pct -lt 50) { return $theme.green }
    if ($pct -lt 75) { return $theme.yellow }
    return $theme.red
}

# ══════════════════════════════════════════════════════════════════════════════
# DATA LOADERS
# ══════════════════════════════════════════════════════════════════════════════
function Load-Sidecar {
    $stateDir = "$env:USERPROFILE\.claude\hooks\cost-state"
    $result = @{ turns = 0; cachePct = 0; tools = @(); hasSidecar = $false; cost = 0 }
    try {
        $sidecar = Get-ChildItem $stateDir -Filter "*.json" -ErrorAction Stop |
            Where-Object { $_.Name -ne 'usage-config.json' -and $_.Name -ne 'usage-api-cache.json' -and $_.Name -ne 'profile-cache.json' } |
            Sort-Object LastWriteTime -Descending |
            Select-Object -First 1
        if ($sidecar) {
            $state = Get-Content $sidecar.FullName -Raw | ConvertFrom-Json -ErrorAction Stop
            $c = $state.cumulative
            $result.turns = $c.agent_calls + $c.tool_calls + $c.chat_calls
            $result.cost = if ($c.cost) { $c.cost } else { 0 }
            $totalIn = $c.input + $c.cache_read
            if ($totalIn -gt 0) {
                $result.cachePct = [math]::Round(($c.cache_read / $totalIn) * 100)
            }
            if ($state.lastTurn.tools -and $state.lastTurn.tools.Count -gt 0) {
                $toolCounts = @{}
                foreach ($tn in $state.lastTurn.tools) {
                    if ($toolCounts.ContainsKey($tn)) { $toolCounts[$tn]++ }
                    else { $toolCounts[$tn] = 1 }
                }
                $toolParts = @()
                foreach ($kv in $toolCounts.GetEnumerator() | Sort-Object Value -Descending) {
                    $toolParts += if ($kv.Value -gt 1) { "$($kv.Key)x$($kv.Value)" } else { $kv.Key }
                }
                $result.tools = $toolParts
            }
            $result.hasSidecar = $true
        }
    } catch {}
    return $result
}

function Compute-RateLimits {
    $cachePath = "$env:USERPROFILE\.claude\hooks\cost-state\usage-api-cache.json"
    $credsPath = "$env:USERPROFILE\.claude\.credentials.json"
    $result = @{ pct5hr = -1; pctWeekly = -1; pctSonnet = -1; reset5hr = ''; resetWeekly = ''; resetSonnet = '' }

    # Use cached data if fresh (< 30 seconds old)
    if (Test-Path $cachePath) {
        try {
            $cache = Get-Content $cachePath -Raw | ConvertFrom-Json -ErrorAction Stop
            $age = ([DateTimeOffset]::UtcNow.ToUnixTimeSeconds()) - $cache.fetched_at
            if ($age -lt 30) {
                $result.pct5hr = $cache.pct5hr
                $result.pctWeekly = $cache.pctWeekly
                $result.pctSonnet = $cache.pctSonnet
                $result.reset5hr = $cache.reset5hr
                $result.resetWeekly = $cache.resetWeekly
                $result.resetSonnet = $cache.resetSonnet
                return $result
            }
        } catch {}
    }

    # Read OAuth token
    if (-not (Test-Path $credsPath)) { return $result }
    try {
        $creds = Get-Content $credsPath -Raw | ConvertFrom-Json -ErrorAction Stop
        $token = $creds.claudeAiOauth.accessToken
        if (-not $token) { return $result }

        # Call Anthropic usage API
        $headers = @{
            "Authorization" = "Bearer $token"
            "User-Agent" = "claude-code-statusline"
            "Accept" = "application/json"
            "anthropic-version" = "2023-06-01"
            "anthropic-beta" = "oauth-2025-04-20"
        }
        $resp = Invoke-RestMethod -Uri "https://api.anthropic.com/api/oauth/usage" -Headers $headers -Method Get -TimeoutSec 5

        $result.pct5hr = if ($resp.five_hour.utilization -ne $null) { [math]::Round($resp.five_hour.utilization) } else { -1 }
        $result.pctWeekly = if ($resp.seven_day.utilization -ne $null) { [math]::Round($resp.seven_day.utilization) } else { -1 }
        $result.pctSonnet = if ($resp.seven_day_sonnet.utilization -ne $null) { [math]::Round($resp.seven_day_sonnet.utilization) } else { -1 }
        $result.reset5hr = if ($resp.five_hour.resets_at) { $resp.five_hour.resets_at } else { '' }
        $result.resetWeekly = if ($resp.seven_day.resets_at) { $resp.seven_day.resets_at } else { '' }
        $result.resetSonnet = if ($resp.seven_day_sonnet.resets_at) { $resp.seven_day_sonnet.resets_at } else { '' }

        # Cache result
        @{
            pct5hr = $result.pct5hr
            pctWeekly = $result.pctWeekly
            pctSonnet = $result.pctSonnet
            reset5hr = $result.reset5hr
            resetWeekly = $result.resetWeekly
            resetSonnet = $result.resetSonnet
            fetched_at = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
        } | ConvertTo-Json | Set-Content $cachePath -Force
    } catch {
        # API failed — try stale cache as fallback
        if (Test-Path $cachePath) {
            try {
                $cache = Get-Content $cachePath -Raw | ConvertFrom-Json -ErrorAction Stop
                $result.pct5hr = $cache.pct5hr
                $result.pctWeekly = $cache.pctWeekly
                $result.pctSonnet = $cache.pctSonnet
            } catch {}
        }
    }
    return $result
}

# ══════════════════════════════════════════════════════════════════════════════
# SEGMENT FUNCTIONS — each returns @{ text; color; empty }
# color = semantic ANSI 256 number (renderers decide fg vs bg usage)
# ══════════════════════════════════════════════════════════════════════════════
function Seg-Dir($data, $theme) {
    $dir = $data.jsonInput.workspace.current_dir -replace '^C:\\Users\\[^\\]+', '~'
    @{ text = " $dir"; color = $theme.blue; empty = $false }
}

function Seg-Git($data, $theme) {
    $branch = ''
    try {
        $gitOut = git -C $data.jsonInput.workspace.current_dir branch --show-current 2>$null
        if ($gitOut) {
            $dirty = git -C $data.jsonInput.workspace.current_dir status --porcelain 2>$null
            $branch = $gitOut + $(if ($dirty) { '*' } else { '' })
        }
    } catch {}
    if (-not $branch) { return @{ text = ''; color = 0; empty = $true } }
    @{ text = " $branch"; color = $theme.purple; empty = $false }
}

function Seg-Model($data, $theme) {
    $model = switch -Wildcard ($data.jsonInput.model.model_id) {
        '*opus*'   { 'opus' }
        '*sonnet*' { 'sonnet' }
        '*haiku*'  { 'haiku' }
        default    { $data.jsonInput.model.display_name }
    }
    $icon = [char]::ConvertFromUtf32(0xF09A1)
    @{ text = "$icon $model"; color = $theme.cyan; empty = $false }
}

function Seg-Turns($data, $theme) {
    if ($data.sidecar.turns -le 0) { return @{ text = ''; color = 0; empty = $true } }
    @{ text = " t:$($data.sidecar.turns)"; color = $theme.cyan; empty = $false }
}

function Seg-Rate5hr($data, $theme) {
    if ($data.rates.pct5hr -lt 0) { return @{ text = ''; color = 0; empty = $true } }
    $col = Rate-Color $data.rates.pct5hr $theme
    @{ text = " 5h:$($data.rates.pct5hr)%"; color = $col; empty = $false }
}

function Seg-RateWeekly($data, $theme) {
    if ($data.rates.pctWeekly -lt 0) { return @{ text = ''; color = 0; empty = $true } }
    $col = Rate-Color $data.rates.pctWeekly $theme
    @{ text = " wk:$($data.rates.pctWeekly)%"; color = $col; empty = $false }
}

function Seg-Cache($data, $theme) {
    if (-not $data.sidecar.hasSidecar) { return @{ text = ''; color = 0; empty = $true } }
    @{ text = " $($data.sidecar.cachePct)%"; color = $theme.green; empty = $false }
}

function Seg-Tools($data, $theme) {
    if ($data.sidecar.tools.Count -eq 0) { return @{ text = ''; color = 0; empty = $true } }
    $list = $data.sidecar.tools -join ' '
    @{ text = " [$list]"; color = $theme.peach; empty = $false }
}

function Seg-Context($data, $theme) {
    $ctxPct = if ($data.jsonInput.context_window.used_percentage) {
        [math]::Round($data.jsonInput.context_window.used_percentage)
    } else { 0 }
    $barW = $data.options.contextBarWidth
    $filled = [math]::Round($barW * $ctxPct / 100)
    $emptyW = $barW - $filled
    $barCol = Rate-Color $ctxPct $theme
    $filledStr = [string]::new([char]0x2588, $filled)
    $emptyStr = [string]::new([char]0x2591, $emptyW)
    @{
        text = " $filledStr$emptyStr $ctxPct%"
        color = $barCol
        empty = $false
        _barCol = $barCol; _dimCol = $theme.dim
        _filledStr = $filledStr; _emptyStr = $emptyStr; _pct = $ctxPct
    }
}

function Seg-Vim($data, $theme) {
    $mode = $data.jsonInput.vim_mode
    if (-not $mode -or -not $mode.mode) { return @{ text = ''; color = 0; empty = $true } }
    $modeText = $mode.mode.ToUpper()
    $col = if ($modeText -eq 'INSERT') { $theme.green } else { $theme.yellow }
    @{ text = " $modeText"; color = $col; empty = $false }
}

function Seg-Cost($data, $theme) {
    if ($data.sidecar.cost -le 0) { return @{ text = ''; color = 0; empty = $true } }
    $val = [math]::Round($data.sidecar.cost, 2)
    @{ text = " `$$val"; color = $theme.yellow; empty = $false }
}

function Seg-RateSonnet($data, $theme) {
    if ($data.rates.pctSonnet -lt 0) { return @{ text = ''; color = 0; empty = $true } }
    $col = Rate-Color $data.rates.pctSonnet $theme
    @{ text = " sn:$($data.rates.pctSonnet)%"; color = $col; empty = $false }
}

function Seg-Reset($data, $theme) {
    $now = [DateTimeOffset]::UtcNow
    $nearest = $null
    foreach ($r in @($data.rates.reset5hr, $data.rates.resetWeekly, $data.rates.resetSonnet)) {
        if (-not $r) { continue }
        try {
            $dt = [DateTimeOffset]::Parse($r)
            $diff = ($dt - $now).TotalMinutes
            if ($diff -gt 0 -and (-not $nearest -or $diff -lt $nearest)) { $nearest = $diff }
        } catch {}
    }
    if (-not $nearest) { return @{ text = ''; color = 0; empty = $true } }
    $hrs = [math]::Floor($nearest / 60)
    $mins = [math]::Round($nearest % 60)
    $display = if ($hrs -gt 0) { "${hrs}h${mins}m" } else { "${mins}m" }
    @{ text = " rst:$display"; color = $theme.cyan; empty = $false }
}

function Seg-Proj($data, $theme) {
    $pct = $data.rates.pct5hr
    if ($pct -lt 0) { return @{ text = ''; color = 0; empty = $true } }
    $reset = $data.rates.reset5hr
    if (-not $reset) { return @{ text = ''; color = 0; empty = $true } }
    try {
        $now = [DateTimeOffset]::UtcNow
        $resetDt = [DateTimeOffset]::Parse($reset)
        $windowStart = $resetDt.AddHours(-5)
        $elapsed = ($now - $windowStart).TotalHours
        if ($elapsed -le 0.05) { return @{ text = ''; color = 0; empty = $true } }
        $remaining = ($resetDt - $now).TotalHours
        if ($remaining -le 0) { return @{ text = ''; color = 0; empty = $true } }
        $rate = $pct / $elapsed
        $projected = [math]::Round($pct + ($rate * $remaining))
        $col = if ($projected -lt 50) { $theme.green } elseif ($projected -lt 80) { $theme.yellow } else { $theme.red }
        @{ text = " pj:${projected}%"; color = $col; empty = $false }
    } catch { return @{ text = ''; color = 0; empty = $true } }
}

# ══════════════════════════════════════════════════════════════════════════════
# SEGMENT DISPATCHER
# ══════════════════════════════════════════════════════════════════════════════
function Get-Segment($name, $data, $theme) {
    switch ($name) {
        'dir'         { Seg-Dir $data $theme }
        'git'         { Seg-Git $data $theme }
        'model'       { Seg-Model $data $theme }
        'turns'       { Seg-Turns $data $theme }
        'rate-5hr'    { Seg-Rate5hr $data $theme }
        'rate-weekly' { Seg-RateWeekly $data $theme }
        'cache'       { Seg-Cache $data $theme }
        'tools'       { Seg-Tools $data $theme }
        'context'     { Seg-Context $data $theme }
        'vim'         { Seg-Vim $data $theme }
        'cost'        { Seg-Cost $data $theme }
        'rate-sonnet' { Seg-RateSonnet $data $theme }
        'reset'       { Seg-Reset $data $theme }
        'proj'        { Seg-Proj $data $theme }
        default       { @{ text = ''; color = 0; empty = $true } }
    }
}

# ══════════════════════════════════════════════════════════════════════════════
# STYLE RENDERERS
# ══════════════════════════════════════════════════════════════════════════════
function Render-Powerline($segments, $theme) {
    $out = ''
    $sep = [char]0xE0B0  # Powerline arrow
    for ($i = 0; $i -lt $segments.Count; $i++) {
        $seg = $segments[$i]
        $sbg = $seg.bg
        # Segment body: colored bg, bright fg text
        $out += "$(Bg $sbg)$(Fg $seg.color)$($seg.text) $r"
        # Arrow separator: prev-bg as fg on next-bg (or terminal default)
        if ($i -lt $segments.Count - 1) {
            $nextBg = $segments[$i + 1].bg
            $out += "$(Fg $sbg)$(Bg $nextBg)$sep$r"
        } else {
            $out += "$(Fg $sbg)$sep$r"
        }
    }
    return $out
}

function Render-Plain($segments, $theme) {
    $parts = @()
    foreach ($seg in $segments) {
        if ($seg._filledStr) {
            $parts += "$(Fg $seg._barCol)$($seg._filledStr)$(Fg $seg._dimCol)$($seg._emptyStr)$r $(Fg $seg._barCol)$($seg._pct)%$r"
        } else {
            $parts += "$(Fg $seg.color)$($seg.text)$r"
        }
    }
    $pipeSep = " $(Fg $theme.dim)|$r "
    return $parts -join $pipeSep
}

function Render-Minimal($segments, $theme) {
    $parts = @()
    foreach ($seg in $segments) {
        if ($seg._filledStr) {
            $parts += "$(Fg $seg._barCol)$($seg._pct)%$r"
        } else {
            $parts += "$(Fg $seg.color)$($seg.text)$r"
        }
    }
    return $parts -join ' '
}

# ══════════════════════════════════════════════════════════════════════════════
# MAIN PIPELINE
# ══════════════════════════════════════════════════════════════════════════════
$cfgPath = Join-Path (Split-Path $MyInvocation.MyCommand.Path) "statusline-config.json"
$slCfg = @{ theme = 'catppuccin-mocha'; style = 'powerline'; segments = @{}; options = @{ contextBarWidth = 15; compactThreshold = 100; minWidth = 60 } }
if (Test-Path $cfgPath) {
    try {
        $loaded = Get-Content $cfgPath -Raw | ConvertFrom-Json -ErrorAction Stop
        if ($loaded.theme) { $slCfg.theme = $loaded.theme }
        if ($loaded.style) { $slCfg.style = $loaded.style }
        if ($loaded.segments) { $slCfg.segments = $loaded.segments }
        if ($loaded.options) {
            if ($loaded.options.contextBarWidth) { $slCfg.options.contextBarWidth = $loaded.options.contextBarWidth }
            if ($loaded.options.compactThreshold) { $slCfg.options.compactThreshold = $loaded.options.compactThreshold }
            if ($loaded.options.minWidth) { $slCfg.options.minWidth = $loaded.options.minWidth }
        }
    } catch {}
}

# Resolve theme
$theme = $Themes[$slCfg.theme]
if (-not $theme) { $theme = $Themes['catppuccin-mocha'] }

# Load data sources
$sidecar = Load-Sidecar
$rates = Compute-RateLimits

$data = @{
    jsonInput = $jsonInput
    sidecar   = $sidecar
    rates     = $rates
    options   = $slCfg.options
}

# Segment order
$segOrder = @('dir','git','model','turns','rate-5hr','rate-weekly','rate-sonnet','cost','reset','proj','cache','tools','context','vim')

# Terminal width
$termWidth = 120
try { $termWidth = [Console]::WindowWidth } catch {}

# Build segments — filter by enabled, priority, and width
# Assign alternating bg colors for powerline arrow visibility
$rendered = @()
$bgToggle = $false
foreach ($name in $segOrder) {
    $segCfg = $slCfg.segments.$name
    $enabled = $true
    $priority = 5
    if ($segCfg) {
        if ($null -ne $segCfg.enabled) { $enabled = $segCfg.enabled }
        if ($null -ne $segCfg.priority) { $priority = $segCfg.priority }
    }
    if (-not $enabled) { continue }

    # Priority filtering by width
    if ($termWidth -lt $slCfg.options.minWidth -and $priority -gt 3) { continue }
    if ($termWidth -lt $slCfg.options.compactThreshold -and $priority -gt 6) { continue }

    $seg = Get-Segment $name $data $theme
    if (-not $seg.empty) {
        $seg.bg = if ($bgToggle) { $theme.bg2 } else { $theme.bg1 }
        $seg.priority = $priority
        $seg.name = $name
        $rendered += $seg
        $bgToggle = -not $bgToggle
    }
}

# Render with selected style
$output = switch ($slCfg.style) {
    'powerline' { Render-Powerline $rendered $theme }
    'plain'     { Render-Plain $rendered $theme }
    'minimal'   { Render-Minimal $rendered $theme }
    default     { Render-Powerline $rendered $theme }
}

[Console]::Write($output)
