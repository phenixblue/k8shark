.PHONY: build test test-race test-cover bench fmt lint lint-ci-install lint-ci e2e kind-up kind-down release-snapshot release-local clean help

BINARY  := kshrk
VERSION ?= dev
LDFLAGS := -w -s -X github.com/phenixblue/k8shark/cmd.Version=$(VERSION)
GOFLAGS := -trimpath
GOLANGCI_LINT_VERSION ?= v1.64.8

build: ## Build the kshrk binary
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) .

test: ## Run unit tests
	go test ./...

test-race: ## Run unit tests with the race detector
	go test -race ./...

test-cover: ## Run unit tests and generate an HTML coverage report
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

bench: ## Run all benchmarks (use BENCH=<regexp> to filter)
	go test -bench=${BENCH:-.} -benchmem -run=^$$ ./...

fmt: ## Format Go source files
	gofmt -w .

lint: ## Run go vet
	go vet ./...

lint-ci-install: ## Install the pinned golangci-lint version used in CI
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint-ci: ## Run golangci-lint exactly like CI (requires lint-ci-install)
	golangci-lint run

e2e: build ## Build binary and run end-to-end tests (requires kind + kubectl)
	./scripts/e2e.sh

kind-up: ## Create a dev KinD cluster with test resources (use --reset to recreate)
	./scripts/kind-up.sh $(ARGS)

kind-down: ## Delete the dev KinD cluster (k8shark-dev)
	kind delete cluster --name k8shark-dev
	rm -f $(HOME)/.kube/k8shark-dev.yaml

clean: ## Remove build artifacts
	rm -f $(BINARY) coverage.out coverage.html

release-snapshot: ## Build a local release snapshot without publishing (no signing)
	goreleaser release --snapshot --clean

release-local: ## Dry-run release with SBOM + Homebrew output but no publish
	goreleaser release --snapshot --clean --skip=sign,publish

help: ## Print available targets
	@grep -E '^[a-zA-Z0-9_-]+:.*## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'

.DEFAULT_GOAL := build
