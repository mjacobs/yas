# Building yas

yas is a cgo-free Go project: the agent (`cmd/yas`) and the sync hub
(`cmd/yas-server`) both build to static binaries with no C toolchain.

## Prerequisites

- Go (version pinned in [go.mod](go.mod)).
- For `make smoke` only: `curl` and `jq`.

## Build & test

```bash
make build        # go build ./...
make test         # go test ./...
make vet lint     # go vet + golangci-lint (gosec, misspell, gofmt)
make smoke        # end-to-end: record -> search -> serve -> curl (throwaway data dir)
```

## Static-binary invariant (cgo-free)

The agent must build with `CGO_ENABLED=0` and cross-compile to any target by
swapping `GOOS`/`GOARCH` — the local store is `modernc.org/sqlite` (pure Go)
precisely so this holds. **Never introduce a cgo dependency.**

```bash
make cross                                   # builds the GOOS/GOARCH matrix to /dev/null
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/yas   # one target, by hand
```

`make cross` fails loudly if anything pulls in cgo.

## Install

```bash
make install                       # cgo-free static `yas` -> ~/.local/bin
make install BINDIR=/usr/local/bin # custom directory
make install PREFIX=/opt/yas      # -> $PREFIX/bin
make install VERSION=1.2.3         # override the stamped version
make uninstall                     # remove the installed binary
```

`make install` builds **only the agent** (`cmd/yas`) — the server is deployed
separately (see [deploy/](deploy/)). It builds with `CGO_ENABLED=0`,
`-trimpath`, and strips debug info (`-s -w`) for a small, reproducible binary.

### Install script

[scripts/install.sh](scripts/install.sh) wraps the same build and additionally
stages the zsh integration (`shell/yas.zsh` → `~/.config/yas/hook.zsh`,
`shell/yas-fzf.zsh` → `~/.config/yas/ctrl-r.zsh`), printing the two `source`
lines to add to `~/.zshrc` — it never edits your shell config itself.

```bash
./scripts/install.sh                 # yas -> ~/.local/bin, hooks -> ~/.config/yas
BINDIR=/usr/local/bin ./scripts/install.sh
./scripts/install.sh --with-server   # also build/install yas-server
./scripts/install.sh --no-shell      # binaries only
```

## Version stamping

`yas version` (also `yas --version` / `-v`) prints `main.version`, which is
baked into the binary at link time.

- **Default**: `VERSION` comes from `git describe --tags --always --dirty`:
  - a tag if one points at HEAD (`v0.1.0`),
  - otherwise the short commit SHA (`9284633`),
  - suffixed `-dirty` when the worktree has uncommitted changes.
- **Override**: `make install VERSION=1.2.3`.
- A plain `go build ./...` (no ldflags) keeps the in-source default
  `0.0.0-dev` — version stamping happens only via `make install`.

### Mechanics (and a gotcha)

The stamp is `-ldflags '-s -w -X main.version=$(VERSION)'`.

The agent's `version` variable lives in **package main**, so the linker symbol
is `main.version` — **not** the full import path
`github.com/mjacobs/yas/cmd/yas.version`. Using the import-path form here
*silently no-ops* (the value stays `0.0.0-dev`); `-X` does not warn on an
unknown symbol. Confirm what the linker sees with:

```bash
go tool nm "$(command -v yas)" | grep '\.version$'   # -> "... main.version"
```

## Releasing

```bash
git tag v0.1.0 && git push --tags
make install        # `yas version` now reports v0.1.0
make release        # static tarballs for the full GOOS/GOARCH matrix
```

`make release` cross-compiles the agent **and** server for every
`CROSS_TARGETS` entry (linux/darwin × amd64/arm64), packaging each pair as
`dist/yas_<version>_<os>_<arch>.tar.gz` plus a `dist/SHA256SUMS` manifest.
Everything is `CGO_ENABLED=0` — no C toolchain, no cross-sysroots.
