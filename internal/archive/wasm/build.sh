#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
: "${SEVENZIP_SRC:?set SEVENZIP_SRC to an extracted official 7-Zip source tree}"
: "${WASI_SDK:?set WASI_SDK to wasi-sdk-34.0-x86_64-linux}"
test "$(sed -n 's/^#define MY_VERSION_NUMBERS "\(.*\)"/\1/p' "$SEVENZIP_SRC/C/7zVersion.h")" = '26.04'
test "$(sed -n '1p' "$WASI_SDK/VERSION")" = '34.0'
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp -a "$SEVENZIP_SRC"/. "$work/7zip"
mkdir -p "$work/7zip/CPP/7zip/Bundles/TaperWasm"
cp "$root/internal/archive/wasm/Taper.cpp" "$root/internal/archive/wasm/compat.h" "$root/internal/archive/wasm/makefile.gcc" "$work/7zip/CPP/7zip/Bundles/TaperWasm/"
patch -d "$work/7zip" -p1 < "$root/internal/archive/wasm/wasi.patch"
cd "$work/7zip/CPP/7zip/Bundles/TaperWasm"
make -j"${JOBS:-$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 2)}" -f makefile.gcc O=_wasi \
  CC="$WASI_SDK/bin/clang" CXX="$WASI_SDK/bin/clang++" \
  CFLAGS_BASE='-O2 -c -DNDEBUG -D_FILE_OFFSET_BITS=64 -D_WASI_EMULATED_GETPID -D_WASI_EMULATED_SIGNAL -D_WASI_EMULATED_PROCESS_CLOCKS -ffunction-sections -fdata-sections' \
  CXXFLAGS_EXTRA='-std=c++17 -include compat.h -fwasm-exceptions -mllvm -wasm-use-legacy-eh=false' \
  LIB2='-fwasm-exceptions -lunwind -mexec-model=reactor -lwasi-emulated-getpid -lwasi-emulated-signal -lwasi-emulated-process-clocks -Wl,--export=taper_open -Wl,--export=taper_extract -Wl,--export=malloc -Wl,--export=free -Wl,-z,stack-size=1048576 -Wl,--initial-memory=2097152 -Wl,--max-memory=1073741824'
cp _wasi/taper7z.wasm "$root/internal/archive/7zip.wasm"
