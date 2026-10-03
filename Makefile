# kubeswift-spin developer workflow. Run `make help` for the target list.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

ROOT := $(abspath .)
BIN := $(ROOT)/bin
export PATH := $(BIN):$(PATH)

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

REGISTRY ?= ghcr.io/kubeswift-io
IMAGE ?= $(REGISTRY)/kubeswift-spin:$(VERSION)
# The runtime image is versioned independently of the controller (see
# runtime/VERSION and docs/runtime-image.md), so a controller release does
# not change the sandbox spec of running SpinApps.
RUNTIME_VERSION := $(shell cat runtime/VERSION)
RUNTIME_IMAGE ?= $(REGISTRY)/kubeswift-spin-runtime:$(RUNTIME_VERSION)
CONTAINER_TOOL ?= docker

# Pinned tool versions.
GOLANGCI_LINT_VERSION ?= v2.14.0
GOVULNCHECK_VERSION ?= v1.8.0
SETUP_ENVTEST_VERSION ?= v0.25.2
KUBECONFORM_VERSION ?= v0.8.0
ACTIONLINT_VERSION ?= v1.7.12
ENVTEST_K8S_VERSION ?= 1.34.1
KIND_K8S_IMAGE ?= kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a
WITH_SPIN_OPERATOR ?=

GOLANGCI_LINT := $(BIN)/golangci-lint
GOVULNCHECK := $(BIN)/govulncheck
SETUP_ENVTEST := $(BIN)/setup-envtest
KUBECONFORM := $(BIN)/kubeconform
ACTIONLINT := $(BIN)/actionlint
SPIN := $(BIN)/spin

EXAMPLES := hello-http request-info outbound-http key-value serverless-ai experimental/mcp
EXAMPLE ?= hello-http
EXAMPLE_REGISTRY ?= $(REGISTRY)/kubeswift-spin-examples
EXAMPLE_TAG ?= $(VERSION)
NAMESPACE ?= default
# The OCI artifact is named after the Spin application ([application] name).
EXAMPLE_NAME = $(shell awk -F'"' '/^name = /{print $$2; exit}' examples/$(EXAMPLE)/spin.toml)

##@ General

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_.-]+:.*##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Format Go code.
	gofmt -s -w cmd internal examples/tools test

.PHONY: fmt-check
fmt-check: ## Fail if Go code is not formatted.
	@out="$$(gofmt -s -l cmd internal examples/tools test)"; if [[ -n "$$out" ]]; then echo "not gofmt-ed:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run golangci-lint.
	$(GOLANGCI_LINT) run ./...

.PHONY: check-prose
check-prose: ## Reject em dashes and section signs in project-authored text.
	hack/check-prose.sh

.PHONY: lint-workflows
lint-workflows: $(ACTIONLINT) ## Lint GitHub Actions workflows.
	$(ACTIONLINT) .github/workflows/*.yaml

.PHONY: vulncheck
vulncheck: $(GOVULNCHECK) ## Scan Go dependencies for known vulnerabilities.
	$(GOVULNCHECK) ./...

##@ Test

.PHONY: test
test: $(SETUP_ENVTEST) ## Run unit tests and the envtest controller suite.
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(BIN)/envtest -p path)" \
	  go test -race -count=1 -coverprofile=cover.out ./...

.PHONY: test-unit
test-unit: ## Run unit tests only (envtest suites skip themselves).
	go test -count=1 ./...

.PHONY: runtime-test
runtime-test: runtime-image spin ## Test the runtime image with Docker (no Kubernetes, no KVM).
	hack/test-runtime-image.sh $(RUNTIME_IMAGE)

.PHONY: kind-test
kind-test: image ## Controller integration test on kind (API reconciliation only, no KVM).
	IMAGE=$(IMAGE) KIND_K8S_IMAGE=$(KIND_K8S_IMAGE) WITH_SPIN_OPERATOR=$(WITH_SPIN_OPERATOR) test/integration/kind-test.sh

.PHONY: e2e
e2e: ## KVM end-to-end test against the current kubeconfig context. See test/e2e/README.md.
	test/e2e/kvm-e2e.sh

##@ Build

.PHONY: build
build: ## Build the controller and entrypoint binaries into bin/.
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w \
	  -X github.com/kubeswift-io/kubeswift-spin/internal/version.Version=$(VERSION) \
	  -X github.com/kubeswift-io/kubeswift-spin/internal/version.Commit=$(COMMIT)" \
	  -o $(BIN)/manager ./cmd/manager
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $(BIN)/kubeswift-spin-entrypoint ./cmd/spin-entrypoint

.PHONY: image
image: ## Build the controller image ($(IMAGE)).
	$(CONTAINER_TOOL) build -t $(IMAGE) --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
	  --build-arg DEFAULT_RUNTIME_IMAGE=$(RUNTIME_IMAGE) .

.PHONY: runtime-image
runtime-image: ## Build the runtime rootfs image ($(RUNTIME_IMAGE)).
	$(CONTAINER_TOOL) build -f runtime/Dockerfile -t $(RUNTIME_IMAGE) .

##@ Helm

.PHONY: helm-lint
helm-lint: $(KUBECONFORM) ## Lint the chart and validate rendered manifests.
	helm lint charts/kubeswift-spin
	helm lint charts/kubeswift-spin --set metrics.serviceMonitor.enabled=true --set metrics.secure=true
	helm template kubeswift-spin charts/kubeswift-spin --namespace kubeswift-spin-system \
	  | $(KUBECONFORM) -strict -summary -skip SpinAppExecutor
	hack/check-chart.sh

##@ Examples

.PHONY: spin
spin: ## Install the pinned Spin CLI into bin/.
	hack/install-spin.sh $(BIN)

.PHONY: examples
examples: spin ## Build every example Spin application.
	@for e in $(EXAMPLES); do echo "==> $$e"; $(SPIN) build -f examples/$$e/spin.toml; done

.PHONY: example-test
example-test: spin ## Run every example locally under spin up and check its behavior.
	hack/test-examples.sh

.PHONY: example-build
example-build: spin ## Build one example: make example-build EXAMPLE=hello-http
	$(SPIN) build -f examples/$(EXAMPLE)/spin.toml

.PHONY: example-push
example-push: example-build ## Push one example as a Spin OCI artifact: make example-push EXAMPLE=hello-http EXAMPLE_REGISTRY=ghcr.io/you EXAMPLE_TAG=v0.1.0
	cd examples/$(EXAMPLE) && $(SPIN) registry push $(EXAMPLE_REGISTRY)/$(EXAMPLE_NAME):$(EXAMPLE_TAG)

.PHONY: example-deploy
example-deploy: ## Apply one example SpinApp: make example-deploy EXAMPLE=hello-http EXAMPLE_REGISTRY=ghcr.io/you EXAMPLE_TAG=v0.1.0
	sed -e 's|^  image: .*|  image: $(EXAMPLE_REGISTRY)/$(EXAMPLE_NAME):$(EXAMPLE_TAG)|' examples/$(EXAMPLE)/spinapp.yaml \
	  | kubectl apply -n $(NAMESPACE) -f -

##@ Quality gate

.PHONY: verify
verify: fmt-check vet lint lint-workflows check-prose test helm-lint ## Pre-commit gate: formatting, vet, lint, prose, tests, chart.

.PHONY: verify-all
verify-all: verify vulncheck example-test runtime-test ## verify plus vulnerability scan, example tests and runtime image tests.

##@ Tools

$(GOLANGCI_LINT):
	GOBIN=$(BIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(GOVULNCHECK):
	GOBIN=$(BIN) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

$(SETUP_ENVTEST):
	GOBIN=$(BIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

$(ACTIONLINT):
	GOBIN=$(BIN) go install github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

$(KUBECONFORM):
	GOBIN=$(BIN) go install github.com/yannh/kubeconform/cmd/kubeconform@$(KUBECONFORM_VERSION)

.PHONY: clean
clean: ## Remove build outputs.
	rm -rf $(BIN)/manager $(BIN)/kubeswift-spin-entrypoint cover.out dist examples/target
