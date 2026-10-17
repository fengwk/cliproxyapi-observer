SHELL := /bin/bash

PLUGIN_ID := cliproxyapi-observer
GOOS ?= $(shell bash -c 'go env GOOS')
GOARCH ?= $(shell bash -c 'go env GOARCH')
VERSION ?= $(shell bash scripts/plugin-version.sh)

# Browser acceptance uses the dependency-free CDP runner documented in
# internal/web/browser/README.md. Every variable keeps an environment override.
OBSERVER_BROWSER ?= internal/web/browser/run.cjs
BROWSER_REPORT_DIR ?= $(CURDIR)/../browser-report
CHROME_BIN ?= /usr/bin/google-chrome-stable

ifeq ($(GOOS),windows)
LIB_EXT := dll
else ifeq ($(GOOS),darwin)
LIB_EXT := dylib
else
LIB_EXT := so
endif

DIST_DIR := dist/$(GOOS)/$(GOARCH)
LIB := $(DIST_DIR)/$(PLUGIN_ID).$(LIB_EXT)
VERSION_VAR := github.com/fengwk/cliproxyapi-observer/internal/plugin.Version
LDFLAGS := -s -w -X $(VERSION_VAR)=$(VERSION)

.PHONY: help build test vet race browser integration package clean

help:
	@echo "cliproxyapi-observer"
	@echo ""
	@echo "  make build        build the native plugin into $(DIST_DIR)/"
	@echo "  make test         canonical Go + Node suite (vet, unit, dependency-free UI)"
	@echo "  make race         go test -race across all packages"
	@echo "  make browser      real Chrome acceptance via $(OBSERVER_BROWSER)"
	@echo "  make integration  real-host tests (requires CPA_BINARY=/abs/path/cli-proxy-api)"
	@echo "  make package      zip the built plugin for the current GOOS/GOARCH"
	@echo "  make clean        remove dist/"
	@echo ""
	@echo "Variables: GOOS, GOARCH, VERSION=$(VERSION)"
	@echo "Browser:   CHROME_BIN=$(CHROME_BIN), BROWSER_REPORT_DIR=$(BROWSER_REPORT_DIR)"
	@echo "Install: copy $(DIST_DIR)/ into <cpa>/plugins/$(GOOS)/$(GOARCH)/"

build:
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -buildmode=c-shared \
		-ldflags '$(LDFLAGS)' -o $(LIB) .
	@rm -f $(DIST_DIR)/$(PLUGIN_ID).h
	@echo "built $(LIB) (version $(VERSION))"

vet:
	go vet ./...

test: vet
	go test ./...
	node --test internal/web/ui.test.cjs internal/web/models.test.cjs internal/web/browser/mock-server.test.cjs

race:
	go test -race ./...

browser:
	@if [ ! -x "$(CHROME_BIN)" ]; then \
		echo "CHROME_BIN is not executable: $(CHROME_BIN)" >&2; \
		exit 2; \
	fi
	CHROME_BIN="$(CHROME_BIN)" node "$(OBSERVER_BROWSER)" --report-dir "$(BROWSER_REPORT_DIR)"

integration:
	@if [ -z "$(CPA_BINARY)" ]; then \
		echo "CPA_BINARY must point to a built cli-proxy-api host binary" >&2; \
		exit 2; \
	fi
	CPA_BINARY="$(CPA_BINARY)" go test -tags=integration ./tests/integration -count=1 -timeout=300s

package: build
	bash scripts/package-plugin.sh "$(VERSION)" "$(GOOS)" "$(GOARCH)" "dist/pkg"

clean:
	rm -rf dist
