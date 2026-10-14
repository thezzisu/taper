# Embedded 7-Zip reader

`taper` embeds `7zip.wasm`, built from the official 7-Zip **26.04** source with
WASI SDK **34.0**. The Go host runs it with wazero and supplies read-only random
access callbacks; the guest receives no filesystem preopens. `Taper.cpp` exposes
only entry metadata and selected entry extraction. Its native
Split handler supplies seekable combined streams; ZIP, RAR, CAB, WIM and image
companions use upstream volume callbacks. See the full
[support matrix](../../../docs/ARCHIVE-SUPPORT.md).

The script checks the source and SDK versions. Use the official source tarball
and SDK release, then rerun
`go test ./internal/archive`.

```sh
SEVENZIP_SRC=/path/to/7zip-26.04 \
WASI_SDK=/path/to/wasi-sdk-34.0-x86_64-linux \
internal/archive/wasm/build.sh
sha256sum internal/archive/7zip.wasm
```

Pinned inputs (SHA-256):

- `7zip-26.04.tar.gz`: `48fdc6dfe879ee2db867bb07f19c4a9406bfea01d51f48785b6c7a137881e25c`
- `wasi-sdk-34.0-x86_64-linux.tar.gz`: `b761e3a0721dbae9c09a0059e5fdb2bf917d1b4a8a7b430fb3b5aafb0984b2c4`
- Current `7zip.wasm`: `c3555853f196bc8c450560a7d2b12d0a11c0189cc7a16b522cb4f85b295898b1`

Clean builds with these inputs reproduce the Wasm digest. Source archives are
available from [7-Zip](https://github.com/ip7z/7zip/releases/tag/26.04) and
[WASI SDK](https://github.com/WebAssembly/wasi-sdk/releases/tag/wasi-sdk-34).

The build is single-threaded, keeps 64-bit file offsets, and enables standard
Wasm C++ exceptions. The runtime caps linear memory and recursive indexing has
independent entry, depth, expansion and filename limits.

The licenses for embedded upstream code are in `licenses/`: 7-Zip LGPL/BSD
terms, the unRAR restriction, and the SDK license material. `Taper.cpp` is the
small taper-specific bridge and is MIT licensed with the repository.
SDK runtime notices under `licenses/wasi/` correspond to wasi-libc
`2e6fb9d8ee0cdf9e431fbcabe8af3115de000a13` and LLVM
`895aa2c896ada719451be2e3673c83da8ddf1141`, as recorded in SDK 34.0's `VERSION`.
