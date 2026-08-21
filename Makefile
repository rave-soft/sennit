# Main development commands. Mirrors CI (.github/workflows) and the
# pre-commit hook (.githooks/pre-commit); Taskfile.yaml remains for the
# Extras inherited from the upstream project (profiling, catwalk, release helpers).

GO       ?= go
BINARY   ?= sennit
SCRATCH  ?= /tmp/sennit-fresh

.PHONY: all build install link run dev fresh test race lint fmt tidy vet \
        schema schema-check check ci hooks clean help

all: build

## build: compile the binary into ./sennit
build:
	$(GO) build -o $(BINARY) .

## install: install into GOBIN
install:
	$(GO) install .

## link: symlink ~/.local/bin/sennit to the project build — after this,
## `make build` in the repo instantly updates the system-wide command
link: build
	ln -sf $(CURDIR)/$(BINARY) $(HOME)/.local/bin/sennit
	@echo "sennit -> $(CURDIR)/$(BINARY)"

## run: run from source
run dev:
	$(GO) run .

## fresh: run with a clean config/data dir (first-run onboarding)
fresh: build
	rm -rf $(SCRATCH) && mkdir -p $(SCRATCH)/cfg $(SCRATCH)/data $(SCRATCH)/proj
	cd $(SCRATCH)/proj && \
		SENNIT_GLOBAL_CONFIG=$(SCRATCH)/cfg/sennit.json \
		SENNIT_GLOBAL_DATA=$(SCRATCH)/data/sennit.json \
		$(CURDIR)/$(BINARY)

## test: fast test suite
test:
	$(GO) test -count=1 ./...

## race: full suite with the race detector, as CI's race job runs it
race:
	$(GO) test -race -failfast ./...

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
	$(GO) run . schema > /tmp/sennit-schema-check.json
	diff -u schema.json /tmp/sennit-schema-check.json

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
	rm -f *.test
	rm -rf site/ .venv-docs/
	$(GO) clean

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
