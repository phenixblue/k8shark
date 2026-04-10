# Archive Format

A k8shark capture is a standard `.tar.gz` file. It can be inspected with any tar tool.

```sh
tar -tzf capture.tar.gz
```

## Layout

```
k8shark-capture/
  metadata.json
  index.json
  records/
    <uuid>.json
    <uuid>.json
    ...
```

## metadata.json

Capture-level information. Written once at the end of the capture run.

```json
{
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
| `capture_id` | UUID, unique per capture run. Used in the generated kubeconfig filename. |
| `captured_at` | UTC timestamp when the first poll fired (approximately `now - duration`). |
| `captured_until` | UTC timestamp when the capture ended. |
| `kubernetes_version` | `gitVersion` from `/version` on the source cluster. |
| `server_address` | API server URL from the kubeconfig used during capture. |
| `record_count` | Total number of individual records written. |

## index.json

Maps canonical API paths to the ordered list of record IDs captured for each path. The mock server uses this to find records without scanning all files.

```json
{
  "/api/v1/namespaces/default/pods": {
    "api_path": "/api/v1/namespaces/default/pods",
    "record_ids": ["uuid-1", "uuid-2", "uuid-3"],
    "times":      ["2026-04-09T10:00:00Z", "2026-04-09T10:00:30Z", "2026-04-09T10:01:00Z"]
  },
  "/api/v1/namespaces/default/pods?as=Table": {
    "api_path": "/api/v1/namespaces/default/pods?as=Table",
    "record_ids": ["uuid-4", "uuid-5"],
    "times":      ["2026-04-09T10:00:00Z", "2026-04-09T10:00:30Z"]
  }
}
```

`record_ids` and `times` are parallel arrays, both ordered by capture time ascending.

### Table response keys

For each resource path, k8shark also captures the Kubernetes Table-format response (the data `kubectl get -o wide` uses). These are stored under the same path with a `?as=Table` suffix. This suffix is a convention internal to k8shark — it does not appear in real API URLs.

## records/\<uuid\>.json

One file per polled API response.

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
| `id` | UUID, matches the filename. |
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

```sh
# List all captured API paths
python3 -c "
import json, sys
idx = json.load(open('k8shark-capture/index.json'))
for path in sorted(idx):
    print(len(idx[path]['record_ids']), path)
" 

# Extract the most recent pod list from default namespace
tar -xOf capture.tar.gz k8shark-capture/index.json \
  | python3 -c "
import json,sys
idx=json.load(sys.stdin)
entry=idx['/api/v1/namespaces/default/pods']
print(entry['record_ids'][-1])
"
# then:
# tar -xOf capture.tar.gz k8shark-capture/records/<uuid>.json | python3 -m json.tool
```

## Redacted archives

`kshrk redact` produces a structurally identical archive where every Kubernetes
Secret record has its `data` and `stringData` fields scrubbed:

- `data` values are replaced with `UkVEQUNURUQ=` (base64 of `"REDACTED"`)
- `stringData` values are replaced with the string `"REDACTED"`
- All other Secret fields (name, namespace, labels, annotations, type) are unchanged
- Non-Secret records are written verbatim

The `index.json` and `metadata.json` are written unchanged. A redacted archive is
fully usable with `kshrk open` — `kubectl get secret` will show the secret names
and types, but all values will be `REDACTED`.

```sh
kshrk redact --in capture.tar.gz --out capture-redacted.tar.gz
```

## Streaming mode (NDJSON stdout)

When `output: "-"` is set in the configuration (or `--output -` on the command line), k8shark writes records to stdout in **newline-delimited JSON (NDJSON)** format instead of writing a `.tar.gz` file. Each line is a complete JSON record object identical to the individual record files described above.

```sh
kshrk capture --config capture.yaml --output - | jq 'select(.api_path == "/api/v1/namespaces/default/pods")'
```

No `metadata.json` or `index.json` is written in streaming mode — only the raw record stream. Pipe to a file or processing tool:

```sh
kshrk capture --config capture.yaml --output - > records.ndjson
```

In streaming mode, SIGTERM or SIGINT causes the engine to stop polling and flush all in-flight records before exiting. Every line in the stream is a complete JSON object.
