# Test display hook with a real session ID
$stateDir = "$env:USERPROFILE\.claude\hooks\cost-state"
$sidecars = Get-ChildItem $stateDir -Filter "*.json" | Where-Object {
    $_.Name -notmatch 'usage-config|usage-api-cache|profile-cache|litellm-pricing'
} | Sort-Object LastWriteTime -Descending | Select-Object -First 1

if (-not $sidecars) {
    Write-Host "No sidecar files found"
    exit 1
}

$sessionId = $sidecars.BaseName
Write-Host "Testing with session: $sessionId"

# Test display hook
$payload = @{ session_id = $sessionId } | ConvertTo-Json
Write-Host "`n=== Display Hook Output ==="
$result = $payload | & "C:\Users\shawn\OneDrive\Desktop\Apps\periscope\periscope.exe" hook display
$parsed = $result | ConvertFrom-Json
Write-Host $parsed.hookSpecificOutput.additionalContext
