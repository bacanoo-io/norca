# Two Go modules (design 12.1): the core at the root and the rig program in
# cli/. Each target runs in both. In Claude's Linux VM, source
# ../.tools/go-env.sh first.

GO ?= go
MODULES := . cli

.PHONY: all build test race vet lint fmt tidy audit clean help

all: vet test build

## build: build the rig program into bin/rig
build:
	$(GO) -C cli build -o ../bin/rig ./cmd/rig

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

## clean: remove build output
clean:
	rm -rf bin/ coverage.out coverage.html

## help: list the targets
help:
	@sed -n 's/^## //p' $(MAKEFILE_LIST)
