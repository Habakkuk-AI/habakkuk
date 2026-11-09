# Shortcuts for working on Habakkuk. Run `make help` for the list.

GO      ?= go
BIN     ?= bin/habakkuk
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
ARGS    ?= chat

.DEFAULT_GOAL := help

.PHONY: build test race lint fmt vet run desktop clean help

build: ## Build bin/habakkuk (static, no CGO)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/habakkuk

test: ## Run the tests
	$(GO) test ./...

race: ## Run the tests with the race detector, as CI does
	$(GO) test -race ./...

lint: ## Run golangci-lint
	golangci-lint run ./...

fmt: ## Format the Go code with gofmt
	gofmt -w cmd internal

vet: ## Run go vet
	$(GO) vet ./...

run: build ## Build and run bin/habakkuk $(ARGS) (default: chat)
	./$(BIN) $(ARGS)

desktop: ## Build the desktop app (apps/desktop; needs CGO and a webview)
	cd apps/desktop && $(GO) build -o habakkuk-desktop .

clean: ## Remove build output
	rm -rf bin dist

help: ## List the targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-10s %s\n", $$1, $$2}'
