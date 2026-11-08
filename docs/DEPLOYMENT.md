# Deployment guide

Target layout:

- **GitHub** stores the source and runs CI.
- **Vercel** serves the static frontend from `frontend/`.
- **Render** runs the Go cloud backend from the repository-root `Dockerfile`.
- **Supabase** provides PostgreSQL.
- **Raspberry Pi** runs the on-site kiosk daemon and connects to Render over `wss://`.

## 0. Security first

The original ZIP contained a PostgreSQL connection string with a password in `render.yaml`. Before deploying:

1. Open the existing Supabase project and rotate/reset its database password.
2. Treat the old password as compromised. Do not reuse it.
3. Do not push the original ZIP as-is. Use this cleaned repository copy.
4. If the original version was already pushed to any Git host, assume the credential is still present in commit history. Rotation is mandatory; history rewriting alone is not a substitute.
5. Keep the GitHub repository private while the product and security model are being developed.

The prepared repository also removes production fallback admin credentials and changes the static Vercel portal to `data-dev-mode="false"`.

## 1. Create a fresh Git repository

The source ZIP has no `.git` history. In a terminal, open the extracted `XeroxGo-starter` folder and run:

```bash
git init -b main
git add .
git status --short
git diff --cached --check
git commit -m "chore: initialize XeroxGo monorepo"
```

Create an empty **private** GitHub repository called `XeroxGo`, without adding a README, `.gitignore`, or license on GitHub. Then:

```bash
git remote add origin https://github.com/<YOUR_GITHUB_ACCOUNT>/XeroxGo.git
git push -u origin main
git switch -c develop
git push -u origin develop
```

Check the remote and branch:

```bash
git remote -v
git branch -vv
```

Do not use `git add .` until you have checked `git status`; verify that no `.env`, PDFs, `.db` files, keys, or credentials are staged.

## 2. Create the Supabase project

1. Create a Supabase project. Choose a region close to the Render service region; the supplied Render Blueprint is set to Frankfurt.
2. Set a new database password. Do not reuse the old password.
3. In the Supabase dashboard, open **Connect** and copy a PostgreSQL connection string.
4. The Render backend is a long-running Go service. Prefer the direct connection when the Render network can reach the Supabase IPv6 endpoint. If it cannot, use Supabase's shared **Session pooler** connection (IPv4). Do not choose the transaction pooler just because its port is `6543`; transaction pooling is primarily intended for short-lived/serverless connections.
5. Keep the connection string private. You will add it to Render as `DATABASE_URL`, not to this repository.

The Go backend creates its current tables and applies its current additive schema changes when it opens the database. The file `deploy/supabase/schema.sql` is not a complete, versioned production migration set; do not assume it creates every table the application needs. Consolidating the DDL into numbered migrations is a follow-up engineering task.

### File storage note

The current application stores print and conversion PDFs on the backend filesystem, not in Supabase Storage. This prepared Render configuration mounts a persistent disk at `/data`. A later improvement should move those blobs to Supabase Storage (or another object store) behind a storage interface.

## 3. Deploy the Go backend to Render

1. In Render, select **New → Blueprint**.
2. Connect the new GitHub repository and select `main`.
3. Render reads the repository-root `render.yaml`; confirm the service is named `xeroxgo-cloud`.
4. The Blueprint uses the root `Dockerfile`, region `frankfurt`, a `1c-2g` service plan, a 10 GB persistent disk mounted at `/data`, and `/healthz` as its health check. This is a paid production-oriented configuration. You can use a free instance for a disposable demo only, not for real print jobs.
5. During Blueprint setup or from the service's Environment page, set:
   - `DATABASE_URL`: the new Supabase connection string.
   - `XEROXGO_ADMIN_USER`: a unique admin username.
   - `XEROXGO_ADMIN_PASSWORD`: a unique password of at least 16 characters.
   - `XEROXGO_ALLOWED_ORIGINS`: comma-separated exact frontend origins, for example `https://<your-vercel-project>.vercel.app`.
6. Keep `XEROXGO_DEV_MODE=0`.
7. Render generates `XEROXGO_DOWNLOAD_SECRET` and `XEROXGO_WEBHOOK_SECRET`. Do not expose them. When Razorpay webhook integration is completed, the webhook secret configured at Razorpay must match the value expected by the backend.
8. Confirm `XEROXGO_JOBS_DIR=/data/jobs` and `XEROXGO_PUBLIC_URL` matches the actual Render service URL.
9. Deploy and inspect the service logs.

Test the health endpoint:

```text
https://<your-render-service>.onrender.com/healthz
```

It should return a successful health response. A green health check does not prove payments, file persistence, or printer connectivity; test those separately.

### Why the persistent disk is required

The current backend writes jobs to disk. Render's default filesystem is ephemeral, and free web services do not support persistent disks. The prepared Blueprint attaches `/data` so job files survive restarts and deploys. A Render disk is available to one service instance, so keep this deployment at one instance until files move to object storage.

## 4. Deploy the frontend to Vercel

1. In Vercel, select **Add New → Project** and import the GitHub `XeroxGo` repository.
2. Set **Root Directory** to `frontend`.
3. Select the **Other** framework preset. This is a static HTML/CSS/JavaScript app; there is no npm build step.
4. Leave the build command empty. Use `.` as the output directory if Vercel asks for one.
5. Deploy.

The existing `frontend/vercel.json` rewrites `/api/*` and `/healthz` to:

```text
https://xeroxgo-7s6i.onrender.com
```

If Render gives the service a different URL, replace that hostname in all three relevant places in `frontend/vercel.json`:

- `/api/:match*` rewrite destination
- `/healthz` rewrite destination
- `connect-src` in the Content-Security-Policy header

Commit and push the change. Vercel will deploy again.

Then add the final Vercel domain to Render's `XEROXGO_ALLOWED_ORIGINS` (without a trailing slash). If you use a custom domain, include that origin too.

## 5. Git-based deployment workflow

The repository's GitHub Actions workflow runs the existing verification gates for pull requests and pushes to `main` or `develop`.

- Vercel is connected directly to GitHub: pull requests get preview deployments; merges to its production branch deploy production.
- Render is connected through the Blueprint and is configured to deploy after repository checks pass.
- Do not add long-lived Vercel deployment tokens to GitHub Actions unless there is a specific reason to bypass the native Git integration.

Recommended release path:

```text
feature branch → pull request → CI green → merge to develop
develop → release pull request → CI green → merge to main
main → Render production deploy + Vercel production deploy
```

Protect `main` in GitHub: require pull requests, require the CI verification job, and disallow force pushes.

## 6. Production acceptance checks

Before connecting a physical kiosk or accepting public orders, verify each layer:

- `https://<render-domain>/healthz` returns success.
- `https://<vercel-domain>/healthz` reaches the Render backend through the Vercel rewrite.
- Customer portal loads from Vercel and requests `/api/*` successfully.
- Admin login works with the credentials configured in Render; change the admin password after the first login if your operational policy requires it.
- A test upload converts and downloads the correct PDF.
- A test job survives a Render restart/redeploy (the `/data` disk is mounted correctly).
- The kiosk connects to `wss://<render-domain>/ws`.
- Razorpay is still a release blocker: the frontend currently contains `rzp_test_placeholder`, and the repository does not yet implement a complete production payment-order flow. Do not accept live payments until server-side order creation, amount binding, signature verification, webhook idempotency, and end-to-end test-mode payment validation are complete.
- Test a real printer only after the kiosk daemon has been configured with the correct printer queue and device address.

## 7. Environment variable ownership

| Variable | Where to set it | Purpose |
|---|---|---|
| `DATABASE_URL` | Render secret | Supabase PostgreSQL connection |
| `XEROXGO_DEV_MODE` | Render | Must be `0` for production |
| `XEROXGO_JOBS_DIR` | Render | `/data/jobs` on the persistent disk |
| `XEROXGO_PUBLIC_URL` | Render | Public backend URL used for generated download links |
| `XEROXGO_PRICE_PER_SHEET_PAISE` | Render, first boot only | Initial price seed; existing admin-managed DB pricing is preserved on later restarts |
| `XEROXGO_DOWNLOAD_SECRET` | Render generated secret | Signed PDF download URLs |
| `XEROXGO_WEBHOOK_SECRET` | Render generated secret | Payment webhook signature verification |
| `XEROXGO_ADMIN_USER` | Render secret/config | First admin account |
| `XEROXGO_ADMIN_PASSWORD` | Render secret | First admin password, minimum 16 characters |
| `XEROXGO_ALLOWED_ORIGINS` | Render config | Exact allowed browser origins |
| `PORT` | Supplied by Render | HTTP listen port; do not hard-code for cloud hosting |
