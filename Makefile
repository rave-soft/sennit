# Main development commands. Mirrors CI (.github/workflows) and the
# pre-commit hook (.githooks/pre-commit); Taskfile.yaml remains for the
# charm-inherited extras (profiling, catwalk, release helpers).

GO       ?= go
BINARY   ?= braid
SCRATCH  ?= /tmp/braid-fresh

.PHONY: all build install run dev fresh test race lint fmt tidy vet \
        schema schema-check swag check ci hooks clean help

all: build

## build: compile the binary into ./braid
build:
	$(GO) build -o $(BINARY) .

## install: install into GOBIN
install:
	$(GO) install .

## link: symlink ~/.local/bin/braid to the project build — after this,
## `make build` in the repo instantly updates the system-wide command
link: build
	ln -sf $(CURDIR)/$(BINARY) $(HOME)/.local/bin/braid
	@echo "braid -> $(CURDIR)/$(BINARY)"

## run: run from source
run dev:
	$(GO) run .

## fresh: run with a clean config/data dir (first-run onboarding)
fresh: build
	rm -rf $(SCRATCH) && mkdir -p $(SCRATCH)/cfg $(SCRATCH)/data $(SCRATCH)/proj
	cd $(SCRATCH)/proj && \
		BRAID_GLOBAL_CONFIG=$(SCRATCH)/cfg/braid.json \
		BRAID_GLOBAL_DATA=$(SCRATCH)/data/braid.json \
		$(CURDIR)/$(BINARY)

## test: fast test suite
test:
	$(GO) test -count=1 ./...

## race: full suite exactly as CI runs it
race:
	@set -eu; \
	test_binary=$$(mktemp "$${TMPDIR:-/tmp}/braid-test.XXXXXX"); \
	trap 'rm -f "$$test_binary"' EXIT; \
	$(GO) build -o "$$test_binary" .; \
	BRAID_TEST_BINARY="$$test_binary" $(GO) test -race -failfast ./...

## lint: golangci-lint with the CI config
lint:
	golangci-lint run --timeout 10m

## fmt: gofmt the tree
fmt:
	gofmt -w .

## tidy: go mod tidy and fail on drift (as CI does)
tidy:
	$(GO) mod tidy
	git diff --exit-code go.mod go.sum

## vet: go vet
vet:
	$(GO) vet ./...

## schema: regenerate schema.json
schema:
	$(GO) run . schema > schema.json

## schema-check: fail if schema.json is stale (as CI does)
schema-check:
	$(GO) run . schema > /tmp/braid-schema-check.json
	diff -u schema.json /tmp/braid-schema-check.json

## swag: regenerate swagger docs
swag:
	$(GO) run github.com/swaggo/swag/cmd/swag@v1.16.6 init \
		--generalInfo main.go --dir . --output internal/swagger \
		--packageName swagger --parseDependency --parseInternal --parseDepth 5

## check: everything the pre-commit hook runs
check: tidy build lint schema-check

## ci: full local equivalent of the CI matrix job + lint
ci: check race

## hooks: enable the versioned pre-commit hook
hooks:
	git config core.hooksPath .githooks

## clean: remove build artifacts
clean:
	rm -f $(BINARY)
	$(GO) clean

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
