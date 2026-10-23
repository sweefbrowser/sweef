# Developer entry points. CI runs the same commands (see .github/workflows/ci.yml).
GO ?= go
export CGO_ENABLED = 0

.PHONY: all build vet fmt fmt-check embeds size test test-sdk test-all check clean help

all: check

help: ## list targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*## /\t/'

build: ## build ./dist-less binary at ./sweef
	$(GO) build -o sweef ./cmd/sweef

vet: ## go vet (composite literal check off, as in CI)
	$(GO) vet -composites=false ./...

fmt: ## format our code (third_party is left alone)
	gofmt -w $$(git ls-files '*.go' | grep -v '^third_party/')

fmt-check: ## fail if our code is not gofmt-clean
	@out=$$(gofmt -l $$(git ls-files '*.go' | grep -v '^third_party/')); \
	  if [ -n "$$out" ]; then echo "not gofmt-clean:"; echo "$$out"; exit 1; fi

embeds: ## rebuild the compressed embeds (.js.z, web.zar, gofont.zar, zoneinfo.zar) after editing JS, UI assets or fonts
	$(GO) generate ./internal/engine ./internal/font ./internal/intl

size: ## release-flags linux/amd64 build: size, headroom to the budget, and where the bytes are (per package and section)
	SWEEF_SIZE_TABLE=1 DIST=$${TMPDIR:-/tmp}/sweef-size scripts/release/build.sh --targets linux/amd64 --skip-launchers

test: ## short tests (what CI runs on Linux)
	$(GO) test -short -count=1 ./...

test-sdk: ## Node and Python SDK tests against a freshly built sweef (local fixtures, no network)
	$(GO) build -o $${TMPDIR:-/tmp}/sweef-sdk-test ./cmd/sweef
	SWEEF_BIN=$${TMPDIR:-/tmp}/sweef-sdk-test node --test pkg/sdk/node/test/sdk.test.js pkg/sdk/node/test/install.test.js
	SWEEF_BIN=$${TMPDIR:-/tmp}/sweef-sdk-test python3 -m unittest discover -s pkg/sdk/py/tests

test-all: ## including the slow, FATE-gated tests
	$(GO) test -count=1 -timeout 60m ./...

check: fmt-check vet test ## everything a PR must pass


clean:
	rm -f sweef *.test *.prof coverage.out
	rm -rf dist
