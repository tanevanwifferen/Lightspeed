# Lightspeed: build, test, install. `make` lists the targets.

MODULE  := github.com/tanevanwifferen/Lightspeed
BIN     := bin/lightspeed
# The version is stamped into internal/cli (see `lightspeed version`); outside a
# git checkout it stays unstamped and the binary reports what the toolchain embedded.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null)
LDFLAGS := $(if $(VERSION),-X $(MODULE)/internal/cli.version=$(VERSION),)

.DEFAULT_GOAL := help
.PHONY: help build test vet install generate check clean bench

help: ## list the targets
	@grep -E '^[a-z]+:.*## ' $(MAKEFILE_LIST) | sed -E 's/:.*## /\t/' | sort

build: ## build ./bin/lightspeed, stamped with the git version
	go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/lightspeed

test: ## go test ./...
	go test ./...

vet: ## go vet ./...
	go vet ./...

install: ## go install ./cmd/lightspeed (to $GOBIN or $GOPATH/bin), stamped
	go install -ldflags '$(LDFLAGS)' ./cmd/lightspeed

generate: ## regenerate the built-in server definitions (internal/serverdef/builtins_gen.go)
	go generate ./...

check: build vet test ## build, vet and test: what must be green before a commit

bench: ## token-budget regression bench against docs/bench/baseline.json (needs a real gopls; docs/bench/README.md)
	go test ./internal/cli/ -run TestBenchTokenBudget -v -timeout 5m

clean: ## remove ./bin
	rm -rf bin
