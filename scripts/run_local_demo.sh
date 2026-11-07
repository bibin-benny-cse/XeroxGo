#!/usr/bin/env bash
set -euo pipefail

# 1. Resolve repository root
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# 2. Check prerequisites
for cmd in go gs qpdf python3; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "Error: required prerequisite '$cmd' not found in PATH" >&2
    exit 1
  fi
done

# 3. Create unique temporary workspace
TEMP_DIR="$(mktemp -d -t xeroxgo_demo_XXXXXX)"
CLOUD_DB="${TEMP_DIR}/cloud.db"
CLOUD_JOBS="${TEMP_DIR}/cloud_jobs"
KIOSK_DB="${TEMP_DIR}/kiosk.db"
KIOSK_JOBS="${TEMP_DIR}/kiosk_jobs"
PREVIEW_PNG="${TEMP_DIR}/xeroxgo_screen.png"
CLOUD_LOG="${TEMP_DIR}/cloud.log"
KIOSK_LOG="${TEMP_DIR}/kiosk.log"

mkdir -p "${CLOUD_JOBS}" "${KIOSK_JOBS}"

# 4. Generate random development secrets & configuration
DOWNLOAD_SECRET="$(python3 -c 'import secrets; print(secrets.token_hex(32))')"
WEBHOOK_SECRET="$(python3 -c 'import secrets; print(secrets.token_hex(32))')"
PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"

export XEROXGO_CLOUD_ADDR="127.0.0.1:${PORT}"
export XEROXGO_CLOUD_DB="${CLOUD_DB}"
export XEROXGO_JOBS_DIR="${CLOUD_JOBS}"
export XEROXGO_DEV_MODE=1
export XEROXGO_PRICE_PER_SHEET_PAISE="${XEROXGO_PRICE_PER_SHEET_PAISE:-500}"
export XEROXGO_DOWNLOAD_SECRET="${DOWNLOAD_SECRET}"
export XEROXGO_WEBHOOK_SECRET="${WEBHOOK_SECRET}"

KIOSK_ID="kiosk-demo"
CLOUD_URL="ws://127.0.0.1:${PORT}/ws"
PORTAL_URL="http://127.0.0.1:${PORT}/k/${KIOSK_ID}"

# Track background PIDs for cleanup
CLOUD_PID=""
KIOSK_PID=""
SMOKE_TEST_MODE=0

terminate_process() {
  local pid="$1"
  local name="$2"
  if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
    kill -TERM "${pid}" 2>/dev/null || true
    local exited=0
    for _ in $(seq 1 50); do
      if ! kill -0 "${pid}" 2>/dev/null; then
        exited=1
        break
      fi
      sleep 0.1
    done
    if [ "${exited}" -eq 0 ]; then
      echo "Warning: ${name} (PID ${pid}) did not shut down within 5s; sending SIGKILL" >&2
      kill -KILL "${pid}" 2>/dev/null || true
      sleep 0.2
    fi
    wait "${pid}" 2>/dev/null || true
  fi
}

cleanup() {
  local exit_code=$?
  trap - EXIT INT TERM
  terminate_process "${KIOSK_PID}" "kiosk daemon"
  terminate_process "${CLOUD_PID}" "cloud server"
  if [ "${exit_code}" -ne 0 ] && [ "${SMOKE_TEST_MODE:-0}" -eq 1 ]; then
    echo "=== Cloud Server Log ===" >&2
    [ -f "${CLOUD_LOG}" ] && cat "${CLOUD_LOG}" >&2 || true
    echo "=== Kiosk Daemon Log ===" >&2
    [ -f "${KIOSK_LOG}" ] && cat "${KIOSK_LOG}" >&2 || true
  fi
  rm -rf "${TEMP_DIR}"
  exit "${exit_code}"
}
trap cleanup EXIT INT TERM

# Build temporary binaries
CLOUD_BIN="${TEMP_DIR}/xeroxgo-cloud"
KIOSK_BIN="${TEMP_DIR}/xeroxgo-kiosk"

(cd "${REPO_ROOT}" && CGO_ENABLED=0 go build -o "${CLOUD_BIN}" ./cloud-backend/cmd/server)
(cd "${REPO_ROOT}" && CGO_ENABLED=0 go build -o "${KIOSK_BIN}" ./kiosk-daemon/cmd/daemon)

# Start Cloud Server
"${CLOUD_BIN}" > "${CLOUD_LOG}" 2>&1 &
CLOUD_PID=$!

# Wait for cloud server healthz
HEALTH_READY=0
for i in $(seq 1 50); do
  if python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:${PORT}/healthz', timeout=1)" >/dev/null 2>&1; then
    HEALTH_READY=1
    break
  fi
  sleep 0.1
done

if [ "${HEALTH_READY}" -ne 1 ]; then
  echo "Error: cloud server failed to become ready at http://127.0.0.1:${PORT}/healthz" >&2
  cat "${CLOUD_LOG}" >&2
  exit 1
fi

# Start Kiosk Daemon in mock-printer mode
"${KIOSK_BIN}" \
  --mock-printer \
  --preview-png="${PREVIEW_PNG}" \
  --cloud-url="${CLOUD_URL}" \
  --kiosk-id="${KIOSK_ID}" \
  --state-db="${KIOSK_DB}" \
  --download-dir="${KIOSK_JOBS}" \
  --idle-qr-content="${PORTAL_URL}" \
  > "${KIOSK_LOG}" 2>&1 &
KIOSK_PID=$!

# Wait for kiosk daemon initialization (idle screen generated)
KIOSK_READY=0
for i in $(seq 1 50); do
  if [ -f "${PREVIEW_PNG}" ] && [ -s "${PREVIEW_PNG}" ]; then
    KIOSK_READY=1
    break
  fi
  sleep 0.1
done

if [ "${KIOSK_READY}" -ne 1 ]; then
  echo "Error: kiosk daemon failed to initialize or produce idle preview" >&2
  cat "${KIOSK_LOG}" >&2
  exit 1
fi

if [ "${1:-}" = "--smoke-test" ]; then
  SMOKE_TEST_MODE=1
fi

if [ "${SMOKE_TEST_MODE}" -eq 1 ]; then
  echo "Running XeroxGo Automated End-to-End Smoke Test..."

  python3 - "${PORT}" "${KIOSK_ID}" "${TEMP_DIR}" "${PREVIEW_PNG}" "${CLOUD_JOBS}" "${KIOSK_DB}" << 'EOF'
import sys
import os
import time
import json
import struct
import sqlite3
import subprocess
import urllib.request
import urllib.error

port = sys.argv[1]
kiosk_id = sys.argv[2]
temp_dir = sys.argv[3]
preview_png = sys.argv[4]
cloud_jobs = sys.argv[5]
kiosk_db = sys.argv[6]

base_url = f"http://127.0.0.1:{port}"

# 1. Generate 3-page test PDF fixture using PostScript + Ghostscript
ps_path = os.path.join(temp_dir, "test_doc.ps")
pdf_path = os.path.join(temp_dir, "test_doc.pdf")

ps_content = """%!PS-Adobe-3.0
%%Pages: 3
%%Page: 1 1
/Helvetica findfont 24 scalefont setfont 100 700 moveto (XeroxGo Demo Page 1) show showpage
%%Page: 2 2
/Helvetica findfont 24 scalefont setfont 100 700 moveto (XeroxGo Demo Page 2) show showpage
%%Page: 3 3
/Helvetica findfont 24 scalefont setfont 100 700 moveto (XeroxGo Demo Page 3) show showpage
"""
with open(ps_path, "w") as f:
    f.write(ps_content)

cmd = ["gs", "-sDEVICE=pdfwrite", "-sPAPERSIZE=a4", "-dFIXEDMEDIA", "-dNOPAUSE", "-dBATCH", "-dQUIET", f"-sOutputFile={pdf_path}", ps_path]
res = subprocess.run(cmd, capture_output=True, text=True)
if res.returncode != 0:
    print(f"Error generating PDF fixture: {res.stderr}", file=sys.stderr)
    sys.exit(1)

with open(pdf_path, "rb") as f:
    pdf_bytes = f.read()

# 2. Upload PDF with 2 copies, DUPLEX_LONG_EDGE
boundary = "----WebKitFormBoundaryXeroxGoSmokeTest7MA4YWxkTrZu0gW"
body = bytearray()

# file field
body.extend(f"--{boundary}\r\n".encode())
body.extend(b'Content-Disposition: form-data; name="file"; filename="test_doc.pdf"\r\n')
body.extend(b"Content-Type: application/pdf\r\n\r\n")
body.extend(pdf_bytes)
body.extend(b"\r\n")

# copies field
body.extend(f"--{boundary}\r\n".encode())
body.extend(b'Content-Disposition: form-data; name="copies"\r\n\r\n')
body.extend(b"2\r\n")

# duplex field
body.extend(f"--{boundary}\r\n".encode())
body.extend(b'Content-Disposition: form-data; name="duplex"\r\n\r\n')
body.extend(b"DUPLEX_LONG_EDGE\r\n")
body.extend(f"--{boundary}--\r\n".encode())

upload_url = f"{base_url}/api/k/{kiosk_id}/upload"
req = urllib.request.Request(
    upload_url,
    data=bytes(body),
    headers={"Content-Type": f"multipart/form-data; boundary={boundary}"},
    method="POST"
)

with urllib.request.urlopen(req, timeout=10) as resp:
    if resp.status != 200:
        print(f"Upload failed with status {resp.status}", file=sys.stderr)
        sys.exit(1)
    upload_data = json.loads(resp.read().decode())

order_id = upload_data.get("order_id")
total_pages = upload_data.get("total_pages")
total_sheets = upload_data.get("total_sheets")
price_paise = upload_data.get("total_price_paise")

# Verification of calculation:
# 3 pages, duplex long edge = ceil(3/2) = 2 sheets per copy.
# 2 copies = 4 sheets total.
# 4 sheets * 500 paise = 2000 paise.
if total_pages != 3:
    print(f"Expected 3 total pages, got {total_pages}", file=sys.stderr)
    sys.exit(1)
if total_sheets != 4:
    print(f"Expected 4 total sheets, got {total_sheets}", file=sys.stderr)
    sys.exit(1)
if price_paise != 2000:
    print(f"Expected price 2000 paise, got {price_paise}", file=sys.stderr)
    sys.exit(1)

# 3. Simulate payment
sim_url = f"{base_url}/api/orders/{order_id}/simulate-payment"
sim_req = urllib.request.Request(sim_url, data=b"", method="POST")
with urllib.request.urlopen(sim_req, timeout=10) as resp:
    if resp.status != 200:
        print(f"Payment simulation failed with status {resp.status}", file=sys.stderr)
        sys.exit(1)
    sim_data = json.loads(resp.read().decode())
    if sim_data.get("status") not in ("DISPATCHED", "PAID_WAITING"):
        print(f"Unexpected status after payment simulation: {sim_data}", file=sys.stderr)
        sys.exit(1)

# 4. Poll status until COMPLETED
status_url = f"{base_url}/api/orders/{order_id}/status"
deadline = time.time() + 45.0
completed = False

while time.time() < deadline:
    try:
        with urllib.request.urlopen(status_url, timeout=5) as resp:
            st = json.loads(resp.read().decode())
            if st.get("status") == "COMPLETED":
                if st.get("pages_printed") == 3 and st.get("sheets_printed") == 4:
                    completed = True
                    break
                else:
                    print(f"Completed with unexpected counts: {st}", file=sys.stderr)
                    sys.exit(1)
            elif st.get("status") in ("PAUSED_ERROR", "FAILED_REFUNDABLE"):
                print(f"Job reached error status: {st}", file=sys.stderr)
                sys.exit(1)
    except Exception as e:
        pass
    time.sleep(0.4)

if not completed:
    print(f"Timeout waiting for order {order_id} to reach COMPLETED", file=sys.stderr)
    sys.exit(1)

# 5. Verify Preview PNG (1920x1080)
if not os.path.exists(preview_png) or os.path.getsize(preview_png) == 0:
    print(f"Preview PNG missing or empty at {preview_png}", file=sys.stderr)
    sys.exit(1)

with open(preview_png, "rb") as f:
    png_hdr = f.read(24)
    if png_hdr[:8] != b"\x89PNG\r\n\x1a\n":
        print("Invalid PNG header", file=sys.stderr)
        sys.exit(1)
    width, height = struct.unpack(">II", png_hdr[16:24])
    if width != 1920 or height != 1080:
        print(f"Invalid preview dimensions: expected 1920x1080, got {width}x{height}", file=sys.stderr)
        sys.exit(1)

# 6. Verify Normalized PDF exists under temporary cloud jobs directory
cloud_files = [f for f in os.listdir(cloud_jobs) if f.endswith(".pdf")]
if not cloud_files:
    print("No normalized PDF found under cloud jobs directory", file=sys.stderr)
    sys.exit(1)

# 7. Verify Kiosk Database has persisted state
con = sqlite3.connect(kiosk_db)
cur = con.cursor()
cur.execute("SELECT status, pages_printed, sheets_printed FROM jobs")
row = cur.fetchone()
con.close()

if not row:
    print("No job record in kiosk database", file=sys.stderr)
    sys.exit(1)
if row[0] != "COMPLETED" or row[1] != 3 or row[2] != 4:
    print(f"Kiosk DB job state invalid: {row}", file=sys.stderr)
    sys.exit(1)

print("SMOKE TEST PASSED: All end-to-end integration checks succeeded.")
EOF

  echo "✅ End-to-end smoke test passed successfully."
  exit 0
fi

# Interactive mode
echo "=========================================================="
echo " XeroxGo local demo is running."
echo " Student portal: ${PORTAL_URL}"
echo " HDMI preview:   ${PREVIEW_PNG}"
echo " Printer:        local TCP mock (no physical printer required)"
echo " Dev Mode:       Active (simulated payments enabled)"
echo "=========================================================="
echo "Press Ctrl+C to stop the demo."

# Keep running until interrupted
while true; do
  sleep 1
done
