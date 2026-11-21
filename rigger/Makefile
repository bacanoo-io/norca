# Two Go modules (design 12.1): the core at the root and the rigger program in
# cli/. Each target runs in both. In Claude's Linux VM, source
# ../.tools/go-env.sh first.

GO ?= go
MODULES := . cli

.PHONY: all build test race vet lint fmt tidy audit check-licenses integration clean help

all: vet test build

## build: build the rigger program into bin/rigger
build:
	$(GO) -C cli build -o ../bin/rigger ./cmd/rigger

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

## clean: remove build output
clean:
	rm -rf bin/ coverage.out coverage.html

## help: list the targets
help:
	@sed -n 's/^## //p' $(MAKEFILE_LIST)
