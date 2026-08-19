# tether — build, test, and release targets. CGO_ENABLED=0 keeps the pure-Go
# SQLite build self-contained.

SHELL := /bin/sh

# Override for a release build: make build VERSION=v1.2.3
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

GO       ?= go
LDFLAGS  := -s -w -X main.version=$(VERSION)
GOFLAGS  := -trimpath -ldflags "$(LDFLAGS)"

BIN       := tether
DIST      := dist
COVERFILE := coverage.out

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@echo "tether $(VERSION)"
	@echo
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[1m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build ./tether with the version stamped in
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN) ./cmd/$(BIN)

.PHONY: install
install: ## go install tether into GOBIN
	CGO_ENABLED=0 $(GO) install $(GOFLAGS) ./cmd/$(BIN)

.PHONY: test
test: ## Run the full test suite with the race detector
	$(GO) test -race -count=1 ./...

.PHONY: test-short
test-short: ## Run the fast tests only (-short, no race detector)
	$(GO) test -short -count=1 ./...

.PHONY: cover
cover: ## Write a coverage profile and print the total
	$(GO) test -race -count=1 -covermode=atomic -coverprofile=$(COVERFILE) ./...
	$(GO) tool cover -func=$(COVERFILE) | tail -n 1

.PHONY: lint
lint: ## Run golangci-lint using .golangci.yml
	golangci-lint run

.PHONY: fmt
fmt: ## Format every Go file in place
	gofmt -s -w .

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: tidy
tidy: ## Tidy go.mod / go.sum
	$(GO) mod tidy

.PHONY: clean
clean: ## Remove build artifacts
	rm -f $(BIN) $(COVERFILE)
	rm -rf $(DIST)
	# Stray files from an interrupted build/gofmt or a crashed editor (see .gitignore).
	find . -name '*-go-tmp-umask' -delete
	find . -name '*.go.[0-9]*' -delete
	find . -name '.fuse_hidden*' -delete
