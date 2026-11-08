#!/usr/bin/env bash
set -euo pipefail

PI_HOST="${1:-xg1@10.252.178.225}"
KIOSK_BIN="bin/xeroxgo-kiosk-arm64"

echo "=== Deploying XeroxGo Kiosk to ${PI_HOST} ==="

if [ ! -f "${KIOSK_BIN}" ]; then
    echo "Building ARM64 binary..."
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o "${KIOSK_BIN}" ./kiosk-daemon/cmd/daemon
fi

echo "1. Copying binary to ${PI_HOST}..."
scp "${KIOSK_BIN}" "${PI_HOST}:/home/xg1/xeroxgo-kiosk.new"

echo "2. Activating new binary and restarting kiosk on ${PI_HOST}..."
ssh -t "${PI_HOST}" "bash -c '
    sudo systemctl stop xeroxgo-kiosk 2>/dev/null || true
    sudo killall -9 xeroxgo-kiosk 2>/dev/null || true
    mv /home/xg1/xeroxgo-kiosk.new /home/xg1/xeroxgo-kiosk
    chmod +x /home/xg1/xeroxgo-kiosk
    if systemctl is-enabled xeroxgo-kiosk >/dev/null 2>&1; then
        sudo systemctl restart xeroxgo-kiosk
        echo \"xeroxgo-kiosk service restarted via systemd.\"
    else
        nohup /home/xg1/xeroxgo-kiosk \
            --kiosk-id=kiosk-demo \
            --cloud-url=wss://xeroxgo-7s6i.onrender.com/ws \
            --cups-printer=DCPT310 \
            --framebuffer=/dev/fb0 \
            --idle-qr-content=https://xeroxgo-frontend.vercel.app > /tmp/xeroxgo-kiosk.log 2>&1 &
        echo \"xeroxgo-kiosk launched directly on /dev/fb0.\"
    fi
'"

echo "=== Deployment Complete! Check your kiosk display. ==="

