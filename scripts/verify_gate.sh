#!/usr/bin/env bash
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

FAILED=0

echo "Starting XeroxGo Monorepo Verification Gate..."
echo "Working directory: ${REPO_ROOT}"
echo ""

# -----------------------------------------------------------------------------
# Gate 1 — Frozen Contracts
# -----------------------------------------------------------------------------
echo "=== Gate 1: Frozen Contracts ==="
gate1_passed=true

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    echo "ERROR: Git repository is not initialized."
    gate1_passed=false
fi

if [[ ! -f "pkg/contracts/models.go" ]]; then
    echo "ERROR: pkg/contracts/models.go does not exist."
    gate1_passed=false
fi

if [[ "$gate1_passed" == "true" ]]; then
    EARLIEST_COMMIT=$(git rev-list --reverse HEAD -- pkg/contracts/models.go 2>/dev/null | head -n 1 || true)
    if [[ -z "$EARLIEST_COMMIT" ]]; then
        echo "Notice: Baseline check deferred until first commit (no commit in git history contains pkg/contracts/models.go)."
        echo "Gate 1 PASSED: models.go exists; awaiting initial commit baseline."
    else
        CURRENT_SHA=$(sha256sum pkg/contracts/models.go | awk '{print $1}')
        COMMITTED_SHA=$(git show "${EARLIEST_COMMIT}:pkg/contracts/models.go" 2>/dev/null | sha256sum | awk '{print $1}')
        if [[ -z "$COMMITTED_SHA" ]]; then
            echo "ERROR: Could not retrieve pkg/contracts/models.go from baseline commit ${EARLIEST_COMMIT}."
            gate1_passed=false
        elif [[ "$CURRENT_SHA" != "$COMMITTED_SHA" ]]; then
            echo "ERROR: Frozen contract changed! SHA256 mismatch against baseline commit ${EARLIEST_COMMIT}."
            echo "  Current:   $CURRENT_SHA"
            echo "  Baseline:  $COMMITTED_SHA"
            gate1_passed=false
        else
            echo "Gate 1 PASSED: pkg/contracts/models.go matches frozen baseline from commit ${EARLIEST_COMMIT} (${CURRENT_SHA})."
        fi
    fi
fi

if [[ "$gate1_passed" != "true" ]]; then
    echo "Gate 1 FAILED"
    FAILED=1
fi
echo ""

# -----------------------------------------------------------------------------
# Gate 2 — Ban the CGO SQLite Driver
# -----------------------------------------------------------------------------
echo "=== Gate 2: Ban CGO SQLite Driver ==="
gate2_passed=true

mapfile -d '' go_files < <(find . -type d \( -name .git -o -name bin \) -prune -o -type f -name '*.go' -print0)

if [[ ${#go_files[@]} -eq 0 ]]; then
    echo "ERROR: No Go source files found to scan."
    gate2_passed=false
else
    set +e
    grep_output=$(grep -n "mattn/go-sqlite3" "${go_files[@]}" 2>&1)
    grep_status=$?
    set -uo pipefail

    if [[ $grep_status -eq 0 ]]; then
        echo "ERROR: Forbidden CGO SQLite driver 'mattn/go-sqlite3' found in Go source files:"
        echo "$grep_output"
        gate2_passed=false
    elif [[ $grep_status -eq 1 ]]; then
        echo "Gate 2 PASSED: Zero occurrences of 'mattn/go-sqlite3' across ${#go_files[@]} Go source files."
    else
        echo "ERROR: grep failed while scanning Go source files (exit code ${grep_status}):"
        echo "$grep_output"
        gate2_passed=false
    fi
fi

if [[ "$gate2_passed" != "true" ]]; then
    echo "Gate 2 FAILED"
    FAILED=1
fi
echo ""

# -----------------------------------------------------------------------------
# Gate 3 — Go Vet
# -----------------------------------------------------------------------------
echo "=== Gate 3: Go Vet ==="
if go vet ./pkg/contracts/... ./cloud-backend/... ./kiosk-daemon/...; then
    echo "Gate 3 PASSED: go vet reported no issues."
else
    echo "Gate 3 FAILED: go vet reported issues."
    FAILED=1
fi
echo ""

# -----------------------------------------------------------------------------
# Gate 4 — Required Builds
# -----------------------------------------------------------------------------
echo "=== Gate 4: Required Builds ==="
mkdir -p bin
gate4_passed=true

echo "Building kiosk daemon (linux/arm64, CGO_ENABLED=0)..."
if ! CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
  -o bin/xeroxgo-kiosk-arm64 \
  ./kiosk-daemon/cmd/daemon; then
    echo "ERROR: Failed to build bin/xeroxgo-kiosk-arm64"
    gate4_passed=false
fi

echo "Building cloud backend (host, CGO_ENABLED=0)..."
if ! CGO_ENABLED=0 go build \
  -o bin/xeroxgo-cloud \
  ./cloud-backend/cmd/server; then
    echo "ERROR: Failed to build bin/xeroxgo-cloud"
    gate4_passed=false
fi

if [[ "$gate4_passed" == "true" ]]; then
    echo "Gate 4 PASSED: Both required binaries built successfully."
else
    echo "Gate 4 FAILED"
    FAILED=1
fi
echo ""

# -----------------------------------------------------------------------------
# Gate 5 — Race-Enabled Test Suite
# -----------------------------------------------------------------------------
echo "=== Gate 5: Race-Enabled Test Suite ==="
if go test -v -race ./pkg/contracts/... ./cloud-backend/... ./kiosk-daemon/...; then
    echo "Gate 5 PASSED: All tests passed with race detector enabled."
else
    echo "Gate 5 FAILED: Tests failed."
    FAILED=1
fi
echo ""

# -----------------------------------------------------------------------------
# Final Verdict
# -----------------------------------------------------------------------------
echo "=== Verification Summary ==="
if [[ $FAILED -eq 0 ]]; then
    echo "✅ ALL GATES PASSED"
    exit 0
else
    echo "❌ VERIFICATION FAILED"
    exit 1
fi
