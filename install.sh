#!/bin/sh
# Periscope installer for Linux/macOS
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/ProgenyAlpha/periscope/master/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- v1.4.0        # pin a version
#   PERISCOPE_VERSION=v1.4.0 sh install.sh            # same, via the environment
#
# Options:
#   --version <tag>    install a specific release instead of the latest
#   --skip-checksum    install even though the release publishes no SHA256SUMS
#   --help
#
# Environment overrides (also used by the test fixture):
#   PERISCOPE_INSTALL_DIR   where to install         (default ~/.local/bin)
#   PERISCOPE_BASE_URL      release asset base URL   (default GitHub releases)
#   PERISCOPE_API_URL       "latest release" API URL (default GitHub API)
#
# This script downloads a binary and puts it on your PATH, so it verifies what
# it downloaded (SHA256 against the SHA256SUMS asset published with the
# release) before installing, and installs by atomic rename rather than by
# writing over the file a running daemon is executing.
set -eu

REPO="ProgenyAlpha/periscope"
INSTALL_DIR="${PERISCOPE_INSTALL_DIR:-${HOME}/.local/bin}"
BASE_URL="${PERISCOPE_BASE_URL:-https://github.com/${REPO}/releases/download}"
API_URL="${PERISCOPE_API_URL:-https://api.github.com/repos/${REPO}/releases/latest}"
TAG="${PERISCOPE_VERSION:-}"
SKIP_CHECKSUM=0

die() {
    echo "install.sh: $*" >&2
    exit 1
}

usage() {
    cat <<'EOF'
Periscope installer for Linux/macOS

Usage:
  curl -fsSL https://raw.githubusercontent.com/ProgenyAlpha/periscope/master/install.sh | sh
  curl -fsSL .../install.sh | sh -s -- v1.4.0    # pin a version

Options:
  --version <tag>    install a specific release instead of the latest
  --skip-checksum    install even though the release publishes no SHA256SUMS
  -h, --help         show this help

Environment:
  PERISCOPE_VERSION       release tag to install
  PERISCOPE_INSTALL_DIR   where to install (default ~/.local/bin)
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --version)
            [ $# -ge 2 ] || die "--version needs a tag (e.g. --version v1.4.0)"
            TAG="$2"
            shift 2
            ;;
        --version=*) TAG="${1#--version=}"; shift ;;
        --skip-checksum) SKIP_CHECKSUM=1; shift ;;
        -h|--help) usage; exit 0 ;;
        -*) die "unknown option: $1" ;;
        *) TAG="$1"; shift ;;
    esac
done

command -v curl >/dev/null 2>&1 || die "curl is required but was not found on PATH."

# ── Platform ────────────────────────────────────────────────────────────────

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) die "Unsupported architecture: $ARCH" ;;
esac
case "$OS" in
    linux|darwin) ;;
    *) die "Unsupported OS: $OS" ;;
esac
ASSET="periscope-${OS}-${ARCH}"

# ── Release tag ─────────────────────────────────────────────────────────────
#
# An empty tag used to sail straight through to a download of
# .../releases/download//periscope-linux-amd64, which 404s with no hint that
# the real problem was a rate-limited or unreachable GitHub API.
if [ -z "$TAG" ]; then
    TAG=$(curl -fsSL "$API_URL" 2>/dev/null | grep '"tag_name"' | head -1 | cut -d'"' -f4 || true)
    [ -n "$TAG" ] || die "Could not determine the latest release from ${API_URL}.
  The GitHub API may be rate-limited or unreachable. Pass a version explicitly:
      curl -fsSL https://raw.githubusercontent.com/${REPO}/master/install.sh | sh -s -- v1.0.0
  Releases are listed at https://github.com/${REPO}/releases"
fi
case "$TAG" in
    v[0-9]*|[0-9]*) ;;
    *) die "\"$TAG\" does not look like a release tag (expected something like v1.4.0)." ;;
esac
case "$TAG" in
    */*|*' '*) die "\"$TAG\" is not a valid release tag." ;;
esac

echo "Installing periscope ${TAG} (${OS}/${ARCH}) into ${INSTALL_DIR}"

# ── Download ────────────────────────────────────────────────────────────────

mkdir -p "$INSTALL_DIR"
[ -w "$INSTALL_DIR" ] || die "${INSTALL_DIR} is not writable."

# Staged inside the destination directory so the final move is a same-
# filesystem rename: atomic, and it replaces the directory entry instead of
# writing through into the file a running `periscope serve` is executing.
TMPBIN=$(mktemp "${INSTALL_DIR}/.periscope.XXXXXX") || die "could not create a temp file in ${INSTALL_DIR}"
TMPSUMS=$(mktemp "${TMPDIR:-/tmp}/periscope-sums.XXXXXX") || die "could not create a temp file"
cleanup() { rm -f "$TMPBIN" "$TMPSUMS"; }
trap cleanup EXIT INT TERM HUP

URL="${BASE_URL}/${TAG}/${ASSET}"
echo "Downloading ${URL}"
curl -fsSL "$URL" -o "$TMPBIN" || die "download failed: ${URL}"
[ -s "$TMPBIN" ] || die "downloaded an empty file from ${URL}"

# ── Verify ──────────────────────────────────────────────────────────────────

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    elif command -v openssl >/dev/null 2>&1; then
        openssl dgst -sha256 "$1" | awk '{print $NF}'
    else
        return 1
    fi
}

if [ "$SKIP_CHECKSUM" -eq 1 ]; then
    echo "WARNING: --skip-checksum given; installing an unverified binary."
else
    SUMS_URL="${BASE_URL}/${TAG}/SHA256SUMS"
    curl -fsSL "$SUMS_URL" -o "$TMPSUMS" 2>/dev/null || die "no checksum file at ${SUMS_URL} — refusing to install an unverified binary.
  Re-run with --skip-checksum if you accept that risk."
    EXPECTED=$(awk -v n="$ASSET" '$2 == n || $2 == "*" n {print $1; exit}' "$TMPSUMS")
    [ -n "$EXPECTED" ] || die "SHA256SUMS has no entry for ${ASSET} — refusing to install an unverified binary."

    ACTUAL=$(sha256_of "$TMPBIN") || die "no SHA-256 tool found (need sha256sum, shasum or openssl) — refusing to install an unverified binary.
  Re-run with --skip-checksum if you accept that risk."
    if [ "$ACTUAL" != "$EXPECTED" ]; then
        die "CHECKSUM MISMATCH for ${ASSET}
      expected: ${EXPECTED}
      actual:   ${ACTUAL}
  The download was corrupted or tampered with. Nothing was installed."
    fi
    echo "Checksum OK (${EXPECTED})"
fi

# ── Install ─────────────────────────────────────────────────────────────────

DEST="${INSTALL_DIR}/periscope"
chmod 755 "$TMPBIN"
mv -f "$TMPBIN" "$DEST" || die "could not move the new binary into ${DEST}"
trap - EXIT INT TERM HUP
rm -f "$TMPSUMS"

echo "Installed periscope ${TAG} to ${DEST}"

# ── Aftercare ───────────────────────────────────────────────────────────────

case ":$PATH:" in
    *":${INSTALL_DIR}:"*) ;;
    *) echo "Add ${INSTALL_DIR} to your PATH: export PATH=\"\$PATH:${INSTALL_DIR}\"" ;;
esac

# A running server keeps executing the old inode after the rename, so it will
# happily go on running the previous version until it is restarted. Say so
# rather than killing someone's daemon from an install script.
serve_running() {
    if command -v pgrep >/dev/null 2>&1; then
        pgrep -f 'periscope serve' >/dev/null 2>&1
    else
        ps ax 2>/dev/null | grep -v grep | grep -q '[p]eriscope serve'
    fi
}
if serve_running; then
    echo
    echo "A 'periscope serve' process is already running the previous binary."
    echo "Restart it to pick up ${TAG}:"
    echo "    pkill -f 'periscope serve' && periscope serve"
fi

echo
echo "Run 'periscope init' to set up, then 'periscope serve' to start."
echo "To remove it later: periscope uninstall"
