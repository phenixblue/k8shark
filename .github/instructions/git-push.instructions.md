---
description: "Use when performing any git operations. Never run git push, git push --force, or any remote-modifying git command without explicit user confirmation first."
---

Never run `git push` (or any variant: `--force`, `--force-with-lease`, `--tags`, etc.) without first asking the user for confirmation.

- `git add`, `git commit`, `git branch`, `git tag`, and read-only commands (`git log`, `git diff`, `git status`) may run freely.
- Before any `git push`, state what will be pushed (branch, remote, commits) and ask: "OK to push?"
- Wait for explicit confirmation before proceeding.

For pushes to branches that are used for PRs, also follow `golangci-lint-before-push.instructions.md` and run the same lint command/version as CI before pushing.
