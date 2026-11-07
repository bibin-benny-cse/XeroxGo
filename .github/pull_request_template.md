## Summary

Describe the user or operational problem this PR solves.

## Scope

- What changed?
- What is deliberately not included?

## Acceptance criteria

- [ ] User-visible behavior matches the agreed requirements.
- [ ] Error and recovery behavior is defined.
- [ ] Authorization and input validation are covered.

## Verification

- [ ] `gofmt` applied to changed Go files.
- [ ] Relevant unit/integration tests pass.
- [ ] `./scripts/verify_gate.sh` passes.
- [ ] `git diff --check` passes for this PR.
- [ ] Manual UI / API / kiosk smoke test completed where applicable.

## Data and deployment impact

- Database migration required: Yes / No
- Environment variables changed: Yes / No
- Render / Vercel configuration changed: Yes / No
- Rollback or compatibility notes:

## Security and privacy

- [ ] No credentials, tokens, private documents, or personal data added.
- [ ] New endpoints enforce the correct authorization and validation.
- [ ] Logs do not expose secrets or document contents.
