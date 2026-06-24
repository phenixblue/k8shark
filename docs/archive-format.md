# Archive Format

A k8shark capture is a `.khsrk` file: a ZIP container whose entries are
individually Zstandard-compressed JSON (except `metadata.json`, which is stored
uncompressed for fast header reads). It can be listed with any ZIP tool.

```sh
unzip -l capture.khsrk
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
  "format_version":      1,
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
| `captured_at` | UTC timestamp when the first poll fired (approximately `now - duration`). |
| `captured_until` | UTC timestamp when the capture ended. |
| `kubernetes_version` | `gitVersion` from `/version` on the source cluster. |
| `server_address` | API server URL from the kubeconfig used during capture. |
| `record_count` | Total number of individual records written. |

## Format version & compatibility

`metadata.json` carries an integer `format_version` identifying the archive
schema. The current version is **1**.

- **Additive changes don't bump it.** New optional metadata fields
  (`omitempty`) and new optional archive entries (e.g. the watch index) are
  backward-compatible: older readers ignore what they don't recognize and
  newer readers tolerate their absence. These keep `format_version` at 1.
- **Breaking changes bump it.** A structural change that an older reader cannot
  safely parse increments the version.
- **Pre-versioning archives** (captured before the field existed) omit
  `format_version`; readers treat them as version 1, since they are
  structurally identical.
- **Newer archives are refused.** If an archive's `format_version` is greater
  than the version a given `kshrk` build understands, the tool stops with a
  clear "upgrade kshrk" error rather than risk misreading an incompatible
  layout. Run `kshrk inspect <archive>` to see an archive's format version.

## index.json.zst

Maps canonical API paths to the ordered sequence numbers captured for each path. The mock server uses this to find records without scanning all files. The entry is Zstd-compressed JSON.

```json
{
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
```

`seqs`, `times`, and `counts` are parallel arrays, ordered by capture time ascending. `seqs[i]` is the sequence number used in the record filename (`records/<pathDir>/<seq>.json.zst`). `counts` is optional — it records the number of top-level items in each list response and is omitted in older archives.

### Table response keys

For each resource path, k8shark also captures the Kubernetes Table-format response (the data `kubectl get -o wide` uses). These are stored under the same path with a `?as=Table` suffix. This suffix is a convention internal to k8shark — it does not appear in real API URLs.

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

`kshrk inspect <archive>` is the easiest way to summarise a capture. To poke at
the raw entries, use a ZIP tool plus a Zstd decompressor (`metadata.json` is the
only uncompressed entry):

```sh
# List entries
unzip -l capture.khsrk

# Read the (uncompressed) metadata
unzip -p capture.khsrk k8shark-capture/metadata.json | python3 -m json.tool

# Read the (zstd-compressed) index and find the latest seq for a path
unzip -p capture.khsrk k8shark-capture/index.json.zst | zstd -d \
  | python3 -c "
import json,sys
idx=json.load(sys.stdin)
entry=idx['/api/v1/namespaces/default/pods']
print('latest seq:', entry['seqs'][-1])
"
# then read that record (replace <pathDir> and <seq>):
# unzip -p capture.khsrk k8shark-capture/records/<pathDir>/<seq>.json.zst | zstd -d | python3 -m json.tool
```

## Redacted archives

`kshrk redact` produces a structurally identical archive where every Kubernetes
Secret record has its `data` and `stringData` fields scrubbed:

- `data` values are replaced with `UkVEQUNURUQ=` (base64 of `"REDACTED"`)
- `stringData` values are replaced with the string `"REDACTED"`
- All other Secret fields (name, namespace, labels, annotations, type) are unchanged
- Non-Secret records are written verbatim

The index is written unchanged; `metadata.json` records the redaction counts
(`redacted`, `secrets_redacted`, `fields_redacted`). A redacted archive is fully
usable with `kshrk open` — `kubectl get secret` will show the secret names and
types, but all values will be `REDACTED`.

```sh
kshrk redact --in capture.khsrk --out capture-redacted.khsrk
```

## Streaming mode (NDJSON stdout)

When `output: "-"` is set in the configuration (or `--output -` on the command line), k8shark writes records to stdout in **newline-delimited JSON (NDJSON)** format instead of writing a `.khsrk` file. Each line is a complete JSON record object identical to the individual record files described above.

```sh
kshrk capture --config capture.yaml --output - | jq 'select(.api_path == "/api/v1/namespaces/default/pods")'
```

No `metadata.json` or `index.json` is written in streaming mode — only the raw record stream. Pipe to a file or processing tool:

```sh
kshrk capture --config capture.yaml --output - > records.ndjson
```

In streaming mode, SIGTERM or SIGINT causes the engine to stop polling and flush all in-flight records before exiting. Every line in the stream is a complete JSON object.
