# Periscope installer for Windows
#
# Usage:
#   irm https://raw.githubusercontent.com/ProgenyAlpha/periscope/master/install.ps1 | iex
#   & ([scriptblock]::Create((irm .../install.ps1))) -Version v1.4.0
#
# Like install.sh, this verifies the download against the release's SHA256SUMS
# asset before installing, stages into a temp file and moves it into place
# rather than writing over a binary that may be running, and refuses to install
# anything it could not verify unless -SkipChecksum is given.
param(
    [string]$Version = $env:PERISCOPE_VERSION,
    [switch]$SkipChecksum
)

$ErrorActionPreference = 'Stop'

$repo    = "ProgenyAlpha/periscope"
$bin     = "periscope.exe"
$asset   = "periscope-windows-amd64.exe"
$baseUrl = if ($env:PERISCOPE_BASE_URL) { $env:PERISCOPE_BASE_URL } else { "https://github.com/$repo/releases/download" }
$apiUrl  = if ($env:PERISCOPE_API_URL)  { $env:PERISCOPE_API_URL }  else { "https://api.github.com/repos/$repo/releases/latest" }

function Die($msg) {
    Write-Error $msg
    exit 1
}

# ── Release tag ─────────────────────────────────────────────────────────────
#
# An empty tag used to build a URL with an empty path segment, which 404s with
# no hint that the real problem was a rate-limited or unreachable GitHub API.
if (-not $Version) {
    try {
        $Version = (Invoke-RestMethod $apiUrl).tag_name
    } catch {
        $Version = $null
    }
    if (-not $Version) {
        Die "Could not determine the latest release from $apiUrl. The GitHub API may be rate-limited or unreachable. Re-run with -Version v1.0.0 (releases are listed at https://github.com/$repo/releases)."
    }
}
if ($Version -notmatch '^v?[0-9][0-9A-Za-z\.\-\+]*$') {
    Die "`"$Version`" does not look like a release tag (expected something like v1.4.0)."
}

$dest = if ($env:PERISCOPE_INSTALL_DIR) { $env:PERISCOPE_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "periscope" }
New-Item -ItemType Directory -Force -Path $dest | Out-Null
$out = Join-Path $dest $bin

Write-Host "Installing periscope $Version into $dest"

# ── Download ────────────────────────────────────────────────────────────────
#
# Staged in the destination directory so the final move is a same-volume
# rename, and so a running periscope.exe is replaced rather than written into.
$tmpBin  = Join-Path $dest (".periscope-" + [System.IO.Path]::GetRandomFileName() + ".tmp")
$tmpSums = Join-Path ([System.IO.Path]::GetTempPath()) ("periscope-sums-" + [System.IO.Path]::GetRandomFileName())

try {
    $url = "$baseUrl/$Version/$asset"
    Write-Host "Downloading $url"
    try {
        Invoke-WebRequest -Uri $url -OutFile $tmpBin -UseBasicParsing
    } catch {
        Die "download failed: $url ($($_.Exception.Message))"
    }
    if (-not (Test-Path $tmpBin) -or (Get-Item $tmpBin).Length -eq 0) {
        Die "downloaded an empty file from $url"
    }

    # ── Verify ──────────────────────────────────────────────────────────────
    if ($SkipChecksum) {
        Write-Host "WARNING: -SkipChecksum given; installing an unverified binary."
    } else {
        $sumsUrl = "$baseUrl/$Version/SHA256SUMS"
        try {
            Invoke-WebRequest -Uri $sumsUrl -OutFile $tmpSums -UseBasicParsing
        } catch {
            Die "no checksum file at $sumsUrl - refusing to install an unverified binary. Re-run with -SkipChecksum if you accept that risk."
        }

        $expected = $null
        foreach ($line in (Get-Content $tmpSums)) {
            $parts = $line -split '\s+', 2
            if ($parts.Count -eq 2 -and $parts[1].Trim().TrimStart('*') -eq $asset) {
                $expected = $parts[0].Trim().ToLower()
                break
            }
        }
        if (-not $expected) {
            Die "SHA256SUMS has no entry for $asset - refusing to install an unverified binary."
        }

        $actual = (Get-FileHash -Algorithm SHA256 -Path $tmpBin).Hash.ToLower()
        if ($actual -ne $expected) {
            Die "CHECKSUM MISMATCH for $asset`n      expected: $expected`n      actual:   $actual`nThe download was corrupted or tampered with. Nothing was installed."
        }
        Write-Host "Checksum OK ($expected)"
    }

    # ── Install ─────────────────────────────────────────────────────────────
    try {
        Move-Item -Force -Path $tmpBin -Destination $out
    } catch {
        Die "could not move the new binary into $out - is periscope.exe running? Stop it and re-run. ($($_.Exception.Message))"
    }
} finally {
    Remove-Item -Force -ErrorAction SilentlyContinue $tmpBin, $tmpSums
}

Write-Host "Installed periscope $Version to $out"

# ── Aftercare ───────────────────────────────────────────────────────────────

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$dest*") {
    [Environment]::SetEnvironmentVariable("Path", "$userPath;$dest", "User")
    Write-Host "Added $dest to user PATH (restart terminal to take effect)"
}

# A running server goes on executing the old image until it is restarted. Say
# so; do not kill someone's daemon from an install script.
if (Get-Process -Name periscope -ErrorAction SilentlyContinue) {
    Write-Host ""
    Write-Host "A periscope process is already running the previous binary."
    Write-Host "Restart it to pick up ${Version}:"
    Write-Host "    Stop-Process -Name periscope; periscope serve"
}

Write-Host ""
Write-Host "Run 'periscope init' to set up, then 'periscope serve' to start."
Write-Host "To remove it later: periscope uninstall"
