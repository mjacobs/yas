# yas — build / test / quality targets.
GO ?= go

# Version stamped into the binary via -ldflags: a git tag or short SHA, suffixed
# -dirty when the worktree has uncommitted changes. Override for a release, e.g.
# `make install VERSION=1.2.3`.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Where `make install` puts the agent CLI. Override PREFIX or BINDIR as needed,
# e.g. `make install BINDIR=/usr/local/bin`.
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin

# Release build flags: stamp the version, strip debug info, and trim absolute
# paths for a smaller, reproducible static binary. The agent CLI is package
# main, so the linker symbol is main.version (not the full import path).
GO_LDFLAGS := -s -w -X main.version=$(VERSION)
GO_BUILD_FLAGS := -trimpath -ldflags '$(GO_LDFLAGS)'

# cgo-free cross-compile matrix — the static-binary property that picked
# modernc.org/sqlite. `make cross` fails loudly if anything pulls in cgo.
CROSS_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: all build test test-js vet lint cross smoke screenshots install uninstall release clean

all: build test

build:
	$(GO) build ./...

test:
	$(GO) test ./...

# Optional (needs Node >= 22 — relies on unflagged ESM syntax detection for the
# extensionless-manifest static/tokens.js; NOT part of `test`, build/CI stay
# Go-only): table-test the web UI's pure JS modules — the token parser against
# the shared vectors, and the view-option helpers (dup-collapsing, filters).
test-js:
	node --test internal/webui/tokens_test.mjs internal/webui/view_test.mjs

vet:
	$(GO) vet ./...

lint:
	golangci-lint run

cross:
	@set -e; for t in $(CROSS_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "  CGO_ENABLED=0 $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -o /dev/null ./cmd/yas; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -o /dev/null ./cmd/yas-server; \
	done; \
	echo "cgo-free cross-compile OK"

smoke: build
	./scripts/smoke.sh

# Regenerate the README terminal screenshots over synthetic demo data.
# Needs `freeze` (go install github.com/charmbracelet/freeze@latest).
screenshots:
	./scripts/screenshots.sh

# Build the cgo-free static agent with the version stamped in and install it.
install:
	@mkdir -p "$(BINDIR)"
	CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS) -o "$(BINDIR)/yas" ./cmd/yas
	@echo "installed yas $(VERSION) -> $(BINDIR)/yas"

uninstall:
	rm -f "$(BINDIR)/yas"

# Static release artifacts: one tarball per CROSS_TARGETS entry containing the
# agent + server, plus a SHA256SUMS manifest. Everything is CGO_ENABLED=0 and
# version-stamped; `make release VERSION=1.2.3` pins a tag instead of the
# git-describe default.
release:
	@set -e; rm -rf dist; mkdir -p dist; \
	for t in $(CROSS_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		out="dist/yas_$(VERSION)_$${os}_$${arch}"; \
		echo "  $$os/$$arch -> $$out.tar.gz"; \
		mkdir -p "$$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build $(GO_BUILD_FLAGS) -o "$$out/yas" ./cmd/yas; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build $(GO_BUILD_FLAGS) -o "$$out/yas-server" ./cmd/yas-server; \
		tar -C "$$out" -czf "$$out.tar.gz" yas yas-server; \
		rm -rf "$$out"; \
	done; \
	(cd dist && sha256sum *.tar.gz > SHA256SUMS); \
	echo "release artifacts in dist/ ($(VERSION))"

clean:
	rm -rf bin dist
