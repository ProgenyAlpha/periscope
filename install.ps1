# Periscope installer for Windows
# Usage: irm https://raw.githubusercontent.com/ProgenyAlpha/periscope/main/install.ps1 | iex
$ErrorActionPreference = 'Stop'

$repo = "ProgenyAlpha/periscope"
$bin  = "periscope.exe"

# Get latest release tag
$release = Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest"
$tag = $release.tag_name
Write-Host "Installing periscope $tag ..."

$asset = "periscope-windows-amd64.exe"
$url = "https://github.com/$repo/releases/download/$tag/$asset"

$dest = Join-Path $env:LOCALAPPDATA "periscope"
New-Item -ItemType Directory -Force -Path $dest | Out-Null
$out = Join-Path $dest $bin

Write-Host "Downloading $url"
Invoke-WebRequest -Uri $url -OutFile $out -UseBasicParsing

# Add to PATH if not already there
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$dest*") {
    [Environment]::SetEnvironmentVariable("Path", "$userPath;$dest", "User")
    Write-Host "Added $dest to user PATH (restart terminal to take effect)"
}

Write-Host "Installed periscope $tag to $out"
Write-Host "Run 'periscope init' to set up, then 'periscope serve' to start."
