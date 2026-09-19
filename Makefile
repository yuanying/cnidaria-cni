# cnidaria-cni
#
# Tests are split by the privileges they need. `make test` passes with nothing but the
# Go toolchain. The netns integration tests, which need root / CAP_NET_ADMIN plus nft
# and ip, sit behind the build tag `netns` so that `go test ./...` does not pick them
# up (ADR 0008).
#
# Go-based development tools (golangci-lint, controller-gen) are declared in go.mod's
# tool directive and run through `go tool`, so nothing is installed globally.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

GO ?= go
BIN ?= bin/cnidaria

# Container image for the daemon. `make image` builds both architectures with buildx;
# set PUSH=1 to push the manifest list instead of leaving it in the builder cache.
IMAGE ?= ghcr.io/yuanying/cnidaria-cni:dev
PLATFORMS ?= linux/amd64,linux/arm64
PUSH ?=

# `kustomize` is not required: kubectl bundles the same build command.
KUSTOMIZE ?= kubectl kustomize

##@ General

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"} \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } \
		/^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

##@ Build

.PHONY: build
build: ## Build the node daemon into bin/.
	CGO_ENABLED=0 $(GO) build -o $(BIN) ./cmd/cnidaria

.PHONY: generate
generate: ## Regenerate DeepCopy methods and the CRD manifest from internal/apis.
	$(GO) tool controller-gen object paths=./internal/apis/...
	$(GO) tool controller-gen crd paths=./internal/apis/... output:crd:dir=deploy/crd

.PHONY: image
image: ## Build the multi-architecture container image with docker buildx.
	docker buildx build --platform $(PLATFORMS) -t $(IMAGE) $(if $(PUSH),--push,) .

.PHONY: kustomize
kustomize: ## Render the deployment manifests from deploy/.
	$(KUSTOMIZE) deploy

##@ Check

.PHONY: fmt
fmt: ## Format the source tree.
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail if any file is not gofmt'd.
	@unformatted="$$(gofmt -l . | grep -v '^bin/' || true)"; \
	if [ -n "$$unformatted" ]; then \
		echo "Not gofmt'd. Run \`make fmt\`:"; \
		echo "$$unformatted" | sed 's/^/  /'; \
		exit 1; \
	fi

.PHONY: vet
vet: ## Run go vet over every package, including the tagged tests.
	$(GO) vet ./...
	$(GO) vet -tags netns ./...

.PHONY: lint
lint: ## Run golangci-lint through go tool.
	$(GO) tool golangci-lint run ./...
	$(GO) tool golangci-lint run --build-tags netns ./test/...

##@ Test

.PHONY: test
test: ## Unit tests. Needs neither privileges nor external commands.
	$(GO) test ./...

##@ Netns

# The netns integration tests build "nodes" out of network namespaces and run against
# them. They need root plus nft and ip. So that the same tests run on a machine without
# nft, the usual entry point is test-netns-docker, which calls them through a privileged
# container that carries the tools (ADR 0008).

NETNS_IMAGE ?= cnidaria-netns:latest

# Asking for this target is a statement that the tools are expected to be there, so a
# missing prerequisite fails rather than skipping. go test buffers the skip reason
# away and prints ok, which reads as a pass on a run that never happened. Pass an
# empty value to get the skip back: make test-netns CNIDARIA_NETNS_REQUIRE=
CNIDARIA_NETNS_REQUIRE ?= 1

.PHONY: test-netns
test-netns: ## The netns integration tests. Needs root / CAP_NET_ADMIN, nft and ip.
	CNIDARIA_NETNS_REQUIRE=$(CNIDARIA_NETNS_REQUIRE) $(GO) test -tags netns -count=1 -timeout 20m ./test/netns/...

.PHONY: netns-image
netns-image: ## Build the container image for the netns integration tests.
	docker build -t $(NETNS_IMAGE) hack/netns

.PHONY: test-netns-docker
test-netns-docker: netns-image ## Bring up a privileged container and run the netns integration tests.
	hack/netns/run-in-docker.sh make test-netns

.PHONY: netns-shell
netns-shell: netns-image ## Open a shell in the netns test container.
	hack/netns/run-in-docker.sh bash
