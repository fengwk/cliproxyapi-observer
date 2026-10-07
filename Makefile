SHELL := /bin/bash

PLUGIN_ID := cliproxyapi-observer
GOOS ?= $(shell bash -c 'go env GOOS')
GOARCH ?= $(shell bash -c 'go env GOARCH')
VERSION ?= $(shell bash scripts/plugin-version.sh)

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

.PHONY: help build test vet race integration package clean

help:
	@echo "cliproxyapi-observer"
	@echo ""
	@echo "  make build        build the native plugin into $(DIST_DIR)/"
	@echo "  make test         go vet, go test and the dependency-free UI tests"
	@echo "  make race         go test -race"
	@echo "  make integration  real-host tests (requires CPA_BINARY=/abs/path/cli-proxy-api)"
	@echo "  make package      zip the built plugin for the current GOOS/GOARCH"
	@echo "  make clean        remove dist/"
	@echo ""
	@echo "Variables: GOOS, GOARCH, VERSION=$(VERSION)"
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
	node --test internal/web/ui.test.cjs

race:
	go test -race ./...

integration:
	@if [ -z "$(CPA_BINARY)" ]; then \
		echo "CPA_BINARY must point to a built cli-proxy-api host binary" >&2; \
		exit 2; \
	fi
	CPA_BINARY="$(CPA_BINARY)" go test -tags=integration ./tests/integration -count=1 -timeout=120s

package: build
	bash scripts/package-plugin.sh "$(VERSION)" "$(GOOS)" "$(GOARCH)" "dist/pkg"

clean:
	rm -rf dist
