# k8shark

> Like Wireshark, but for Kubernetes.

**k8shark** captures a Kubernetes cluster's state over time and packages it into a single portable archive. A built-in mock API server lets support engineers replay that archive exactly like a live cluster — no direct connectivity required.

## How it works

1. **Capture** — `kshrk capture` polls the Kubernetes API at configured intervals for a set duration and packages all responses into a `.tar.gz` file.
2. **Open** — `kshrk open capture.tar.gz` extracts the archive, starts a local mock HTTPS API server, and writes a kubeconfig. Set `KUBECONFIG` and use `kubectl` normally.

A customer hands over one file. A support engineer queries the environment interactively, without live cluster access or back-and-forth command output.

## Quick start

```sh
# Install
brew install phenixblue/tap/k8shark

# Capture cluster state for 10 minutes
kshrk capture --config k8shark.yaml

# Replay the capture
kshrk open capture.tar.gz
export KUBECONFIG=~/.kube/k8shark-<id>.yaml
kubectl get pods -A
```

## Documentation

| Doc | Description |
|-----|-------------|
| [docs/usage.md](docs/usage.md) | Installation, capture and open workflows, all CLI flags, kubectl compatibility |
| [docs/config.md](docs/config.md) | Config file reference, namespaced vs cluster-scoped resources, example configs |
| [docs/releases.md](docs/releases.md) | How to cut a release, GoReleaser pipeline, signing, Homebrew tap |
| [docs/development.md](docs/development.md) | Building, testing, linting, KinD dev cluster, E2E tests, package layout |
| [docs/archive-format.md](docs/archive-format.md) | Internal `.tar.gz` layout, record and index JSON schemas |

## License

Apache 2.0
