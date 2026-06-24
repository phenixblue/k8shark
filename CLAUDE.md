# CLAUDE.md

Guidance for Claude Code when working in this repository.

## Code review notes

- **Copilot's "map literal won't compile" warnings are false positives.** Copilot
  repeatedly flags `capture.Index` map literals such as

  ```go
  capture.Index{
      "/api/v1/...": {APIPath: "/api/v1/...", Seqs: []int{0}},
  }
  ```

  claiming they need `&capture.IndexEntry{...}` because `Index` is
  `map[string]*IndexEntry` (see `internal/capture/record.go`). This is valid Go:
  the spec lets you elide the `&T` for map/array/slice values whose element type
  is a pointer to a composite literal ("elements or keys that are addresses of
  composite literals may elide the `&T` when the element or key type is `*T`").
  CI compiles and runs these tests. Dismiss this comment rather than "fixing" it.
