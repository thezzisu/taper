# taper

[![CI](https://github.com/thezzisu/taper/actions/workflows/ci.yml/badge.svg)](https://github.com/thezzisu/taper/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/thezzisu/taper?include_prereleases)](https://github.com/thezzisu/taper/releases)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

Taper backs up files to manually loaded LTO tapes and keeps an encrypted SQLite
catalog you can search while the tapes are offline. Replica sets share one
immutable archive stream across cartridges and sparse disk images. Small files
share 1 MiB records; complete archives can be prepared on disk and copied to tape.

**v0.1.0 is a preview.** The device backend is Linux-only, with hardware
qualification on LTO-6 and amd64. Linux arm64 binaries are cross-built. See
[validation](docs/VALIDATION.md) for what has been tested and its limits.

## Install

Download a Linux archive and `checksums.txt` from
[Releases](https://github.com/thezzisu/taper/releases). Verify the downloaded
archive before extracting it:

```sh
sha256sum --check --ignore-missing checksums.txt
tar -xzf taper_0.1.0_linux_amd64.tar.gz
./taper --version
```

Or build with Go 1.26 or later; no C compiler or system SQLite is needed:

```sh
git clone https://github.com/thezzisu/taper.git
cd taper
make build
```

Source builds report `dev`; use `make build VERSION=v0.1.0` to set a version.
The archive indexer embeds 7-Zip as WASI WebAssembly and does not invoke an
external `7z` executable. Its [support matrix](docs/ARCHIVE-SUPPORT.md) documents
formats, multi-volume archives, encryption and indexing limits.

## First backup

Choose the correct Linux SG device. `/dev/sgN` below is a placeholder; a stable
symlink to that SG node also works. `/dev/nstN` uses a different driver interface.
Run taper as your user with device permissions; any elevation happens outside it.

```sh
taper catalog init
taper key generate --name COLD --key-file /private/cold.key --keycard /private/cold.keycard
# Save the recovery card separately.
taper replica create cold --key-file /private/cold.key
taper replica add cold --device /dev/sgN --label 123456L6

taper stage add --source /data/snapshot .
taper stage plan --replica cold --name photos
taper stage write --image /cache/cold.img
taper replica sync cold --from-image /cache/cold.img --to-device /dev/sgN

taper archive ls
taper archive browse photos
taper catalog export --dest /private/catalog-snapshot
```

Keep sources unchanged from staging through writing. `stage rm PATH` removes a
selection without deleting the source. `stage add --rules FILE` accepts explicit
include/exclude patterns; taper has no built-in cleanup policy.

Planning hashes and deduplicates files, indexes user archives and checks the
smallest member tape's budget, including archives still owed to that member.
An image-only write therefore still needs a registered tape with recorded capacity.
Hardware compression defaults on; optional `stage write --zstd` defaults off.
Capacity planning assumes no compression gain.

## Images, browsing and recovery

```sh
# Capture once, then merge caches without another tape read.
taper replica sync cold --from-device /dev/sgN --to-image /ssd/cold.img
taper replica sync cold --from-image /ssd/cold.img --to-image /hdd/cold.img

# Offline metadata, including nested archive paths.
taper archive browse photos --tui

# Mount original files read-only; requires mount permission supplied externally.
taper replica mount cold /mount/point --image /hdd/cold.img
```

Images can be sparse. Offline missing blocks return `ENODATA`; damaged data
returns `EIO`. With `--device`, mounting can fill missing ranges using bounded
caching and coalesced reads. Multiple archives appear under separate directories;
`--archive ID` mounts one directly at the root.

Deep indexing follows archive layers such as `backup.zip!/game.rar!/OST/track.flac`.
Documents, applications and disk images stay ordinary files. Locked containers
can be indexed with `stage unlock PATH`; `--remember` saves the password in the
encrypted catalog. Original archive bytes are always preserved.

`tape verify` reads the whole tape or an explicit `--archive FIRST[-LAST]` range
once. `tape reindex` rebuilds metadata in one full pass. Neither runs automatically
after writing. Qualify the complete I/O path before using tape, and choose a
memory pool that keeps the drive streaming; `--memory` is not a process RSS limit.

## CLI and configuration

Commands use English text and one spelling, with no aliases. `--help` lists
options. Human output includes scope previews and progress; `--json` provides
structured stdout with progress on stderr. Tape movement prompts once;
`-y/--yes` confirms and `-n/--dry-run` previews without moving tape or changing data.

| Setting | Flag | Environment / default |
|---|---|---|
| Catalog | `--catalog` | `TAPER_CATALOG`, otherwise `$XDG_DATA_HOME/taper/catalog.db` or `~/.local/share/taper/catalog.db` |
| Passphrase file | `--pass-file` | `TAPER_PASS_FILE`, otherwise an existing secure `session.pass` in the default data directory |

Explicit flags take precedence. Without a passphrase file, taper prompts without
echoing. Passphrase, key and archive-password files must be private (`chmod 600`);
unsafe permissions produce an error.

Keep catalog exports and recovery keys separately. The catalog passphrase and
replica encryption key have different roles. Tape, image and catalog formats
remain version 1; unsupported schemas are rejected without automatic migration.
Format v1 does not preserve permissions, ownership, empty directories or original
sparse-file layout. Image encryption is XChaCha20 with CRC checks, not AEAD.

- [Command reference](docs/COMMANDS.md)
- [Replica sets, images and FUSE](docs/IMAGES.md)
- [Operation and recovery](docs/OPERATIONS.md)
- [Tape format](docs/FORMAT.md)
- [Validation](docs/VALIDATION.md)

## Development and releases

```sh
make check        # formatting, module consistency and go vet
make test         # tests with race detection; no physical tape access
make integration  # actual kernel FUSE with simulated tapes; needs sudo
make fuzz         # bounded decoder fuzzing
make build
```

After CI passes on `main`, push an annotated tag for that commit. Do not reuse a tag:

```sh
git tag -a vX.Y.Z -m 'Release vX.Y.Z'
git push origin vX.Y.Z
```

The release workflow requires successful CI for that commit. GoReleaser builds
static Linux amd64/arm64 archives with dependency licenses and SHA-256 checksums.
All `v0.*` and suffixed tags are published as prereleases. The release workflow
generates release notes from GitHub and does not rerun CI or access physical tape.

## License

Copyright 2026 thezzisu.

[Apache License 2.0](LICENSE), downloaded from the
[Apache Software Foundation](https://www.apache.org/licenses/LICENSE-2.0.txt).
Third-party components retain their own licenses; see the
[embedded 7-Zip notices](internal/archive/wasm/licenses/7zip.txt). Binary archives
include Go dependency, 7-Zip and WASI license notices.
