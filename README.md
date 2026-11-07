# XeroxGo

XeroxGo is a campus self-service printing platform. This repository is a Go monorepo containing the cloud API, static web portal, admin dashboard, Raspberry Pi kiosk daemon, shared contracts, and deployment configuration.

## Architecture

| Component | Technology | Deployment |
|---|---|---|
| Customer portal and admin UI | Static HTML, CSS, JavaScript, PWA | Vercel (`frontend/`) |
| Cloud API | Go, REST, WebSockets, PDF conversion | Render (Docker) |
| Relational data | PostgreSQL | Supabase |
| Print-job files | Local filesystem today | Render persistent disk at `/data` |
| Physical kiosk | Go daemon, Raspberry Pi OS, CUPS | Raspberry Pi device |
| Shared messages and types | Go module | `pkg/contracts/` |

The cloud backend and kiosk daemon communicate through WebSockets. The current backend writes uploaded and converted files to its local jobs directory, so the Render service must have a persistent disk until object storage is implemented.

## Repository layout

```text
.
├── cloud-backend/       # Go HTTP/WebSocket API and PDF processing
├── frontend/            # Static customer portal and admin dashboard
├── kiosk-daemon/        # Raspberry Pi print spooler and display daemon
├── pkg/contracts/       # Shared Go contracts
├── deploy/
│   ├── rpi-os/          # systemd service configuration
│   └── supabase/        # SQL reference
├── design/              # UI screen designs
├── scripts/             # Local demo, verification, and device scripts
├── Dockerfile           # Cloud backend container
├── render.yaml          # Render Blueprint
└── go.work              # Go workspace
```

## Requirements

- Go **1.26.9** (matches the workspace modules).
- Git.
- Docker for reproducing the Render container build locally.
- For local PDF conversion: `qpdf`, Ghostscript, LibreOffice, and Poppler utilities.

## Local development

The supported integrated demo is:

```bash
./scripts/run_local_demo.sh
```

It starts the cloud backend and a simulated kiosk/printer flow. The script enables development payment simulation; do not expose that mode to the public internet.

Run the code verification suite from the repository root:

```bash
./scripts/verify_gate.sh
```

The CI workflow runs the same verification gates. `go test ./...` is also useful for a fast Go workspace test run.

`.env.example` documents the environment variable names. The Go binary does **not** automatically load `.env`; export variables in your shell or use the local demo script.

## Git workflow

- `main` is the production branch.
- `develop` is the integration branch.
- Use short-lived branches such as `feat/job-cancellation`, `fix/payment-state`, or `chore/ci-cache`.
- Open pull requests into `develop`. Merge a tested release from `develop` into `main`.
- Do not commit secrets, `.env` files, PDF job files, database files, or generated binaries.
- Keep `pkg/contracts/models.go` stable unless a coordinated protocol change is explicitly planned; the verification script checks its baseline.

Example first push (create an empty **private** GitHub repository named `XeroxGo` first):

```bash
git init -b main
git add .
git status --short
git commit -m "chore: initialize XeroxGo monorepo"
git remote add origin https://github.com/<YOUR_GITHUB_ACCOUNT>/XeroxGo.git
git push -u origin main

git switch -c develop
git push -u origin develop
```

Alternatively, after the first commit, GitHub CLI can create and push the private repository:

```bash
gh auth login
gh repo create XeroxGo --private --source=. --remote=origin --push
```

For the detailed hosting steps, see [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md). For the engineering workflow using ChatGPT and Antigravity, see [`docs/ENGINEERING_WORKFLOW.md`](docs/ENGINEERING_WORKFLOW.md).

## Important current limitations

This repository is a working foundation, not yet a production payment-and-storage implementation:

1. The customer portal currently contains a placeholder Razorpay key (`rzp_test_placeholder`). A real payment integration needs server-side order creation, amount verification, signature verification, idempotent webhook handling, and a full test-mode validation before accepting live payments.
2. Uploaded and converted files are stored on the backend filesystem. The Render Blueprint mounts a paid persistent disk at `/data` for the current single-instance deployment. For horizontal scaling and stronger durability, implement an object-storage adapter (for example, Supabase Storage) and store object keys rather than local paths.
3. `deploy/supabase/schema.sql` is not the complete schema source. The Go store currently creates and upgrades additional tables at startup. A versioned migration system should replace ad-hoc startup DDL before multiple environments and production releases are managed.
4. Do not treat development payment simulation as a live payment method.

## Security

The original supplied archive contained a Supabase database connection string with an embedded password. The credential has been removed from this prepared copy, but it must be rotated in Supabase before any deployment. If the original repository was previously pushed, assume the old credential remains exposed in Git history and rotate it regardless of whether history is later rewritten.

Never put secrets in source code, `render.yaml`, frontend files, GitHub commits, screenshots, or ChatGPT/Antigravity prompts. Store production secrets in Render environment variables.
