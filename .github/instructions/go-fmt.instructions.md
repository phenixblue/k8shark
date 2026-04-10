---
description: "Use when modifying Go source files. Enforces running make fmt after all Go changes are completed."
applyTo: "**/*.go"
---

After completing all pending Go code changes in a task, run `make fmt` before finishing.

- Run `make fmt` once — after **all** edits in the current task are done, not after each individual file change.
- Do not run `make fmt` mid-task while other edits are still pending.
- If `make fmt` produces a diff (reformatted files), stage those changes as part of the same commit.
