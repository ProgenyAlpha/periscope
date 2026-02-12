# cost-tracker-stop.ps1 — Stop hook: process transcript, write sidecar state
# Fires after Claude finishes each turn. Silent — no stdout needed.

$ErrorActionPreference = 'SilentlyContinue'

# Read hook payload from stdin
try {
    $stdinData = [System.Console]::In.ReadToEnd()
    $payload = $stdinData | ConvertFrom-Json -ErrorAction Stop
} catch { exit 0 }

$transcriptPath = $payload.transcript_path
$sessionId = $payload.session_id
if (-not $transcriptPath -or -not $sessionId) { exit 0 }
if (-not (Test-Path $transcriptPath)) { exit 0 }

# Extract project from transcript path: .claude/projects/<slug>/<session>.jsonl
$projectSlug = ''
$parentDir = Split-Path $transcriptPath -Parent
$parentName = Split-Path $parentDir -Leaf
if ($parentName -ne 'cost-state') {
    # Convert slug like C--Users-name-Projects-MyApp to readable path
    $projectSlug = $parentName
}

$stateDir = "$env:USERPROFILE\.claude\hooks\cost-state"
$statePath = Join-Path $stateDir "$sessionId.json"

# Model pricing ($ per million tokens) — updated 2026-02-08 from platform.claude.com/docs/en/about-claude/pricing
$pricing = @{
    'claude-opus-4-6'            = @{ input=5;    cache_read=0.50; cache_write=6.25;  output=25 }
    'claude-opus-4-5'            = @{ input=5;    cache_read=0.50; cache_write=6.25;  output=25 }
    'claude-opus-4-1'            = @{ input=15;   cache_read=1.50; cache_write=18.75; output=75 }
    'claude-sonnet-4-5-20250929' = @{ input=3;    cache_read=0.30; cache_write=3.75;  output=15 }
    'claude-haiku-4-5-20251001'  = @{ input=1;    cache_read=0.10; cache_write=1.25;  output=5  }
    'claude-haiku-3-5'           = @{ input=0.80; cache_read=0.08; cache_write=1.00;  output=4  }
}

function Get-ModelRates($model) {
    foreach ($key in $pricing.Keys) {
        if ($model -like "$key*" -or $model -eq $key) { return $pricing[$key] }
    }
    return $pricing['claude-opus-4-6']
}

function Get-TurnInfo($content) {
    $info = @{ type = "chat"; tools = @(); agents = @() }
    if (-not $content) { return $info }
    foreach ($block in $content) {
        if ($block.type -eq 'tool_use') {
            $info.tools += $block.name
            if ($block.name -eq 'Task') {
                $info.type = "agent"
                if ($block.input -and $block.input.subagent_type) {
                    $info.agents += $block.input.subagent_type
                }
            } elseif ($info.type -ne "agent") {
                $info.type = "tool"
            }
        }
    }
    return $info
}

# Load or init state
$state = $null
if (Test-Path $statePath) {
    try { $state = Get-Content $statePath -Raw | ConvertFrom-Json -ErrorAction Stop } catch {}
}
if (-not $state) {
    $state = [PSCustomObject]@{
        lastOffset  = 0
        cumulative  = [PSCustomObject]@{
            input=0; cache_read=0; cache_write=0; output=0
            cost=0.0; agent_cost=0.0; tool_cost=0.0; chat_cost=0.0
            agent_calls=0; tool_calls=0; chat_calls=0
            tools = [PSCustomObject]@{}
        }
        lastTurn    = [PSCustomObject]@{
            cost=0.0; type="chat"; model=""; input=0; cache_read=0; cache_write=0; output=0
            tools=@()
        }
    }
}
# Ensure tools property exists (upgrade from older sidecar)
if (-not $state.cumulative.PSObject.Properties['tools']) {
    $state.cumulative | Add-Member -NotePropertyName 'tools' -NotePropertyValue ([PSCustomObject]@{}) -Force
}

# Check for compaction (file got smaller)
$fileSize = (Get-Item $transcriptPath).Length
if ($state.lastOffset -gt $fileSize) {
    $state.lastOffset = 0
    $state.cumulative = [PSCustomObject]@{
        input=0; cache_read=0; cache_write=0; output=0
        cost=0.0; agent_cost=0.0; tool_cost=0.0; chat_cost=0.0
        agent_calls=0; tool_calls=0; chat_calls=0
        tools = [PSCustomObject]@{}
    }
}

# Read only new bytes since last check
try {
    $stream = [System.IO.FileStream]::new(
        $transcriptPath,
        [System.IO.FileMode]::Open,
        [System.IO.FileAccess]::Read,
        [System.IO.FileShare]::ReadWrite
    )
    $stream.Seek($state.lastOffset, [System.IO.SeekOrigin]::Begin) | Out-Null
    $reader = [System.IO.StreamReader]::new($stream)
} catch { exit 0 }

# Reset last turn for this new turn
$turnCost = 0.0
$turnType = "chat"
$turnModel = ""
$turnInput = 0
$turnCacheRead = 0
$turnCacheWrite = 0
$turnOutput = 0
$turnTools = @()

# Find last user message position and process assistant entries after it
$newEntries = @()
while ($null -ne ($line = $reader.ReadLine())) {
    if ($line.Trim().Length -eq 0) { continue }
    try {
        $entry = $line | ConvertFrom-Json -ErrorAction Stop
        $newEntries += $entry
    } catch { continue }
}

$newOffset = $stream.Position
$reader.Close()
$stream.Close()

# Find last user message in new entries to delimit "this turn"
$lastUserIdx = -1
for ($i = $newEntries.Count - 1; $i -ge 0; $i--) {
    if ($newEntries[$i].type -eq 'user') { $lastUserIdx = $i; break }
}

foreach ($entry in $newEntries) {
    if ($entry.type -ne 'assistant' -or -not $entry.message -or -not $entry.message.usage) { continue }

    $usage = $entry.message.usage
    $model = $entry.message.model
    $rates = Get-ModelRates $model

    $inTok = [long]($usage.input_tokens)
    $crTok = [long]($usage.cache_read_input_tokens)
    $cwTok = [long]($usage.cache_creation_input_tokens)
    $outTok = [long]($usage.output_tokens)

    $cost = (($inTok * $rates.input) + ($crTok * $rates.cache_read) + ($cwTok * $rates.cache_write) + ($outTok * $rates.output)) / 1000000

    $turnInfo = Get-TurnInfo $entry.message.content
    $entryType = $turnInfo.type

    # Weighted tokens for this entry (rate-limit relevant)
    # cache_read does NOT count toward ITPM on modern models (Opus 4.5+, Sonnet 4+, Haiku 4.5)
    # output weighted 5x because OTPM limits are ~5x tighter than ITPM
    $tw = @{ input=1.0; cache_read=0; cache_write=1.0; output=5.0 }
    $entryWeighted = ($inTok * $tw.input) + ($crTok * $tw.cache_read) + ($cwTok * $tw.cache_write) + ($outTok * $tw.output)

    # Cumulative
    $state.cumulative.input += $inTok
    $state.cumulative.cache_read += $crTok
    $state.cumulative.cache_write += $cwTok
    $state.cumulative.output += $outTok
    $state.cumulative.cost += $cost

    switch ($entryType) {
        "agent" { $state.cumulative.agent_cost += $cost; $state.cumulative.agent_calls++ }
        "tool"  { $state.cumulative.tool_cost += $cost; $state.cumulative.tool_calls++ }
        "chat"  { $state.cumulative.chat_cost += $cost; $state.cumulative.chat_calls++ }
    }

    # Per-tool tracking: attribute calls and weighted tokens
    $seenTools = @{}
    foreach ($toolName in $turnInfo.tools) {
        # Build key: for Task tools, append agent subtype
        $tKey = $toolName
        if ($toolName -eq 'Task' -and $turnInfo.agents.Count -gt 0) {
            $agentIdx = 0
            foreach ($t in $turnInfo.tools) {
                if ($t -eq 'Task') {
                    if ($agentIdx -lt $turnInfo.agents.Count) {
                        $tKey = "Task/$($turnInfo.agents[$agentIdx])"
                    }
                    $agentIdx++
                }
            }
        }

        if (-not $state.cumulative.tools.PSObject.Properties[$tKey]) {
            $state.cumulative.tools | Add-Member -NotePropertyName $tKey -NotePropertyValue ([PSCustomObject]@{
                calls = 0; weighted = 0.0
            }) -Force
        }
        $state.cumulative.tools.$tKey.calls++
        if (-not $seenTools.ContainsKey($tKey)) { $seenTools[$tKey] = $true }
    }
    # Attribute weighted tokens evenly across unique tools in this entry
    if ($seenTools.Count -gt 0) {
        $perToolW = $entryWeighted / $seenTools.Count
        foreach ($tKey in $seenTools.Keys) {
            $state.cumulative.tools.$tKey.weighted += $perToolW
        }
    }

    # This turn's totals
    $turnCost += $cost
    $turnInput += $inTok
    $turnCacheRead += $crTok
    $turnCacheWrite += $cwTok
    $turnOutput += $outTok
    $turnTools += $turnInfo.tools
    $turnModel = $model
    if ($entryType -eq "agent") { $turnType = "agent" }
    elseif ($entryType -eq "tool" -and $turnType -ne "agent") { $turnType = "tool" }
}

# Update state
$state.lastOffset = $newOffset
$state.lastTurn = [PSCustomObject]@{
    cost        = $turnCost
    type        = $turnType
    model       = $turnModel
    input       = $turnInput
    cache_read  = $turnCacheRead
    cache_write = $turnCacheWrite
    output      = $turnOutput
    tools       = $turnTools
}

# Store project slug on sidecar (extracted from transcript path)
if ($projectSlug) {
    $state | Add-Member -NotePropertyName 'project' -NotePropertyValue $projectSlug -Force
}

# ============================================
# A) Append to usage-history.jsonl
# ============================================
$historyPath = Join-Path $stateDir "usage-history.jsonl"
$totalApiCalls = $state.cumulative.agent_calls + $state.cumulative.tool_calls + $state.cumulative.chat_calls
$shortSid = $sessionId.Substring(0, [math]::Min(8, $sessionId.Length))

$historyEntry = @{
    ts    = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    sid   = $shortSid
    input = [long]$state.cumulative.input
    cr    = [long]$state.cumulative.cache_read
    cw    = [long]$state.cumulative.cache_write
    out   = [long]$state.cumulative.output
    cost  = [math]::Round($state.cumulative.cost, 2)
    turns = $totalApiCalls
} | ConvertTo-Json -Compress

try {
    $histStream = [System.IO.FileStream]::new(
        $historyPath,
        [System.IO.FileMode]::Append,
        [System.IO.FileAccess]::Write,
        [System.IO.FileShare]::Read
    )
    $histWriter = [System.IO.StreamWriter]::new($histStream)
    $histWriter.WriteLine($historyEntry)
    $histWriter.Close()
    $histStream.Close()
} catch {}

# ============================================
# B) Weekly compaction — prune usage-history entries >7 days
# ============================================
try {
    $now = [System.DateTimeOffset]::UtcNow
    $sevenDaysAgo = $now.AddDays(-7)

    # Track last compaction time in a lightweight file
    $compactMarker = Join-Path $stateDir "last-compaction.txt"
    $shouldCompact = $true
    if (Test-Path $compactMarker) {
        try {
            $lastCompact = [System.DateTimeOffset]::Parse((Get-Content $compactMarker -Raw).Trim())
            if (($now - $lastCompact).TotalHours -lt 24) { $shouldCompact = $false }
        } catch {}
    }

    if ($shouldCompact -and (Test-Path $historyPath)) {
        $histLines = Get-Content $historyPath -ErrorAction Stop
        $kept = @()
        foreach ($hl in $histLines) {
            if ($hl.Trim().Length -eq 0) { continue }
            try {
                $he = $hl | ConvertFrom-Json -ErrorAction Stop
                $heTime = [System.DateTimeOffset]::Parse($he.ts)
                if ($heTime -ge $sevenDaysAgo) { $kept += $hl }
            } catch { $kept += $hl }
        }
        if ($kept.Count -lt $histLines.Count) {
            $kept -join "`n" | Set-Content $historyPath -Encoding UTF8 -Force
        }
        $now.ToString('yyyy-MM-ddTHH:mm:ssZ') | Set-Content $compactMarker -Encoding UTF8 -Force
    }

    # Also compact limit-history.jsonl (prune entries >7 days)
    $limitHistPath = Join-Path $stateDir "limit-history.jsonl"
    if ($shouldCompact -and (Test-Path $limitHistPath)) {
        $lhLines = Get-Content $limitHistPath -ErrorAction Stop
        $lhKept = @()
        foreach ($ll in $lhLines) {
            if ($ll.Trim().Length -eq 0) { continue }
            try {
                $le = $ll | ConvertFrom-Json -ErrorAction Stop
                $leTime = [System.DateTimeOffset]::Parse($le.ts)
                if ($leTime -ge $sevenDaysAgo) { $lhKept += $ll }
            } catch { $lhKept += $ll }
        }
        if ($lhKept.Count -lt $lhLines.Count) {
            $lhKept -join "`n" | Set-Content $limitHistPath -Encoding UTF8 -Force
        }
    }
} catch {}

# Write state
try {
    $state | ConvertTo-Json -Depth 5 | Set-Content $statePath -Encoding UTF8 -Force
} catch {}

exit 0
