# Engineering workflow: ChatGPT + Antigravity

Use a controlled handoff. ChatGPT should act as the product/architecture/security reviewer; Antigravity should operate on the checked-out repository, edit files, run commands, and produce the implementation diff. Avoid two agents independently editing the same files.

## Responsibilities

### ChatGPT — engineering lead and reviewer

Use ChatGPT to:
- Turn a feature request into requirements and acceptance criteria.
- Inspect the relevant repository code and map the affected components.
- Propose data-model, API, security, error-handling, and observability changes.
- Identify migration and backward-compatibility risks.
- Write a precise implementation brief for Antigravity.
- Review the resulting diff, test output, and deployment plan.

Ask for concrete file paths, failure cases, and test cases. Do not accept a broad rewrite when a scoped change will solve the problem.

### Antigravity — repository implementation environment

Use Antigravity to:
- Open the local `XeroxGo` Git checkout.
- Inspect the current branch and working tree before editing.
- Make the approved changes in a feature branch.
- Run formatting, unit tests, verification gates, and local smoke tests.
- Show the diff and summarize every file changed.
- Never commit, push, deploy, or modify production data without an explicit human decision.

## Per-feature cycle

1. **Create an issue/brief.** Record the user problem, desired behavior, acceptance criteria, non-goals, and risks.
2. **Ask ChatGPT for a technical plan.** Require the plan to identify the exact files, APIs, database changes, security boundaries, and test cases.
3. **Create a feature branch.** Example: `git switch develop && git pull && git switch -c feat/print-job-cancellation`.
4. **Give Antigravity the approved plan.** Have it inspect first and report the intended edits before implementation for high-risk work.
5. **Implement one bounded change.** Keep database and shared-contract changes explicit; do not casually alter `pkg/contracts/models.go`.
6. **Verify locally.** At minimum run `gofmt` on changed Go files, `go test ./...`, `./scripts/verify_gate.sh`, and `git diff --check`. Run the integrated demo for changes to upload, pricing, payment, status, or kiosk communication.
7. **Ask ChatGPT to review the diff.** Include the diff, test output, and any assumptions. Request a review for correctness, data loss, authorization, race conditions, and deployment impact.
8. **Open a pull request.** CI must pass. Review the code and the UI preview before merge.
9. **Release through `main`.** Merge only after staging acceptance checks. Let GitHub, Vercel, and Render deploy from the connected repository.
10. **Verify production.** Check health, logs, customer upload, admin access, job persistence, WebSocket reconnect, and printer behavior.

## ChatGPT planning prompt

```text
Act as the senior engineer for XeroxGo. Read the relevant repository files before proposing changes.
Do not rewrite unrelated parts of the app.

For this request, produce:
1. User problem and expected behavior
2. Acceptance criteria and non-goals
3. Exact files/components likely to change
4. API and database impact, including migration/backward-compatibility risks
5. Security, privacy, and failure-mode analysis
6. Unit, integration, and manual test plan
7. Deployment and rollback considerations

Do not invent existing functions or schemas. Identify unknowns explicitly.
Do not output implementation code until the plan is agreed.
```

## Antigravity implementation prompt

```text
You are implementing an approved change in the existing XeroxGo monorepo.
First inspect the current branch, git status, and the relevant code.
Do not overwrite unrelated work or perform a broad rewrite.

Implement only the approved scope. Preserve existing behavior unless the acceptance
criteria explicitly change it. Do not put secrets in source files. Do not commit,
push, deploy, or change production data.

After implementation:
- format changed Go files with gofmt
- run relevant unit tests and the full verification script
- run git diff --check
- show the complete diff
- list files changed and explain why
- list tests run, results, and any tests not run
- identify remaining risks and manual checks
```

## Operating rules

- Keep one active coding agent per feature branch. Do not ask ChatGPT and Antigravity to modify the same branch at the same time.
- Keep changes small enough to review. Avoid mixing UI redesign, payment changes, database migrations, and deployment changes in one pull request.
- Never paste real Supabase passwords, payment secrets, admin credentials, private customer documents, or production logs containing personal data into prompts.
- Treat agent output as a proposal until tests and human review confirm it.
- Make database schema changes through reviewed, versioned migrations as that system is introduced.
- Use test-mode payments and a non-production Supabase project for integration work.
