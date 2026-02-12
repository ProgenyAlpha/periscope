Start-Sleep -Seconds 2

Write-Host "=== Dashboard (runtime shell) ==="
$dash = Invoke-WebRequest -Uri 'http://localhost:8385/' -UseBasicParsing
Write-Host "Status: $($dash.StatusCode), Size: $($dash.Content.Length) bytes"
$hasPeri = $dash.Content.Contains('Periscope')
$hasRegister = $dash.Content.Contains('registerWidget')
Write-Host "Contains 'Periscope': $hasPeri"
Write-Host "Contains 'registerWidget': $hasRegister"

Write-Host ""
Write-Host "=== Widgets ==="
$widgets = Invoke-RestMethod -Uri 'http://localhost:8385/api/plugins/widgets'
Write-Host "Available: $($widgets -join ', ')"

foreach ($w in $widgets) {
    $wr = Invoke-WebRequest -Uri "http://localhost:8385/api/plugins/widgets/$w" -UseBasicParsing
    Write-Host "  $w - $($wr.Content.Length) bytes"
}

Write-Host ""
Write-Host "=== Data check ==="
$d = Invoke-RestMethod -Uri 'http://localhost:8385/api/data'
Write-Host "Sidecars: $($d.sidecars.Count), History: $($d.history.Count)"
Write-Host "Live usage 5hr: $($d.liveUsage.pct5hr)%, Weekly: $($d.liveUsage.pctWeekly)%"

Write-Host ""
Write-Host "=== Health ==="
$h = Invoke-RestMethod -Uri 'http://localhost:8385/api/health'
Write-Host "OK: $($h.ok), Clients: $($h.clients)"
