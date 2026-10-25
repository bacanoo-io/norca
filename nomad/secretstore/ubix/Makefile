# uBixVault — developer tasks.
# Run `make help` for the list.

BINARY      := ubixvault
PKG         := ./...
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -X main.version=$(VERSION)
GOBIN       := $(shell go env GOPATH)/bin
IMAGE       ?= ghcr.io/cwolsen7905/ubixvault
CHART       := deploy/charts/ubixvault
# DSN for the local MariaDB from `docker compose up -d mariadb` (compose.yaml).
MARIADB_DSN ?= root:root@tcp(127.0.0.1:3306)/ubixvault

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the ubixvault binary into ./bin
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/ubixvault

.PHONY: test
test: ## Run tests with the race detector and coverage
	go test -race -covermode=atomic -coverprofile=coverage.out $(PKG)

.PHONY: cover
cover: test ## Show total test coverage
	@go tool cover -func=coverage.out | tail -1

.PHONY: fmt
fmt: ## Format the code
	gofmt -w .

.PHONY: fmtcheck
fmtcheck: ## Fail if any file is not gofmt-clean
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## Run go vet
	go vet $(PKG)

.PHONY: lint
# Keep in step with the version .github/workflows/ci.yml pins; the config is v2,
# which a v1 golangci-lint cannot read.
GOLANGCI_LINT_VERSION ?= v2.12.2
# Run it from where `go install` puts it, so a different golangci-lint earlier
# on PATH (or none at all) cannot change the result.
GOLANGCI_LINT := $(or $(shell go env GOBIN),$(shell go env GOPATH)/bin)/golangci-lint

lint: ## Run golangci-lint at CI's version (installs it if missing or different)
	@$(GOLANGCI_LINT) version 2>/dev/null | grep -q "version $(GOLANGCI_LINT_VERSION:v%=%) " || \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	$(GOLANGCI_LINT) run

.PHONY: vuln
vuln: ## Run govulncheck (installs it if missing)
	@command -v govulncheck >/dev/null 2>&1 || go install golang.org/x/vuln/cmd/govulncheck@latest
	govulncheck $(PKG)

.PHONY: integration
integration: ## Integration tests against the compose MariaDB (MARIADB_DSN)
	UBIXVAULT_MARIADB_DSN='$(MARIADB_DSN)' \
		go test -tags integration ./internal/database/... ./internal/storage/...

.PHONY: console-e2e
console-e2e: ## Drive the /ui/ console in headless Chrome against a fresh server (needs Node >= 22 + Chrome)
	test/e2e/console/run.sh

.PHONY: fuzz
fuzz: ## Fuzz the policy (HCL/JSON) parser 30s; swap the target for others
	go test -run=x -fuzz=FuzzParseDocument -fuzztime=30s ./internal/policy/

.PHONY: tidy
tidy: ## Tidy go.mod/go.sum
	go mod tidy

.PHONY: ci
ci: fmtcheck vet test build ## Run the checks CI runs

.PHONY: docker
docker: ## Build the container image (tagged with VERSION)
	docker build -t $(IMAGE):$(VERSION) --build-arg VERSION=$(VERSION) .

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart, single-replica and HA
	helm lint $(CHART) --set tls.existingSecret=tls --set autoUnseal.existingSecret=kek
	helm lint $(CHART) --set tls.existingSecret=tls --set autoUnseal.existingSecret=kek \
		--set storage.type=mysql --set storage.mysql.dsnSecret=dsn --set ha.enabled=true --set replicaCount=3

.PHONY: helm-template
helm-template: ## Render the Helm chart to stdout
	helm template ubixvault $(CHART) --set tls.existingSecret=tls --set autoUnseal.existingSecret=kek

.PHONY: clean
clean: ## Remove build and coverage artifacts
	rm -rf bin coverage.out
