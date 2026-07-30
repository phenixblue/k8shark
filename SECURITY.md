# Security Policy

## Reporting a vulnerability

**Please do not open a public GitHub issue for security vulnerabilities.**
Issues are public by default, and a vulnerability report filed that way
discloses it to everyone before a fix exists.

Instead, use GitHub's private vulnerability reporting for this repository:

1. Go to the [Security tab](https://github.com/phenixblue/k8shark/security).
2. Click **Report a vulnerability**.
3. Describe the issue, affected version(s), and reproduction steps if you
   have them.

This opens a private draft security advisory visible only to you and the
maintainers — nothing is public until a fix is ready and you and the
maintainers agree to disclose.

## What to expect

- **Acknowledgment**: within 5 business days.
- **Fix timeline**: depends on severity and complexity; we'll share an
  estimate once the report is triaged.
- **Credit**: reporters are credited in the published advisory unless they
  ask not to be.

## Scope

k8shark captures Kubernetes cluster state into a `.kshrk` archive and replays
it through a local mock API server. Vulnerability reports are especially
relevant if they involve:

- The archive format or its parsing (`internal/archive`) — a `.kshrk` file is
  designed to be handed between organizations, so a maliciously crafted
  archive is a real threat model (decompression bombs, path traversal on
  extraction, malformed ZIP/JSON handling).
- Archive encryption (`--encrypt`/`--encrypt-recipient`) — see
  [docs/encryption-threat-model.md](docs/encryption-threat-model.md) for what
  it's designed to protect against; a gap relative to that model is a
  security bug.
- The mock API server (`internal/server`) — request handling, the writable
  overlay, or anything that could let a client escape the sandboxed,
  localhost-only server.
- Redaction (`internal/redact`) — a redaction rule that fails to remove
  sensitive data it claims to remove.

## Supported versions

Only the latest released version (including release candidates) receives
security fixes — see the [backport policy](docs/stability-policy.md#backport--patch-policy).
