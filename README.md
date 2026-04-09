# k8shark

> Like Wireshark, but for Kubernetes.

**k8shark** captures a Kubernetes cluster's state over time and packages it into a single portable archive. A built-in mock API server lets support engineers replay that archive exactly like a live cluster — no direct connectivity required.

## How it works

1. **Capture** — `kshrk capture` polls the Kubernetes API at defined intervals for a configured duration, recording every response as JSON. Everything is packaged into a `k8shark-<timestamp>.tar.gz` file.

2. **Open** — `kshrk open capture.tar.gz` extracts the archive, starts a local mock HTTPS API server, and writes a kubeconfig pointing at it. Run `export KUBECONFIG=~/.kube/k8shark-<id>` and use `kubectl` normally.

This lets a customer hand over a single capture file so a support engineer can query the environment interactively, without waiting for back-and-forth command output or needing cluster access.

## Install

```sh
go install github.com/phenixblue/k8shark@latest
```

Or build from source:

```sh
git clone https://github.com/phenixblue/k8shark
cd k8shark
go build -o kshrk .
```

## Usage

### Capture

```sh
# Using a config file
kshrk capture --config k8shark.yaml

# Override output path and duration
kshrk capture --config k8shark.yaml --output my-capture.tar.gz --duration 5m
```

Example `k8shark.yaml`:

```yaml
duration: 10m
output: ./capture.tar.gz

resources:
  - group: ""
    version: v1
    resource: pods
    namespaces: [default, kube-system]
    interval: 30s
  - group: ""
    version: v1
    resource: nodes
    interval: 60s
  - group: apps
    version: v1
    resource: deployments
    namespaces: [default]
    interval: 30s
  - group: ""
    version: v1
    resource: events
    namespaces: [default]
    interval: 10s
```

### Open

```sh
kshrk open capture.tar.gz

# Output:
# k8shark mock server running
#   Address:    https://127.0.0.1:54321
#   Kubeconfig: ~/.kube/k8shark-abc123
#
# Run: export KUBECONFIG=~/.kube/k8shark-abc123
# Then use kubectl normally against the capture.
```

### Flags

| Flag | Description |
|------|-------------|
| `--config` | Path to capture config file (default: `./k8shark.yaml`) |
| `--verbose` / `-v` | Enable verbose output |

#### `capture`

| Flag | Description |
|------|-------------|
| `--output` / `-o` | Output `.tar.gz` path |
| `--kubeconfig` | Path to kubeconfig for the source cluster |
| `--duration` | Override capture duration (e.g. `5m`, `1h`) |

#### `open`

| Flag | Description |
|------|-------------|
| `--port` | Port for mock API server (default: random) |
| `--kubeconfig-out` | Where to write the generated kubeconfig |
| `--at` | Pin replay to a specific timestamp (RFC3339) |

## Capture archive format

```
k8shark-capture/
  metadata.json       # cluster info, k8s version, capture config, timestamps
  records/
    <uuid>.json       # one JSON file per polled API response
  index.json          # maps api_path -> [record IDs sorted by time]
```

## License

Apache 2.0
