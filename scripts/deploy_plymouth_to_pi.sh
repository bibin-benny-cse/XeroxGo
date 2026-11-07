#!/usr/bin/env bash
# ==============================================================================
# XeroxGo Kiosk — Remote Plymouth Theme Deployment Script
# ==============================================================================
# Copies the Plymouth theme assets and installer to the Raspberry Pi over SSH
# and executes the setup with root privileges.
#
# Usage:
#   ./scripts/deploy_plymouth_to_pi.sh [user@host]
#   ./scripts/deploy_plymouth_to_pi.sh xg1@10.252.178.225
# ==============================================================================
set -euo pipefail

PI_HOST="${1:-xg1@10.252.178.225}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${SCRIPT_DIR}/.."
THEME_DIR="${REPO_ROOT}/kiosk-daemon/plymouth"
INSTALLER="${SCRIPT_DIR}/setup_plymouth.sh"

echo "=== Deploying XeroxGo Plymouth Boot Splash to ${PI_HOST} ==="

if [ ! -d "${THEME_DIR}/xeroxgo" ]; then
    echo "ERROR: Theme directory not found at ${THEME_DIR}/xeroxgo" >&2
    exit 1
fi

echo "1. Uploading theme package to ${PI_HOST}..."
ssh "${PI_HOST}" "mkdir -p /tmp/xeroxgo-plymouth/kiosk-daemon/plymouth /tmp/xeroxgo-plymouth/scripts"
scp -r "${THEME_DIR}/xeroxgo" "${PI_HOST}:/tmp/xeroxgo-plymouth/kiosk-daemon/plymouth/"
scp "${INSTALLER}" "${PI_HOST}:/tmp/xeroxgo-plymouth/scripts/setup_plymouth.sh"

echo "2. Running installer on ${PI_HOST}..."
ssh -t "${PI_HOST}" "sudo bash /tmp/xeroxgo-plymouth/scripts/setup_plymouth.sh"

echo ""
echo "=== Plymouth Deployment Finished ==="

