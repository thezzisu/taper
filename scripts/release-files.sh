#!/bin/sh
set -eu

# Packaging only; GoReleaser also includes the checked-in 7-Zip/WASI notices.
taper_tools="$(pwd)/.local/release-tools"
mkdir -p "$taper_tools" licenses
GOBIN="$taper_tools" go install github.com/google/go-licenses/v2@v2.0.1
CGO_ENABLED=0 "$taper_tools/go-licenses" save ./cmd/taper \
    --ignore github.com/thezzisu/taper,github.com/ncruces/go-sqlite3-wasm/v6 \
    --save_path=licenses --force
# go-licenses does not recognize this module's MIT-0 text; include it verbatim.
taper_sqlite=$(go list -m -f '{{.Dir}}' github.com/ncruces/go-sqlite3-wasm/v6)
mkdir -p licenses/github.com/ncruces/go-sqlite3-wasm/v6
cp "$taper_sqlite/LICENSE" licenses/github.com/ncruces/go-sqlite3-wasm/v6/LICENSE
cp "$(go env GOROOT)/LICENSE" licenses/Go-LICENSE
