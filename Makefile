# sr-cache developer commands. Run `make help` for a summary.

GO        ?= go
GOFLAGS   ?= -trimpath
LDFLAGS   ?= -s -w
BIN       := bin/sr-proxy
IMAGE     ?= ghcr.io/akrishnandg/sr-cache
TAG       ?= dev

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the binary into ./bin/sr-proxy
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(BIN) .

.PHONY: test
test: ## Run all tests
	$(GO) test ./...

.PHONY: test-race
test-race: ## Run tests with -race
	$(GO) test -race ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: lint
lint: vet ## Run linters (go vet for now; add golangci-lint when wired)

.PHONY: tidy
tidy: ## Tidy and verify go.mod / go.sum
	$(GO) mod tidy
	$(GO) mod verify

.PHONY: run
run: ## Run the proxy locally with .env
	$(GO) run .

.PHONY: docker
docker: ## Build the Docker image as $(IMAGE):$(TAG)
	docker build -t $(IMAGE):$(TAG) .

.PHONY: docker-run
docker-run: ## Run the image locally (expects .env in the cwd)
	docker run --rm -p 8080:8080 --env-file .env $(IMAGE):$(TAG)

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart
	helm lint charts/sr-cache

.PHONY: helm-template
helm-template: ## Render the Helm chart with defaults
	helm template sr-cache charts/sr-cache

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin/

.PHONY: ci
ci: vet test ## Run the same checks CI runs
