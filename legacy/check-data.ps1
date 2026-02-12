$r = Invoke-WebRequest -Uri 'http://localhost:8385/api/data' -UseBasicParsing
$j = $r.Content | ConvertFrom-Json
Write-Host "Sidecars: $($j.sidecars.Count)"
Write-Host "History: $($j.history.Count)"
Write-Host "LimitHistory: $($j.limitHistory.Count)"
Write-Host "HasProfile: $($null -ne $j.profile)"
Write-Host "HasUsage: $($null -ne $j.liveUsage)"
if ($j.sidecars.Count -gt 0) {
  $first = $j.sidecars[0]
  Write-Host "First sidecar ID: $($first.id.Substring(0,8))"
  $c = $first.data.cumulative
  if ($c) {
    Write-Host "  Cost: $($c.cost), Input: $($c.input), CacheRead: $($c.cache_read), Output: $($c.output)"
    Write-Host "  AgentCalls: $($c.agent_calls), ToolCalls: $($c.tool_calls), ChatCalls: $($c.chat_calls)"
    $toolCount = 0
    if ($c.tools) { $toolCount = ($c.tools | Get-Member -MemberType NoteProperty).Count }
    Write-Host "  Unique tools: $toolCount"
  }
  $lt = $first.data.lastTurn
  if ($lt) {
    Write-Host "  LastTurn type=$($lt.type) model=$($lt.model) cost=$($lt.cost)"
  }
}
