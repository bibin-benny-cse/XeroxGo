# XeroxGo Kiosk Provisioning & Credential Security Guide

This document describes the operator procedure for provisioning, rotating, and revoking machine credentials for registered XeroxGo physical kiosks.

---

## 1. Security Architecture

- **Independent Per-Machine Credentials**: Every XeroxGo physical kiosk is assigned a unique `kiosk_id` and a unique high-entropy secret. XeroxGo strictly forbids shared or hardcoded kiosk credentials.
- **Stored Secret Hashes Only**: The backend database stores only the SHA-256 hexadecimal hash (`kiosks.secret_hash`) of each kiosk secret. Plaintext machine secrets are never stored in the database, never logged, and never included in telemetry or API responses.
- **Constant-Time Verification**: When a kiosk submits API requests with `X-Kiosk-ID` and `X-Kiosk-Secret` (or `Authorization: Bearer <secret>`), the backend validates the incoming secret against the persisted hash using `crypto/subtle.ConstantTimeCompare` to eliminate timing side-channel attacks.
- **Header-Bound Authentication**: The backend never trusts a `kiosk_id` passed only in request bodies or query parameters. The authenticated kiosk ID is bound exclusively from the verified header credentials.
- **Instant Revocation**: Setting `is_active = 0` in the database immediately causes all requests from that kiosk to be rejected with `403 Forbidden`.

---

## 2. Provisioning a New Kiosk

Use the dedicated provisioning CLI tool:

```bash
# Provision a new kiosk
go run ./cloud-backend/cmd/provision \
  -kiosk-id "kiosk-library-01" \
  -name "Central Campus Library Ground Floor"
```

### CLI Output Example:
```text
================================================================================
                     XEROXGO KIOSK PROVISIONING SUCCESS                        
================================================================================
Kiosk ID:       kiosk-library-01
Name:           Central Campus Library Ground Floor
Status:         ACTIVE
Secret Hash:    e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
--------------------------------------------------------------------------------
PROVISIONING CREDENTIAL (COPY NOW — THIS VALUE IS NEVER STORED OR SHOWN AGAIN):
XEROXGO_KIOSK_SECRET=a8f9c2d1e0b347689123456789abcdef0123456789abcdef0123456789abcdef
--------------------------------------------------------------------------------
OPERATOR INSTRUCTIONS:
1. SSH into the physical kiosk device.
2. Write the credential to /etc/xeroxgo/kiosk.env with restricted permissions:
   sudo tee /etc/xeroxgo/kiosk.env > /dev/null <<EOF
   XEROXGO_KIOSK_ID=kiosk-library-01
   XEROXGO_KIOSK_SECRET=a8f9c2d1e0b347689123456789abcdef0123456789abcdef0123456789abcdef
   EOF
   sudo chmod 0600 /etc/xeroxgo/kiosk.env
   sudo chown xeroxgo:xeroxgo /etc/xeroxgo/kiosk.env
3. Restart the kiosk daemon: sudo systemctl restart xeroxgo-daemon
================================================================================
```

---

## 3. On-Device Configuration (Raspberry Pi)

1. **Restricted File Permissions**: The file `/etc/xeroxgo/kiosk.env` must be owned by the service user and have mode `0600` (`-rw-------`), readable only by the XeroxGo daemon process.
2. **Systemd Service Integration**: The kiosk daemon service file (`/etc/systemd/system/xeroxgo-daemon.service`) loads these variables via:
   ```ini
   [Service]
   EnvironmentFile=/etc/xeroxgo/kiosk.env
   ExecStart=/usr/local/bin/xeroxgo-daemon
   User=xeroxgo
   Group=xeroxgo
   Restart=always
   ```

---

## 4. Credential Revocation

If a kiosk machine is decommissioned, compromised, or stolen:

```bash
# Immediately revoke machine access
go run ./cloud-backend/cmd/provision \
  -kiosk-id "kiosk-library-01" \
  -revoke
```

All subsequent PIN redemption requests, payment attempts, and WebSocket handshakes from that machine will be immediately blocked with `403 Forbidden`.

---

## 5. Credential Rotation

To rotate a kiosk's secret:
1. Run `go run ./cloud-backend/cmd/provision -kiosk-id "kiosk-library-01"` to issue a new secret.
2. The CLI atomically updates `kiosks.secret_hash` with the new hash.
3. Update `/etc/xeroxgo/kiosk.env` on the device and restart the daemon.

