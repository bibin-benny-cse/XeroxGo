#!/usr/bin/env bash
# ==============================================================================
# XeroxGo Kiosk — Raspberry Pi Silent Boot & Custom Plymouth Installer
# ==============================================================================
# Configures a 100% silent, branded boot experience matching Android / commercial kiosks:
# 1. Disables Raspberry Pi GPU rainbow test screen (disable_splash=1).
# 2. Removes the 4 Raspberry Pi CPU logos (logo.nologo).
# 3. Disables blinking console cursor (vt.global_cursor_default=0).
# 4. Redirects early kernel console output to tty3 (console=tty3, loglevel=3, quiet).
# 5. Suppresses systemd status messages (systemd.show_status=0).
# 6. Installs and activates XeroxGo Plymouth boot theme.
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
THEME_SRC_DIR="${SCRIPT_DIR}/../kiosk-daemon/plymouth/xeroxgo"
TARGET_THEME_DIR="/usr/share/plymouth/themes/xeroxgo"

echo "========================================================"
echo "    XeroxGo Kiosk — Silent Boot & Plymouth Setup        "
echo "========================================================"

if [ "$(id -u)" -ne 0 ]; then
    echo "ERROR: This setup script must be executed as root." >&2
    echo "Please run: sudo bash $0" >&2
    exit 1
fi

# 1. Locate config.txt and cmdline.txt across Raspberry Pi OS versions
CONFIG_TXT=""
CMDLINE_TXT=""

if [ -f "/boot/firmware/config.txt" ]; then
    CONFIG_TXT="/boot/firmware/config.txt"
    CMDLINE_TXT="/boot/firmware/cmdline.txt"
elif [ -f "/boot/config.txt" ]; then
    CONFIG_TXT="/boot/config.txt"
    CMDLINE_TXT="/boot/cmdline.txt"
else
    echo "WARNING: Could not find standard Raspberry Pi boot config paths."
    echo "Searched: /boot/firmware/ and /boot/"
fi

# 2. Install Plymouth packages if missing
echo "[1/5] Ensuring Plymouth packages are installed..."
if ! command -v plymouth >/dev/null 2>&1; then
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends plymouth plymouth-themes
else
    echo "Plymouth is already installed."
fi

# 3. Deploy XeroxGo Theme Assets
echo "[2/5] Deploying XeroxGo Plymouth theme to ${TARGET_THEME_DIR}..."
mkdir -p "${TARGET_THEME_DIR}"

if [ ! -d "${THEME_SRC_DIR}" ]; then
    echo "ERROR: Theme source directory not found at ${THEME_SRC_DIR}" >&2
    exit 1
fi

cp -r "${THEME_SRC_DIR}/"* "${TARGET_THEME_DIR}/"
chmod 644 "${TARGET_THEME_DIR}"/*
chmod 755 "${TARGET_THEME_DIR}"

# 4. Configure Raspberry Pi Silent Firmware (config.txt)
if [ -n "${CONFIG_TXT}" ] && [ -f "${CONFIG_TXT}" ]; then
    echo "[3/5] Configuring firmware silent flags in ${CONFIG_TXT}..."
    cp -n "${CONFIG_TXT}" "${CONFIG_TXT}.bak.$(date +%Y%m%d%H%M%S)" || true

    # Disable rainbow splash screen
    if grep -q "^disable_splash=" "${CONFIG_TXT}"; then
        sed -i 's/^disable_splash=.*/disable_splash=1/' "${CONFIG_TXT}"
    else
        echo "disable_splash=1" >> "${CONFIG_TXT}"
    fi

    # Suppress low-voltage / warning icon overlays from ruining boot logo
    if grep -q "^avoid_warnings=" "${CONFIG_TXT}"; then
        sed -i 's/^avoid_warnings=.*/avoid_warnings=1/' "${CONFIG_TXT}"
    else
        echo "avoid_warnings=1" >> "${CONFIG_TXT}"
    fi
fi

# 5. Configure Kernel Commandline for Silent Boot (cmdline.txt)
# IMPORTANT: cmdline.txt MUST remain on a single line!
if [ -n "${CMDLINE_TXT}" ] && [ -f "${CMDLINE_TXT}" ]; then
    echo "[4/5] Configuring silent boot parameters in ${CMDLINE_TXT}..."
    cp -n "${CMDLINE_TXT}" "${CMDLINE_TXT}.bak.$(date +%Y%m%d%H%M%S)" || true

    CURRENT_CMDLINE="$(tr '\n' ' ' < "${CMDLINE_TXT}" | sed -e 's/[[:space:]]*$//')"

    # Replace console=tty1 with console=tty3 to divert kernel logs away from main display
    CURRENT_CMDLINE="$(echo "${CURRENT_CMDLINE}" | sed 's/console=tty1\b/console=tty3/g')"

    # Parameters to enforce for Android-style clean boot
    REQUIRED_PARAMS=(
        "quiet"
        "splash"
        "plymouth.ignore-serial-consoles"
        "logo.nologo"
        "vt.global_cursor_default=0"
        "consoleblank=0"
        "loglevel=3"
        "systemd.show_status=0"
        "rd.systemd.show_status=false"
    )

    for param in "${REQUIRED_PARAMS[@]}"; do
        if ! echo "${CURRENT_CMDLINE}" | grep -q -w "${param}"; then
            CURRENT_CMDLINE="${CURRENT_CMDLINE} ${param}"
        fi
    done

    # Format cleanly on a single line
    echo "${CURRENT_CMDLINE}" | tr -s ' ' | tr '\n' ' ' | sed -e 's/[[:space:]]*$//' > "${CMDLINE_TXT}"
    echo "" >> "${CMDLINE_TXT}"
fi

# 6. Set Default Plymouth Theme and Rebuild Initramfs
echo "[5/5] Activating XeroxGo theme and updating initramfs..."
if command -v plymouth-set-default-theme >/dev/null 2>&1; then
    plymouth-set-default-theme -R xeroxgo
else
    update-alternatives --install /usr/share/plymouth/themes/default.plymouth default.plymouth "${TARGET_THEME_DIR}/xeroxgo.plymouth" 100
    update-alternatives --set default.plymouth "${TARGET_THEME_DIR}/xeroxgo.plymouth"
    if command -v update-initramfs >/dev/null 2>&1; then
        update-initramfs -u
    fi
fi

echo ""
echo "========================================================"
echo "✅ XeroxGo Silent Boot & Plymouth Theme Setup Complete!"
echo "========================================================"
echo "To test the splash screen without rebooting, run:"
echo "  sudo plymouthd && sudo plymouth --show-splash && sleep 5 && sudo plymouth --quit"
echo ""
echo "On next reboot, the Raspberry Pi will boot silently directly"
echo "into the XeroxGo logo with no Raspberry Pi branding."
echo "========================================================"

