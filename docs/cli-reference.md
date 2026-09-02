# CLI Reference

Generated from the live Cobra command tree by `make docs` (tools/gendocs) —
do not edit by hand. Run `make docs` after changing any command or flag
definition; CI's `contract` job fails if this file drifts from `cmd/`, the
same way it fails on gofmt or `go.mod` drift.

See [docs/usage.md](usage.md) for a curated, worked-example walkthrough of
the commands most people reach for first; this page is the exhaustive,
always-accurate flag reference for all of them, including the ones
usage.md doesn't cover in depth (`encrypt`, `decrypt`, `version`,
`completion`) and every global flag.

## kshrk

k8shark — Kubernetes cluster state capture and replay

### Synopsis

k8shark captures a Kubernetes cluster's state over time and replays
it through a mock API server, letting support engineers query a customer's
environment without direct connectivity.

### Options

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -h, --help                             help for kshrk
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk anonymize](#kshrk-anonymize)	 - Anonymize identifying values across a capture archive, consistently
* [kshrk capture](#kshrk-capture)	 - Capture Kubernetes cluster state to a .kshrk archive
* [kshrk completion](#kshrk-completion)	 - Generate the autocompletion script for the specified shell
* [kshrk decrypt](#kshrk-decrypt)	 - Decrypt an encrypted capture archive back to plaintext
* [kshrk diagnose](#kshrk-diagnose)	 - Analyze a capture and report likely problems
* [kshrk diff](#kshrk-diff)	 - Compare two capture snapshots
* [kshrk encrypt](#kshrk-encrypt)	 - Encrypt an existing capture archive after the fact
* [kshrk inspect](#kshrk-inspect)	 - Display a summary of a capture archive's contents
* [kshrk open](#kshrk-open)	 - Open a capture file and start a mock Kubernetes API server
* [kshrk query](#kshrk-query)	 - Search or run a JSONPath query across every captured object
* [kshrk redact](#kshrk-redact)	 - Redact Secret data and arbitrary fields from a capture archive
* [kshrk replay](#kshrk-replay)	 - Replay a capture forward through time at a chosen speed
* [kshrk transitions](#kshrk-transitions)	 - List resource state changes from a capture archive
* [kshrk ui](#kshrk-ui)	 - Open an interactive web explorer for a capture archive
* [kshrk validate](#kshrk-validate)	 - Validate a capture config file without connecting to a cluster
* [kshrk version](#kshrk-version)	 - Print the kshrk version


## kshrk anonymize

Anonymize identifying values across a capture archive, consistently

### Synopsis

Produces a new capture archive with values of the given categories replaced
by stable, deterministic aliases: the same original value maps to the same
alias everywhere it occurs in the archive, so relationships between objects
(a Pod's namespace, a Namespace's own identity, an Event's involvedObject)
stay intact. The original archive is not modified; the output defaults to
<in>-anonymized.kshrk.

This is a different tool than "kshrk redact": redact replaces one exact
field path with a fixed constant, everywhere it's configured to look;
anonymize replaces every occurrence of a value it recognizes, consistently,
using a deterministic alias derived from a salt.

Categories available: namespace, node, pod, workload, ip, url, image -- see
https://github.com/phenixblue/k8shark/issues/137.

Categories and field-path exclusion rules may also be loaded from a config
file's anonymize block via --config; see docs/config.md.

By default, a value is only anonymized at the field paths each category
already knows to look at. --full-sweep additionally replaces any substring
occurrence of a discovered namespace/node/pod/workload/ip/url value anywhere
else in a record's text (an Event message, an escaped-JSON-string
annotation, an unrecognized CRD's field) -- slower, and opt-in, since
matching a short or common value as a substring carries a real
false-positive risk. See https://github.com/phenixblue/k8shark/issues/361.

```
kshrk anonymize <capture.kshrk> --categories <category> [--out <anonymized.kshrk>] [flags]
```

### Examples

```
  # Anonymize every namespace name in a capture
  kshrk anonymize capture.kshrk --categories namespace

  # Anonymize multiple categories in one pass
  kshrk anonymize capture.kshrk --categories namespace --categories node --categories pod

  # Anonymize IPs, hostnames, and image registries
  kshrk anonymize capture.kshrk --categories ip --categories url --categories image

  # Reproduce the exact same aliases on a re-run
  kshrk anonymize capture.kshrk --categories namespace --anonymize-salt-file salt.txt

  # Categories and exclusion rules from a config file, as JSON output
  kshrk anonymize capture.kshrk --config k8shark.yaml -o json

  # Emit the original-to-alias mapping, encrypted to a recipient
  kshrk anonymize capture.kshrk --categories namespace --emit-mapping --encrypt-recipient age1abc...

  # Anonymize and write to a chosen path
  kshrk anonymize capture.kshrk --out safe.kshrk --categories namespace

  # Also catch occurrences outside known field paths (slower, opt-in)
  kshrk anonymize capture.kshrk --categories namespace --categories ip --full-sweep
```

### Options

```
      --anonymize-salt-file string       read the anonymize salt from this file (first line, hex-encoded) instead of $KSHRK_ANONYMIZE_SALT or generating one
      --categories stringArray           category to anonymize (repeatable); supported: namespace, node, pod, workload, ip, url, image
      --config string                    capture config file whose anonymize.categories/anonymize.rules block is applied
      --emit-mapping                     write the original-to-alias mapping alongside the output archive
      --emit-mapping-plaintext           allow --emit-mapping to write an unencrypted mapping when no --encrypt-* flag is set (not recommended)
      --encrypt                          encrypt the output archive with a passphrase (age); from --encrypt-passphrase-file, $KSHRK_ENCRYPT_PASSPHRASE, or an interactive prompt
      --encrypt-passphrase-file string   read the encryption passphrase from this file (first line) instead of prompting
      --encrypt-recipient stringArray    age recipient public key (age1...) to encrypt to (repeatable); mutually exclusive with passphrase encryption
      --encrypt-recipients-file string   file of age recipient public keys (one per line) to encrypt to
      --full-sweep                       also replace any substring occurrence of a discovered namespace/node/pod/workload/ip/url value anywhere in a record's text, not just at known field paths (slower, opt-in; see #361)
  -h, --help                             help for anonymize
      --mapping-path string              path for the mapping file (default: <out>.mapping.json, or .mapping.json.age when encrypted)
      --out string                       output archive path (default: <in>-anonymized.kshrk)
  -o, --output string                    output format: text or json (default "text")
```

### Options inherited from parent commands

```
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk](#kshrk)	 - k8shark — Kubernetes cluster state capture and replay


## kshrk capture

Capture Kubernetes cluster state to a .kshrk archive

### Synopsis

Runs a series of Kubernetes API read operations at defined intervals
for a configured duration. All responses are recorded and packaged into a
single .kshrk capture file for later replay.

Resources, namespaces, and intervals come from the --config file. Use
--auto-discover to capture every available API resource without listing them,
--out - to stream records as NDJSON to stdout, --redact-secrets to scrub
Secret values from the archive after capture, and --encrypt (passphrase) or
--encrypt-recipient (age public keys) to write the archive as an encrypted
(age) envelope.

```
kshrk capture [flags]
```

### Examples

```
  # Capture using a config file
  kshrk capture --config k8shark.yaml

  # Auto-discover and capture all resources for 5 minutes
  kshrk capture --auto-discover --duration 5m

  # Stream records as NDJSON to stdout instead of writing an archive
  kshrk capture --config k8shark.yaml --out -

  # Capture, then redact Secret values from the archive
  kshrk capture --config k8shark.yaml --redact-secrets

  # Capture and encrypt the archive, prompting for a passphrase
  kshrk capture --config k8shark.yaml --encrypt

  # Encrypt using a passphrase read from a file (no prompt)
  kshrk capture --config k8shark.yaml --encrypt-passphrase-file ./pass.txt

  # Encrypt to one or more age recipient public keys (shareable)
  kshrk capture --config k8shark.yaml --encrypt-recipient age1abc... --encrypt-recipient age1def...
```

### Options

```
      --allow-secret stringArray         namespace/name of secret to preserve when --redact-secrets is set (repeatable)
      --auto-discover                    auto-discover and capture all available API resources
      --duration string                  capture duration, overrides config file value (e.g. 10m, 1h)
      --encrypt                          encrypt the output archive with a passphrase (age); from --encrypt-passphrase-file, $KSHRK_ENCRYPT_PASSPHRASE, or an interactive prompt
      --encrypt-passphrase-file string   read the encryption passphrase from this file (first line) instead of prompting
      --encrypt-recipient stringArray    age recipient public key (age1...) to encrypt to (repeatable); mutually exclusive with passphrase encryption
      --encrypt-recipients-file string   file of age recipient public keys (one per line) to encrypt to
  -h, --help                             help for capture
      --kubeconfig string                path to kubeconfig (defaults to KUBECONFIG env, then ~/.kube/config)
      --out string                       output file path (default: ./k8shark-<timestamp>.kshrk)
      --redact-field stringArray         field redaction rule applied after capture: <fieldPath>:<Kind>:<replacement>[:<valueType>] (repeatable)
      --redact-secrets                   redact Secret data and stringData values from the archive after capture
```

### Options inherited from parent commands

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk](#kshrk)	 - k8shark — Kubernetes cluster state capture and replay


## kshrk completion

Generate the autocompletion script for the specified shell

### Synopsis

Generate the autocompletion script for kshrk for the specified shell.
See each sub-command's help for details on how to use the generated script.


### Options

```
  -h, --help   help for completion
```

### Options inherited from parent commands

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk](#kshrk)	 - k8shark — Kubernetes cluster state capture and replay
* [kshrk completion bash](#kshrk-completion-bash)	 - Generate the autocompletion script for bash
* [kshrk completion fish](#kshrk-completion-fish)	 - Generate the autocompletion script for fish
* [kshrk completion powershell](#kshrk-completion-powershell)	 - Generate the autocompletion script for powershell
* [kshrk completion zsh](#kshrk-completion-zsh)	 - Generate the autocompletion script for zsh


## kshrk completion bash

Generate the autocompletion script for bash

### Synopsis

Generate the autocompletion script for the bash shell.

This script depends on the 'bash-completion' package.
If it is not installed already, you can install it via your OS's package manager.

To load completions in your current shell session:

	source <(kshrk completion bash)

To load completions for every new session, execute once:

#### Linux:

	kshrk completion bash > /etc/bash_completion.d/kshrk

#### macOS:

	kshrk completion bash > $(brew --prefix)/etc/bash_completion.d/kshrk

You will need to start a new shell for this setup to take effect.


```
kshrk completion bash
```

### Options

```
  -h, --help              help for bash
      --no-descriptions   disable completion descriptions
```

### Options inherited from parent commands

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk completion](#kshrk-completion)	 - Generate the autocompletion script for the specified shell


## kshrk completion fish

Generate the autocompletion script for fish

### Synopsis

Generate the autocompletion script for the fish shell.

To load completions in your current shell session:

	kshrk completion fish | source

To load completions for every new session, execute once:

	kshrk completion fish > ~/.config/fish/completions/kshrk.fish

You will need to start a new shell for this setup to take effect.


```
kshrk completion fish [flags]
```

### Options

```
  -h, --help              help for fish
      --no-descriptions   disable completion descriptions
```

### Options inherited from parent commands

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk completion](#kshrk-completion)	 - Generate the autocompletion script for the specified shell


## kshrk completion powershell

Generate the autocompletion script for powershell

### Synopsis

Generate the autocompletion script for powershell.

To load completions in your current shell session:

	kshrk completion powershell | Out-String | Invoke-Expression

To load completions for every new session, add the output of the above command
to your powershell profile.


```
kshrk completion powershell [flags]
```

### Options

```
  -h, --help              help for powershell
      --no-descriptions   disable completion descriptions
```

### Options inherited from parent commands

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk completion](#kshrk-completion)	 - Generate the autocompletion script for the specified shell


## kshrk completion zsh

Generate the autocompletion script for zsh

### Synopsis

Generate the autocompletion script for the zsh shell.

If shell completion is not already enabled in your environment you will need
to enable it.  You can execute the following once:

	echo "autoload -U compinit; compinit" >> ~/.zshrc

To load completions in your current shell session:

	source <(kshrk completion zsh)

To load completions for every new session, execute once:

#### Linux:

	kshrk completion zsh > "${fpath[1]}/_kshrk"

#### macOS:

	kshrk completion zsh > $(brew --prefix)/share/zsh/site-functions/_kshrk

You will need to start a new shell for this setup to take effect.


```
kshrk completion zsh [flags]
```

### Options

```
  -h, --help              help for zsh
      --no-descriptions   disable completion descriptions
```

### Options inherited from parent commands

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk completion](#kshrk-completion)	 - Generate the autocompletion script for the specified shell


## kshrk decrypt

Decrypt an encrypted capture archive back to plaintext

### Synopsis

Writes a plaintext copy of an age-encrypted .kshrk archive. The
original encrypted archive is not modified; the output defaults to
<in>-decrypted.kshrk.

Every other kshrk command (inspect, open, ui, replay, diff, query,
transitions, diagnose, redact) already reads an encrypted archive
transparently given a key — this command is for producing a standalone
plaintext copy, e.g. to hand off to a tool that can't decrypt, or before
re-encrypting to a different key/recipient set.

```
kshrk decrypt <capture.kshrk> [flags]
```

### Examples

```
  # Decrypt using a passphrase read from a file
  kshrk decrypt capture.kshrk --decrypt-passphrase-file ./pass.txt

  # Decrypt using an age identity (private key) file
  kshrk decrypt capture.kshrk --decrypt-identity-file ./key.txt

  # Choose the output path explicitly
  kshrk decrypt capture.kshrk --out plain.kshrk --decrypt-passphrase-file ./pass.txt
```

### Options

```
  -h, --help         help for decrypt
      --out string   output archive path (default: <in>-decrypted.kshrk)
```

### Options inherited from parent commands

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk](#kshrk)	 - k8shark — Kubernetes cluster state capture and replay


## kshrk diagnose

Analyze a capture and report likely problems

### Synopsis

Runs a diagnostic pass over a capture and prints severity-ranked findings
(CrashLoopBackOff, OOMKilled, image-pull failures, unschedulable pods, unbound
PVCs, version skew, …) — without starting a server.

Use -o json/yaml for automation, and --fail-on to exit non-zero for CI gating.

```
kshrk diagnose <capture.kshrk> [flags]
```

### Examples

```
  # Ranked findings as a table
  kshrk diagnose capture.kshrk

  # Only warnings and above, scheduling category, as JSON
  kshrk diagnose capture.kshrk --severity warning --category scheduling -o json

  # Fail the build if anything critical is found
  kshrk diagnose capture.kshrk --fail-on critical
```

### Options

```
      --at string         analyze state at a timestamp (RFC3339 or relative duration like -5m); default latest
      --category string   only report this category (workload, scheduling, storage, node, cluster)
      --fail-on string    exit non-zero if any finding is at least this severity (info, warning, critical)
  -h, --help              help for diagnose
  -o, --output string     output format: table, json, or yaml (default "table")
      --severity string   minimum severity to report: info, warning, or critical (default "info")
```

### Options inherited from parent commands

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk](#kshrk)	 - k8shark — Kubernetes cluster state capture and replay


## kshrk diff

Compare two capture snapshots

### Synopsis

Compares resource state between two capture archives (--before/--after),
or between two points in time within a single archive (--archive with
--from/--to), and prints a diff. Limit the scope with --resource and
--namespace, and choose text or json output with -o. Exits non-zero when
differences are found.

```
kshrk diff [flags]
```

### Examples

```
  # Diff two separate captures
  kshrk diff --before before.kshrk --after after.kshrk

  # Diff two points in time within one capture
  kshrk diff --archive capture.kshrk --from -10m --to -1m

  # Limit to a resource and namespace, as JSON
  kshrk diff --before before.kshrk --after after.kshrk --resource pods --namespace default -o json
```

### Options

```
      --after string       after archive path
      --archive string     single archive path for intra-archive diff
      --before string      before archive path
      --from string        time for the before snapshot, with --archive (RFC3339 or relative duration like -5m)
  -h, --help               help for diff
      --namespace string   limit diff to one namespace
  -o, --output string      output format: text or json (default "text")
      --resource string    limit diff to one resource type, e.g. pods
      --to string          time for the after snapshot, with --archive (RFC3339 or relative duration like -1m)
```

### Options inherited from parent commands

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk](#kshrk)	 - k8shark — Kubernetes cluster state capture and replay


## kshrk encrypt

Encrypt an existing capture archive after the fact

### Synopsis

Writes an age-encrypted copy of an existing plaintext .kshrk archive.
The original archive is not modified; the output defaults to
<in>-encrypted.kshrk.

This is a whole-file transform, not a new capture: use it to encrypt an
archive you already have (e.g. before sharing it), as an alternative to
'kshrk capture --encrypt' at capture time.

```
kshrk encrypt <capture.kshrk> [flags]
```

### Examples

```
  # Encrypt with a passphrase, prompting for it
  kshrk encrypt capture.kshrk --encrypt

  # Encrypt using a passphrase read from a file (no prompt)
  kshrk encrypt capture.kshrk --encrypt-passphrase-file ./pass.txt

  # Encrypt to one or more age recipient public keys
  kshrk encrypt capture.kshrk --encrypt-recipient age1abc...

  # Choose the output path explicitly
  kshrk encrypt capture.kshrk --out shared.kshrk --encrypt-recipient age1abc...
```

### Options

```
      --encrypt                          encrypt the output archive with a passphrase (age); from --encrypt-passphrase-file, $KSHRK_ENCRYPT_PASSPHRASE, or an interactive prompt
      --encrypt-passphrase-file string   read the encryption passphrase from this file (first line) instead of prompting
      --encrypt-recipient stringArray    age recipient public key (age1...) to encrypt to (repeatable); mutually exclusive with passphrase encryption
      --encrypt-recipients-file string   file of age recipient public keys (one per line) to encrypt to
  -h, --help                             help for encrypt
      --out string                       output archive path (default: <in>-encrypted.kshrk)
```

### Options inherited from parent commands

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk](#kshrk)	 - k8shark — Kubernetes cluster state capture and replay


## kshrk inspect

Display a summary of a capture archive's contents

### Synopsis

Reads a k8shark capture archive and prints capture metadata (format
version, capture window, Kubernetes version, record count) and a table of
captured resource types, without starting a server. Use -o json or -o yaml
for machine-readable output.

```
kshrk inspect <capture.kshrk> [flags]
```

### Examples

```
  # Summarize a capture as a table
  kshrk inspect capture.kshrk

  # Machine-readable output
  kshrk inspect capture.kshrk -o json
  kshrk inspect capture.kshrk -o yaml
```

### Options

```
  -h, --help            help for inspect
  -o, --output string   Output format: table, json, or yaml (default "table")
  -w, --wide            Show full namespace list in table output
```

### Options inherited from parent commands

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk](#kshrk)	 - k8shark — Kubernetes cluster state capture and replay


## kshrk open

Open a capture file and start a mock Kubernetes API server

### Synopsis

Reads a k8shark capture archive (in memory, without extracting to disk),
starts a local mock Kubernetes API server, and writes a kubeconfig so kubectl
can connect immediately. Use --at to replay the cluster as it looked at a
specific point in the capture window.

```
kshrk open <capture.kshrk> [flags]
```

### Examples

```
  # Open a capture and start the mock API server
  kshrk open capture.kshrk

  # Replay the state 5 minutes before the capture ended
  kshrk open capture.kshrk --at -5m

  # Replay at a specific timestamp, on a fixed port
  kshrk open capture.kshrk --at 2026-04-09T10:30:00Z --api-port 8443
```

### Options

```
      --api-port string         port for the mock API server (0 = random available port) (default "0")
      --at string               pin replay to a specific timestamp (RFC3339) or relative duration like -5m
  -h, --help                    help for open
      --kubeconfig-out string   where to write the generated kubeconfig (default: ~/.kube/k8shark-<id>.yaml)
```

### Options inherited from parent commands

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk](#kshrk)	 - k8shark — Kubernetes cluster state capture and replay


## kshrk query

Search or run a JSONPath query across every captured object

### Synopsis

Evaluates an expression against every object captured in the archive — across
all resource types and namespaces — at a chosen snapshot, and prints what
matched.

By default the expression is a kubectl-style JSONPath template. With --text
or --regex, it's instead a plain substring or regular expression searched
across every object body and captured pod log (the current container's log,
plus the previous container's log if that was captured too).

Limit the scope with --resource and --namespace, and pin the snapshot in time
with --at.

```
kshrk query <capture.kshrk> <expression> [flags]
```

### Examples

```
  # Every container image in the capture (JSONPath)
  kshrk query capture.kshrk '{.spec.containers[*].image}'

  # Just Deployments' replica counts, as JSON
  kshrk query capture.kshrk '{.spec.replicas}' --resource deployments -o json

  # Where does this error string appear, across objects and pod logs?
  kshrk query capture.kshrk 'connection refused' --text

  # Same, with a regular expression
  kshrk query capture.kshrk 'connection (refused|reset)' --regex
```

### Options

```
      --at string          query state at a timestamp (RFC3339 or relative duration like -5m); default latest
  -h, --help               help for query
      --namespace string   limit the query to one namespace
  -o, --output string      output format: table or json (default "table")
      --regex              treat the expression as a regular expression, searched across object bodies and pod logs instead of JSONPath
      --resource string    limit the query to one resource type, e.g. pods
      --text               treat the expression as a plain substring, searched across object bodies and pod logs instead of JSONPath
```

### Options inherited from parent commands

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk](#kshrk)	 - k8shark — Kubernetes cluster state capture and replay


## kshrk redact

Redact Secret data and arbitrary fields from a capture archive

### Synopsis

Produces a new capture archive with Kubernetes Secret data replaced by
"REDACTED" and any configured field-level redaction rules applied. The original
archive is not modified; the output defaults to <in>-redacted.kshrk.

Field rules can be supplied via --redact-field (repeatable) with the format:
  <fieldPath>:<Kind>:<replacement>[:<valueType>]

Rules may also be loaded from a config file's redaction.rules block via --config.

```
kshrk redact <capture.kshrk> [--out <redacted.kshrk>] [flags]
```

### Examples

```
  # Redact all Secret data and stringData values
  kshrk redact capture.kshrk --redact-secrets

  # Apply a single field-level redaction rule
  kshrk redact capture.kshrk --redact-field "data.api-key:ConfigMap:REDACTED"

  # Use redaction.rules from a config file, writing to a chosen path
  kshrk redact capture.kshrk --out safe.kshrk --config k8shark.yaml

  # Redact and encrypt the output to an age recipient (decrypt an encrypted
  # source with --decrypt-passphrase-file / --decrypt-identity-file)
  kshrk redact capture.kshrk --redact-secrets --encrypt-recipient age1abc...
```

### Options

```
      --allow-secret stringArray         namespace/name of secret to preserve (repeatable)
      --config string                    capture config file whose redaction.rules block is applied
      --encrypt                          encrypt the output archive with a passphrase (age); from --encrypt-passphrase-file, $KSHRK_ENCRYPT_PASSPHRASE, or an interactive prompt
      --encrypt-passphrase-file string   read the encryption passphrase from this file (first line) instead of prompting
      --encrypt-recipient stringArray    age recipient public key (age1...) to encrypt to (repeatable); mutually exclusive with passphrase encryption
      --encrypt-recipients-file string   file of age recipient public keys (one per line) to encrypt to
  -h, --help                             help for redact
      --out string                       output archive path (default: <in>-redacted.kshrk)
      --redact-field stringArray         field redaction rule: <fieldPath>:<Kind>:<replacement>[:<valueType>] (repeatable)
      --redact-secrets                   redact all Kubernetes Secret data and stringData values
```

### Options inherited from parent commands

```
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk](#kshrk)	 - k8shark — Kubernetes cluster state capture and replay


## kshrk replay

Replay a capture forward through time at a chosen speed

### Synopsis

Plays a k8shark capture forward through time, streaming captured watch
events (ADDED/MODIFIED/DELETED) to clients as a replay clock advances. Unlike
'open --at', which jumps the whole view to a single instant, replay advances a
clock and streams change over time — so controllers/operators and 'kubectl get
--watch' observe the cluster changing exactly as it did during capture.

A kubeconfig is written just like 'open'; point kubectl or a controller at it.

```
kshrk replay <capture.kshrk> [flags]
```

### Examples

```
  # Replay the whole capture at twice its original speed
  kshrk replay capture.kshrk --speed 2x

  # Replay in slow motion
  kshrk replay capture.kshrk --speed 0.5x

  # Loop the last 10 minutes of the capture
  kshrk replay capture.kshrk --from -10m --to -1m --loop

  # Start paused (press Enter to begin)
  kshrk replay capture.kshrk --start-paused
```

### Options

```
      --api-port string           port for the mock API server (0 = random available port) (default "0")
      --controller-log string     destination for kube-controller-manager's own output when --with-controller-manager is set: a file path, or "-" to stream it inline (default: a temp file whose path is printed at startup)
      --from string               replay window start: RFC3339 or relative duration like -10m (default: capture start)
  -h, --help                      help for replay
      --kubeconfig-out string     where to write the generated kubeconfig (default: ~/.kube/k8shark-<id>.yaml)
      --loop                      restart the replay from the window start when it reaches the end
      --schedule-pods             bind unscheduled pods to a node on create (the scheduler replay lacks); --writable only (default true)
      --speed string              playback speed factor, e.g. 2x, 3x, 0.5x (default "1x")
      --start-paused              start paused; press Enter to begin playback
      --to string                 replay window end: RFC3339 or relative duration like -1m (default: capture end)
      --ui                        also start the web dashboard as a replay transport (VCR)
      --ui-port string            port for the dashboard when --ui is set (0 = random) (default "0")
      --with-controller-manager   also run kube-controller-manager (downloaded/built to match the capture's Kubernetes version) against the server, reconciling a curated set of controllers (namespace, serviceaccount, resourcequota, daemonset, deployment, replicaset, statefulset, job, cronjob, endpoint, endpointslice, endpointslicemirroring, disruption) — see docs/kwok.md (implies --writable)
      --with-kwok                 also run a detected 'kwok' binary against the server to drive pod/node lifecycle (implies --writable)
      --writable                  accept client writes into an in-memory overlay (closed-loop controller dev)
```

### Options inherited from parent commands

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk](#kshrk)	 - k8shark — Kubernetes cluster state capture and replay


## kshrk transitions

List resource state changes from a capture archive

### Synopsis

Reads a k8shark capture archive and reports ADDED, MODIFIED, and DELETED
events for captured resources, without starting a replay server.

For watch-enabled captures, events are read directly from the watch-event index.
For poll-only captures, consecutive snapshots are diff'd to infer changes.

Narrow the output with --resource / --namespace / --name and the --from/--to
time window, add --diff to show field-level changes for MODIFIED events, and use
-o json for machine-readable output.

```
kshrk transitions <capture.kshrk> [flags]
```

### Examples

```
  # List all state changes in a capture
  kshrk transitions capture.kshrk

  # Only Deployment changes in the "prod" namespace
  kshrk transitions capture.kshrk --resource deployments --namespace prod

  # Show field diffs for MODIFIED events within a time window
  kshrk transitions capture.kshrk --diff --from 2026-04-09T10:00:00Z --to 2026-04-09T10:05:00Z

  # Machine-readable output
  kshrk transitions capture.kshrk -o json
```

### Options

```
      --diff               show field diffs for MODIFIED events
      --from string        start of time window (RFC3339 or relative duration like -10m)
  -h, --help               help for transitions
      --name string        filter by exact object name
      --namespace string   filter by exact namespace
  -o, --output string      output format: table or json (default "table")
      --resource string    filter by resource name fragment (e.g. pods, deployments)
      --to string          end of time window (RFC3339 or relative duration like -1m)
```

### Options inherited from parent commands

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk](#kshrk)	 - k8shark — Kubernetes cluster state capture and replay


## kshrk ui

Open an interactive web explorer for a capture archive

### Synopsis

Starts a local web UI for browsing a k8shark capture — namespaces,
workloads, pods, object YAML/JSON, relationships, and a watch-event timeline —
and also runs the mock Kubernetes API server with generated kubeconfig output.
Ports default to random; pin them with --ui-port / --api-port (or a ui: block
in the config file).

```
kshrk ui <capture.kshrk> [flags]
```

### Examples

```
  # Browse a capture in the web UI
  kshrk ui capture.kshrk

  # Pin the UI and mock API server ports
  kshrk ui capture.kshrk --ui-port 8080 --api-port 8081

  # Open the UI pinned to a point in time
  kshrk ui capture.kshrk --at -5m

  # Watch a closed-loop simulation in the dashboard (see docs/kwok.md)
  kshrk ui capture.kshrk --with-kwok --with-controller-manager
```

### Options

```
      --api-port string           port for the mock API server (0 = random available port) (default "0")
      --at string                 pin UI data to a specific timestamp (RFC3339 or relative duration like -5m)
      --controller-log string     destination for kube-controller-manager's own output when --with-controller-manager is set: a file path, or "-" to stream it inline (default: a temp file whose path is printed at startup)
      --from string               replay window start: RFC3339 or relative duration like -10m
  -h, --help                      help for ui
      --kubeconfig-out string     where to write the generated kubeconfig (default: ~/.kube/k8shark-<id>.yaml)
      --loop                      replay mode: restart from the window start when the end is reached
      --speed string              replay mode: playback speed factor, e.g. 2x, 3x, 0.5x (enables replay)
      --start-paused              replay mode: start paused (the UI defaults to this; pass --start-paused=false to auto-play)
      --to string                 replay window end: RFC3339 or relative duration like -1m
      --ui-port string            port for the local UI server (0 = random available port) (default "0")
      --with-controller-manager   also run kube-controller-manager (downloaded/built to match the capture's Kubernetes version) against the server, reconciling a curated set of controllers (namespace, serviceaccount, resourcequota, daemonset, deployment, replicaset, statefulset, job, cronjob, endpoint, endpointslice, endpointslicemirroring, disruption) — see docs/kwok.md (implies --writable)
      --with-kwok                 replay mode: also run a detected 'kwok' binary against the server to drive pod/node lifecycle (implies --writable)
      --writable                  replay mode: accept client writes into an in-memory overlay
```

### Options inherited from parent commands

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk](#kshrk)	 - k8shark — Kubernetes cluster state capture and replay


## kshrk validate

Validate a capture config file without connecting to a cluster

### Synopsis

Parse and validate a k8shark capture config file, reporting any errors or
warnings without connecting to a cluster or making any API calls. Hard errors
(e.g. a missing resource/version, an unparseable duration) exit non-zero;
warnings (e.g. a very short interval) are printed but exit zero.

```
kshrk validate [flags]
```

### Examples

```
  # Validate a capture config before running it
  kshrk validate --config k8shark.yaml
```

### Options

```
  -h, --help   help for validate
```

### Options inherited from parent commands

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk](#kshrk)	 - k8shark — Kubernetes cluster state capture and replay


## kshrk version

Print the kshrk version

```
kshrk version [flags]
```

### Options

```
  -h, --help   help for version
```

### Options inherited from parent commands

```
      --config string                    config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)
      --decrypt-identity-file string     age identity (key) file to decrypt an encrypted archive
      --decrypt-passphrase-file string   read the passphrase for an encrypted archive from this file (first line)
  -v, --verbose                          enable verbose output
```

### SEE ALSO

* [kshrk](#kshrk)	 - k8shark — Kubernetes cluster state capture and replay


