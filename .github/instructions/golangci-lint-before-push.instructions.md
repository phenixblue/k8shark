---
description: "Use when performing git push operations, updating PR branches, or preparing PR-ready commits. Enforces running golangci-lint exactly like CI before any push."
---

# Run GolangCI-Lint Before Push

Before any `git push` to a branch used for a PR:

1. Install the pinned CI lint version:
   - `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0`
2. Run the same lint command as CI from repo root:
   - `golangci-lint run`

Rules:

- Do not push if lint fails.
- Use the same execution format as CI (no extra flags).
- Report lint result before pushing (pass/fail and any fixes applied).
- If local PATH/version mismatch prevents lint execution, resolve that first, then rerun lint before pushing.
