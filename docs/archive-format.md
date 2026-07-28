# Archive Format

A k8shark capture is a `.kshrk` file: a ZIP container whose entries are
individually Zstandard-compressed JSON (except `metadata.json`, which is stored
uncompressed for fast header reads). It can be listed with any ZIP tool —
unless it was written with any of the `--encrypt`, `--encrypt-passphrase-file`,
`--encrypt-recipient`, or `--encrypt-recipients-file` flags (available on both
`kshrk capture` and `kshrk redact`), in which case the whole file is a single
[age](https://age-encryption.org/v1) envelope around this same ZIP layout; see
[Encryption](#encryption) below.

```sh
unzip -l capture.kshrk
```

## Layout

```
k8shark-capture/
  metadata.json              # uncompressed
  index.json.zst             # zstd-compressed
  watch-index.json.zst       # zstd-compressed; only when watch: true was used
  records/
    <pathDir>/               # <pathDir> = first 16 hex chars of SHA-256(apiPath)
      0.json.zst             # one file per record, named by sequence number
      1.json.zst
      ...
```

Each record lives under a directory derived from a hash of its API path, and is
named by its 0-based sequence number within that path (`<seq>.json.zst`). The
`index.json.zst` maps each API path to its ordered sequence numbers.

## metadata.json

Capture-level information. Written once at the end of the capture run.

```json
{
  "format_version":      2,
  "capture_id":          "550e8400-e29b-41d4-a716-446655440000",
  "captured_at":         "2026-04-09T10:00:00Z",
  "captured_until":      "2026-04-09T10:10:00Z",
  "kubernetes_version":  "v1.30.2",
  "server_address":      "https://192.168.1.100:6443",
  "record_count":        480
}
```

| Field | Description |
|-------|-------------|
| `format_version` | Archive schema version (see [below](#format-version--compatibility)). Absent in pre-versioning archives, which are read as version 1. |
| `capture_id` | UUID, unique per capture run. Used in the generated kubeconfig filename. |
| `captured_at` | UTC timestamp recorded immediately before the first poll/watch goroutines launch (after preflight and discovery). |
| `captured_until` | UTC timestamp when the capture ended. |
| `kubernetes_version` | `gitVersion` from `/version` on the source cluster. |
| `server_address` | API server URL from the kubeconfig used during capture. |
| `record_count` | Total number of individual records written. |

## Format version & compatibility

`metadata.json` carries an integer `format_version` identifying the archive
schema. The current version is **2**. This is the stability promise for the
`.kshrk` format.

### What is guaranteed (the contract)

The **stable surface** is the logical structure, not the bytes:

- The **entry layout** — the `k8shark-capture/` tree and the names/roles of
  `metadata.json`, `index.json.zst`, `watch-index.json.zst`, and
  `records/<pathDir>/<seq>.json.zst`.
- The **JSON schemas** of the metadata, index, watch-index, and record objects
  documented on this page.
- The **`pathDir` derivation** (first 16 hex chars of SHA-256 of the API path)
  and the per-path 0-based `seq` numbering.

### What is NOT part of the contract

These are implementation details and may change without a version bump, because
the reader does not depend on them:

- The **ZIP compression method** of each entry (Store vs Deflate). The payload
  entries are already Zstd-compressed, so the writer typically just stores them
  in the ZIP — but Deflate-stored archives are equally valid and still read.
- Entry **ordering** within the ZIP and per-entry **timestamps**.
- Exact byte size / Zstd encoder level.

Read `.kshrk` files via a ZIP reader plus the documented schemas, Zstd-decoding
only the `*.json.zst` entries (`metadata.json` is plain JSON) — never by
assuming a fixed byte layout.

### Evolution rules

- **Additive changes don't bump the version.** New optional metadata fields
  (`omitempty`) and new optional archive entries (e.g. the watch index) are
  backward-compatible. Consumers **must ignore unknown fields and entries** so
  they keep working against newer archives.
- **Breaking changes bump `format_version`** — any structural change an older
  reader could not safely parse.

### Reader compatibility promise

- A given `kshrk` **MAJOR** release reads **every** archive whose
  `format_version` is ≤ the version that build understands. The `1.x` line reads
  all version-1 archives for the life of the `1.x` series.
- **Pre-versioning archives** (captured before the field existed) omit
  `format_version`; they are treated as version 1, since they are structurally
  identical.
- **Newer archives are refused, not mis-read.** If an archive's `format_version`
  is greater than the build understands, `kshrk` stops with a clear "upgrade
  kshrk" error. Run `kshrk inspect <archive>` to see an archive's format
  version.

### Changing the format (for contributors)

A breaking format change must, in one change: bump `CurrentFormatVersion`
(`internal/archive/format`), update this section, and extend the
golden-fixture tests (`internal/archive`) so the new build still reads older
fixtures — freeze a new `golden-vN` fixture pair (plaintext + passphrase) for
the new format alongside the existing ones for older versions, never replace
them. Additive changes need only an `omitempty` field / optional entry and a
note here.

## index.json.zst

Maps canonical API paths to the ordered sequence numbers captured for each path. The mock server uses this to find records without scanning all files. The entry is Zstd-compressed JSON.

Since format version 2, the map is wrapped under a top-level `entries` key so
the index can gain sibling fields later (e.g. a schema marker, a count) without
another format-version bump:

```json
{
  "entries": {
    "/api/v1/namespaces/default/pods": {
      "api_path": "/api/v1/namespaces/default/pods",
      "seqs":     [0, 1, 2],
      "times":    ["2026-04-09T10:00:00Z", "2026-04-09T10:00:30Z", "2026-04-09T10:01:00Z"],
      "counts":   [4, 4, 5]
    },
    "/api/v1/namespaces/default/pods?as=Table": {
      "api_path": "/api/v1/namespaces/default/pods?as=Table",
      "seqs":     [0, 1],
      "times":    ["2026-04-09T10:00:00Z", "2026-04-09T10:00:30Z"]
    }
  }
}
```

`seqs`, `times`, and `counts` are parallel arrays, ordered by capture time ascending. `seqs[i]` is the sequence number used in the record filename (`records/<pathDir>/<seq>.json.zst`). `counts` is optional — it records the number of top-level items in each list response and is omitted in older archives.

A version-1 archive's `index.json.zst` (and `watch-index.json.zst`) is a
**bare** top-level map — the `entries` wrapper without the `entries` key
itself, i.e. this same object with `entries`'s value promoted to the top
level. Readers in the `1.x` line accept both shapes for the life of the
series; writers always emit the wrapped shape.

### Table response keys

For each resource path, k8shark also captures the Kubernetes Table-format response (the data `kubectl get -o wide` uses). These are stored under the same path with a `?as=Table` suffix. This suffix is a convention internal to k8shark — it does not appear in real API URLs.

In addition, every capture records a **columns-only** Table schema for each list-capable native (built-in) kind whose cluster-scoped list path isn't already captured as a full `?as=Table` (i.e. kinds that are untargeted, or targeted only in specific namespaces). It is stored at the kind's cluster-scoped list path with a `?as=TableSchema` suffix. These records contain the server's `columnDefinitions` with the `rows` array stripped — no object data — so the replay server can render kubectl-accurate columns (and `-o wide`) for objects it has no full Table for (e.g. writable-overlay creations or untargeted kinds) without persisting data for kinds the user didn't capture. Custom resources are excluded (their columns come from the CRD's `additionalPrinterColumns`). Both `?as=Table` and `?as=TableSchema` are k8shark-internal conventions and never appear in real API URLs.

## watch-index.json.zst

Only present in archives captured with `watch: true`. Maps each watched API path to its ordered watch events; each event is a separate record with `event_type` `ADDED`, `MODIFIED`, or `DELETED`. Wrapped under `entries` the same way as `index.json.zst` above, and for the same reason.

```json
{
  "entries": {
    "/api/v1/namespaces/default/pods": {
      "api_path":    "/api/v1/namespaces/default/pods",
      "seqs":        [0, 1],
      "times":       ["2026-04-09T10:00:05Z", "2026-04-09T10:02:11Z"],
      "event_types": ["ADDED", "MODIFIED"]
    }
  }
}
```

`seqs`, `times`, and `event_types` are parallel arrays, ordered by capture time ascending, mirroring `index.json.zst`'s convention.

## records/\<pathDir\>/\<seq\>.json.zst

One Zstd-compressed file per polled API response, named by its sequence number.

```json
{
  "id":            "550e8400-e29b-41d4-a716-446655440000",
  "captured_at":   "2026-04-09T10:00:30Z",
  "api_path":      "/api/v1/namespaces/default/pods",
  "http_method":   "GET",
  "response_code": 200,
  "response_body": { "apiVersion": "v1", "kind": "PodList", "items": [...] }
}
```

| Field | Description |
|-------|-------------|
| `id` | UUID identifying this record (the on-disk filename is the sequence number, not this id). |
| `captured_at` | When this specific poll was recorded. |
| `api_path` | The canonical path key (includes `?as=Table` suffix for Table records). |
| `http_method` | Always `GET`. |
| `response_code` | HTTP status code from the source cluster (`200`, `403`, etc.). |
| `response_body` | Raw JSON response body from the Kubernetes API. |

## Discovery endpoints

In addition to resource paths, k8shark captures API discovery and OpenAPI endpoints so the mock server returns accurate data for tools that inspect the cluster's API surface:

| Path | Description |
|------|-------------|
| `/api` | Core API versions |
| `/api/v1` | Core API resource list |
| `/apis` | All API group list |
| `/apis/<group>/<version>` | Per-group resource list (one per group-version) |
| `/openapi/v2` | OpenAPI v2 spec (for `kubectl explain`) |
| `/openapi/v3` | OpenAPI v3 path index |
| `/openapi/v3/...` | Per-group OpenAPI v3 specs |

## Reading a capture manually

`kshrk inspect <archive>` is the easiest way to summarize a capture. To poke at
the raw entries, use a ZIP tool plus a Zstd decompressor (`metadata.json` is the
only uncompressed entry):

```sh
# List entries
unzip -l capture.kshrk

# Read the (uncompressed) metadata
unzip -p capture.kshrk k8shark-capture/metadata.json | python3 -m json.tool

# Read the (zstd-compressed) index and find the latest seq for a path
unzip -p capture.kshrk k8shark-capture/index.json.zst | zstd -d \
  | python3 -c "
import json,sys
idx=json.load(sys.stdin)
entry=idx['/api/v1/namespaces/default/pods']
print('latest seq:', entry['seqs'][-1])
"
# then read that record (replace <pathDir> and <seq>):
# unzip -p capture.kshrk k8shark-capture/records/<pathDir>/<seq>.json.zst | zstd -d | python3 -m json.tool
```

## Redacted archives

`kshrk redact` produces a structurally identical archive where every Kubernetes
Secret record has its `data` and `stringData` fields scrubbed:

- `data` values are replaced with `UkVEQUNURUQ=` (base64 of `"REDACTED"`)
- `stringData` values are replaced with the string `"REDACTED"`
- All other Secret fields (name, namespace, labels, annotations, type) are unchanged
- Non-Secret records are written verbatim

The index is written unchanged; `metadata.json` records a `redacted` flag plus
the counts `secrets_redacted` and `fields_redacted`. A redacted archive is fully
usable with `kshrk open` — `kubectl get secret` will show the secret names and
types, but all values will be `REDACTED`.

```sh
kshrk redact --in capture.kshrk --out capture-redacted.kshrk
```

## Encryption

`kshrk capture`'s encryption flags (`--encrypt`, `--encrypt-passphrase-file`,
`--encrypt-recipient`, `--encrypt-recipients-file` — and the equivalent flags
on `kshrk redact`) write a `.kshrk` file as a single
[age](https://age-encryption.org/v1) envelope wrapping the entire ZIP
container described above — not per-entry encryption. On write, the ZIP
writer streams into `age.Encrypt`'s writer instead of directly into the
output file. On read, `age.DecryptReaderAt` gives back a seekable
`io.ReaderAt` over the decrypted plaintext, which `zip.NewReader` reads from
directly — so an encrypted archive gets the same random-access reads as a
plaintext one (used by `open`/`ui`), with nothing ever decrypted to a
plaintext temp file or fully buffered in memory. `DecryptReaderAt`'s `ReadAt`
is safe for concurrent use and internally caches only the most-recently
decrypted chunk.

**Detection, not a metadata field.** An encrypted archive is recognized by
sniffing the file's first bytes for the literal, public age spec header
(`age-encryption.org/v1\n`) — not by a flag inside `metadata.json`, since
metadata.json is itself inside the ciphertext and unreadable without a key.
This also means `format_version` is untouched by encryption: it's a
transparent outer envelope, and `CheckFormatVersion` keeps governing only the
*decrypted* payload structure exactly as documented above. A pre-encryption
`kshrk` build has no way to recognize an age-wrapped file and will fail with a
raw, unhelpful zip-parsing error rather than a clear message — there's no way
to retrofit that for binaries already in the wild.

**What's hidden vs. what leaks.** Because the whole ZIP is inside one
ciphertext, entry names (the SHA-256-derived `pathDir` directories), the ZIP
central directory, and per-entry sizes/counts are all hidden — not just
record payloads. The residual leak is the ciphertext's approximate total size
(rounded to the age STREAM chunk boundary) and the fact that it's an
age-encrypted file at all (the header is a public part of the spec, not a
secret). See [encryption-threat-model.md](encryption-threat-model.md) for the
full threat model, key-handling rules, and what's explicitly out of scope.

Key modes: a passphrase (age `ScryptRecipient`/`ScryptIdentity`) or one or
more X25519 recipient public keys — never both for the same archive (age
requires a passphrase recipient to be the file's only recipient).
Passphrase/identity material is read from files, `$KSHRK_ENCRYPT_PASSPHRASE`
/ `$KSHRK_DECRYPT_PASSPHRASE`, or an interactive prompt — never through Viper
or the YAML config file.

Since an encrypted archive can't be listed with a plain ZIP tool (unlike the
plaintext archives described above), use `kshrk inspect` with the appropriate
`--decrypt-*` flag to summarize one, or age-decrypt it first
(`age --decrypt -i key.txt capture.kshrk > capture-plain.kshrk`) to poke at it
with the manual ZIP/Zstd commands shown below.

## Streaming mode (NDJSON stdout)

When `output: "-"` is set in the configuration (or `--output -` on the command line), k8shark writes records to stdout in **newline-delimited JSON (NDJSON)** format instead of writing a `.kshrk` file. Each line is a complete JSON record object identical to the individual record files described above.

```sh
kshrk capture --config capture.yaml --output - | jq 'select(.api_path == "/api/v1/namespaces/default/pods")'
```

No `metadata.json` or `index.json` is written in streaming mode — only the raw record stream. Pipe to a file or processing tool:

```sh
kshrk capture --config capture.yaml --output - > records.ndjson
```

In streaming mode, SIGTERM or SIGINT causes the engine to stop polling and flush all in-flight records before exiting. Every line in the stream is a complete JSON object.
