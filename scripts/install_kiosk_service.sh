#!/usr/bin/env bash
# ==============================================================================
# XeroxGo Kiosk — Systemd Service Installer for Raspberry Pi
# ==============================================================================
# Installs and enables the xeroxgo-kiosk.service systemd unit for automatic
# startup upon boot directly on the framebuffer (/dev/fb0).
#
# Usage:
#   sudo bash scripts/install_kiosk_service.sh [user]
#   sudo bash scripts/install_kiosk_service.sh xg1
# ==============================================================================
set -euo pipefail

KIOSK_USER="${1:-xg1}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${SCRIPT_DIR}/.."
SERVICE_SRC="${REPO_ROOT}/deploy/rpi-os/xeroxgo-kiosk.service"
DEFAULT_SRC="${REPO_ROOT}/deploy/rpi-os/xeroxgo-kiosk.default"

echo "========================================================"
echo "    XeroxGo Kiosk — Systemd Service Installer           "
echo "========================================================"

if [ "$(id -u)" -ne 0 ]; then
    echo "ERROR: This script must be run as root." >&2
    echo "Please run: sudo bash $0" >&2
    exit 1
fi

if ! id "${KIOSK_USER}" >/dev/null 2>&1; then
    echo "ERROR: User '${KIOSK_USER}' does not exist on this system." >&2
    exit 1
fi

# 1. Create runtime data and jobs directories
echo "[1/6] Creating /var/kiosk data directories..."
mkdir -p /var/kiosk/jobs
chown -R "${KIOSK_USER}:${KIOSK_USER}" /var/kiosk
chmod -R 755 /var/kiosk

# 2. Add kiosk user to hardware groups (framebuffer, printer, input, tty)
echo "[2/6] Adding ${KIOSK_USER} to hardware groups (video, lp, input, tty)..."
for grp in video lp input tty; do
    if getent group "${grp}" >/dev/null 2>&1; then
        usermod -aG "${grp}" "${KIOSK_USER}"
    fi
done

# 3. Disable console login prompt on tty1 so it doesn't fight for /dev/fb0
echo "[3/6] Disabling conflicting getty@tty1 console prompt..."
systemctl stop getty@tty1.service 2>/dev/null || true
systemctl mask getty@tty1.service 2>/dev/null || true
systemctl unmask plymouth-quit.service 2>/dev/null || true
systemctl unmask plymouth-quit-wait.service 2>/dev/null || true

# 4. Install configuration file in /etc/default/
echo "[4/6] Installing configuration into /etc/default/xeroxgo-kiosk..."
if [ ! -f "/etc/default/xeroxgo-kiosk" ]; then
    cp "${DEFAULT_SRC}" /etc/default/xeroxgo-kiosk
    chmod 644 /etc/default/xeroxgo-kiosk
    echo "Created /etc/default/xeroxgo-kiosk."
else
    echo "/etc/default/xeroxgo-kiosk already exists, preserving your settings."
fi

# 5. Install systemd service unit
echo "[5/6] Installing /etc/systemd/system/xeroxgo-kiosk.service..."
sed -e "s/User=xg1/User=${KIOSK_USER}/g" \
    -e "s/Group=xg1/Group=${KIOSK_USER}/g" \
    -e "s|/home/xg1/xeroxgo-kiosk|/home/${KIOSK_USER}/xeroxgo-kiosk|g" \
    "${SERVICE_SRC}" > /etc/systemd/system/xeroxgo-kiosk.service

chmod 644 /etc/systemd/system/xeroxgo-kiosk.service

# 6. Reload systemd and enable auto-start
echo "[6/6] Enabling and starting xeroxgo-kiosk service..."
systemctl daemon-reload
systemctl enable xeroxgo-kiosk.service

# Check if binary exists before starting
if [ -f "/home/${KIOSK_USER}/xeroxgo-kiosk" ]; then
    systemctl restart xeroxgo-kiosk.service
    echo "Service started successfully!"
else
    echo "NOTE: Binary /home/${KIOSK_USER}/xeroxgo-kiosk not found yet."
    echo "Deploy the binary using: ./scripts/deploy_pi.sh"
    echo "The service will automatically run on boot once deployed."
fi

echo ""
echo "========================================================"
echo "✅ XeroxGo Kiosk Service Configured!"
echo "========================================================"
echo "Useful commands:"
echo "  Check status:   sudo systemctl status xeroxgo-kiosk"
echo "  View live logs: sudo journalctl -u xeroxgo-kiosk -f"
echo "  Restart kiosk:  sudo systemctl restart xeroxgo-kiosk"
echo "  Stop kiosk:     sudo systemctl stop xeroxgo-kiosk"
echo "========================================================"

