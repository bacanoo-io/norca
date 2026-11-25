# Two Go modules (design 12.1): the core at the root and the rigger program in
# cli/. Each target runs in both. In Claude's Linux VM, source
# ../.tools/go-env.sh first.

GO ?= go
MODULES := . cli

.PHONY: all build test race vet lint fmt tidy audit check-licenses integration examples clean help

all: vet test build

# The build rigger version prints: VERSION is set by a release (make build
# VERSION=v0.4.0); COMMIT and DATE are the commit of the checkout and its
# time. A build without them (go build, go install) reads what the go
# command recorded instead.
VERSION ?=
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)
DATE ?= $(shell git log -1 --format=%cI 2>/dev/null)
BUILD_PKG := github.com/bacanoo-io/norca/rigger/cli/command
LDFLAGS := -X $(BUILD_PKG).buildVersion=$(VERSION) -X $(BUILD_PKG).buildCommit=$(COMMIT) -X $(BUILD_PKG).buildDate=$(DATE)

## build: build the rigger program into bin/rigger, with its version, commit and date
build:
	$(GO) -C cli build -ldflags "$(LDFLAGS)" -o ../bin/rigger ./cmd/rigger

## test: run the tests of both modules
test:
	@for m in $(MODULES); do $(GO) -C $$m test -count=1 ./... || exit 1; done

## race: run the tests with the race detector (needs cgo)
race:
	@for m in $(MODULES); do $(GO) -C $$m test -race -count=1 ./... || exit 1; done

## vet: run go vet in both modules
vet:
	@for m in $(MODULES); do $(GO) -C $$m vet ./... || exit 1; done

## lint: run golangci-lint in both modules
lint:
	@for m in $(MODULES); do (cd $$m && golangci-lint run ./...) || exit 1; done

## fmt: format the code of both modules
fmt:
	@for m in $(MODULES); do (cd $$m && golangci-lint fmt ./...) || exit 1; done

## tidy: tidy go.mod and go.sum of both modules
tidy:
	@for m in $(MODULES); do $(GO) -C $$m mod tidy || exit 1; done

## audit: look for known vulnerabilities reachable from the code
audit:
	@for m in $(MODULES); do (cd $$m && govulncheck ./...) || exit 1; done

## check-licenses: no dependency and no file under the Business Source License
check-licenses:
	@scripts/check-licenses.sh

## integration: the tests against a running agent (norca agent -dev; NORCA_ADDR)
integration:
	$(GO) -C cli test -tags integration -count=1 ./...

## examples: write the signatures and the lock files of examples/ again, after a change of a module or a stack there (examples/README.md)
examples:
	$(GO) test -count=1 -run '^TestSyncStack_Examples$$' . -update

## clean: remove build output
clean:
	rm -rf bin/ coverage.out coverage.html

## help: list the targets
help:
	@sed -n 's/^## //p' $(MAKEFILE_LIST)
