#!/bin/sh
# Periscope installer for Linux/macOS
# Usage: curl -fsSL https://raw.githubusercontent.com/ProgenyAlpha/periscope/main/install.sh | sh
set -e

REPO="ProgenyAlpha/periscope"
INSTALL_DIR="${HOME}/.local/bin"

# Detect OS and arch
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
    x86_64)  ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

ASSET="periscope-${OS}-${ARCH}"
if [ "$OS" != "darwin" ] && [ "$OS" != "linux" ]; then
    echo "Unsupported OS: $OS"; exit 1
fi

# Get latest tag
TAG=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | head -1 | cut -d'"' -f4)
echo "Installing periscope ${TAG} ..."

URL="https://github.com/${REPO}/releases/download/${TAG}/${ASSET}"
mkdir -p "$INSTALL_DIR"
echo "Downloading ${URL}"
curl -fsSL "$URL" -o "${INSTALL_DIR}/periscope"
chmod +x "${INSTALL_DIR}/periscope"

# Check PATH
case ":$PATH:" in
    *":${INSTALL_DIR}:"*) ;;
    *) echo "Add ${INSTALL_DIR} to your PATH: export PATH=\"\$PATH:${INSTALL_DIR}\"" ;;
esac

echo "Installed periscope ${TAG} to ${INSTALL_DIR}/periscope"
echo "Run 'periscope init' to set up, then 'periscope serve' to start."
