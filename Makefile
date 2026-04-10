.PHONY: build test test-race test-cover fmt lint e2e kind-up kind-down clean help

BINARY  := kshrk
VERSION ?= dev
LDFLAGS := -w -s -X github.com/phenixblue/k8shark/cmd.Version=$(VERSION)
GOFLAGS := -trimpath

build: ## Build the kshrk binary
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) .

test: ## Run unit tests
	go test ./...

test-race: ## Run unit tests with the race detector
	go test -race ./...

test-cover: ## Run unit tests and generate an HTML coverage report
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

fmt: ## Format Go source files
	gofmt -w .

lint: ## Run go vet
	go vet ./...

e2e: build ## Build binary and run end-to-end tests (requires kind + kubectl)
	./scripts/e2e.sh

kind-up: ## Create a dev KinD cluster with test resources (use --reset to recreate)
	./scripts/kind-up.sh $(ARGS)

kind-down: ## Delete the dev KinD cluster (k8shark-dev)
	kind delete cluster --name k8shark-dev
	rm -f $(HOME)/.kube/k8shark-dev.yaml

clean: ## Remove build artifacts
	rm -f $(BINARY) coverage.out coverage.html

help: ## Print available targets
	@grep -E '^[a-zA-Z0-9_-]+:.*## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'

.DEFAULT_GOAL := build
