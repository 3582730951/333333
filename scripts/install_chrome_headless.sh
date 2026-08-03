#!/bin/bash
# install_chrome_headless.sh — Install Chrome for headless browser automation on cloud VPS
# Tested on: Ubuntu 22.04/24.04, Debian 11/12, RHEL 9
# Run as root or with sudo

set -e

echo "=== Installing Chrome for headless automation ==="

# Detect OS
if [ -f /etc/os-release ]; then
    . /etc/os-release
    OS=$ID
    VER=$VERSION_ID
else
    echo "Cannot detect OS. Exiting."
    exit 1
fi

echo "Detected OS: $OS $VER"

case "$OS" in
    ubuntu|debian)
        echo "Installing Chrome on Debian/Ubuntu..."
        apt-get update -qq
        apt-get install -y wget gnupg2 ca-certificates apt-transport-https
        wget -q -O - https://dl.google.com/linux/linux_signing_key.pub | apt-key add -
        echo "deb [arch=amd64] http://dl.google.com/linux/chrome/deb/ stable main" > /etc/apt/sources.list.d/google-chrome.list
        apt-get update -qq
        apt-get install -y google-chrome-stable fonts-liberation fonts-noto-cjk
        ;;
    rhel|centos|rocky|almalinux)
        echo "Installing Chrome on RHEL/CentOS..."
        cat > /etc/yum.repos.d/google-chrome.repo << 'EOF'
[google-chrome]
name=google-chrome
baseurl=http://dl.google.com/linux/chrome/rpm/stable/x86_64
enabled=1
gpgcheck=1
gpgkey=https://dl.google.com/linux/linux_signing_key.pub
EOF
        yum install -y google-chrome-stable liberation-fonts google-noto-cjk-fonts
        ;;
    *)
        echo "Unsupported OS: $OS. Manually install Chrome."
        exit 1
        ;;
esac

CHROME_BIN=$(which google-chrome || which google-chrome-stable || echo "")
if [ -z "$CHROME_BIN" ]; then
    echo "ERROR: Chrome installation failed"
    exit 1
fi

echo "Chrome installed: $CHROME_BIN"
$CHROME_BIN --version
echo "✅ Chrome ready for headless automation"
