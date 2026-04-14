---
mode: agent
description: "Use when preparing a branch to push for PR review or release readiness. Runs tests and golangci-lint in CI-compatible mode before pushing."
---

Prepare the current branch for push and PR update.

## Required checks

1. Run formatting:
   - `make fmt`
2. Run repository tests:
   - `make test`
3. Run golangci-lint exactly like CI:
   - `make lint-ci-install`
   - `make lint-ci`

## Push workflow

1. Summarize what will be pushed (branch, remote, commits).
2. Ask for explicit confirmation before running `git push`.
3. Push only after confirmation.
4. Report check results and push outcome.

## If checks fail

- Stop before push.
- Show the failing command output summary.
- Propose minimal fixes, apply them if requested, then rerun checks.
