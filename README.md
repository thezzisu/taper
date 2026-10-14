# taper

Packed archives for manually loaded LTO tapes, with an encrypted SQLite catalog
that stays searchable while the tapes are offline. Small files share 1 MiB
records. Adjacent path/length metadata allows rebuilding the catalog by reading
the tape sequentially.

The device backend is Linux-only and uses SG_IO directly in the current process.
The current hardware qualification is for LTO-6. Select the drive explicitly when
running a device command; planning recommends tapes from the offline catalog.

## Build

Go 1.26 or newer. The SQLite dependency does not require a C SQLite installation.

```sh
go build -o taper ./cmd/taper
```

The archive indexer embeds the official 7-Zip 26.04 engine as a WASI WebAssembly
module and runs it with wazero. It does not invoke `7z`, require a system 7-Zip
installation, or use CGO. The pinned bridge build and license texts are in
`internal/archive/wasm/`. [Archive support](docs/ARCHIVE-SUPPORT.md) lists all
formats, volume layouts, encryption support and tested limits.

[Commands](docs/COMMANDS.md) lists every implemented command and option.
[CLI/TUI design](docs/CLI-DESIGN.md) records the implemented command interface
and the remaining TUI proposal.

## First backup

Select the correct SG device before running a tape command. `/dev/sg1` below is
an example, not a persistent identity. A stable symlink resolving to the SG node
also works; the usual `/dev/tape/by-id/*-nst` links resolve to a different driver
interface and cannot be passed to this backend.

```sh
taper catalog init
taper key generate --name TAPE001 --key-file /private/TAPE001.key --keycard /private/TAPE001.keycard
# Save the recovery card separately before registering the key.
taper tape register --device /dev/sg1 --name TAPE001 --key-file /private/TAPE001.key

taper stage add --source /data/snapshot --rules /private/rules.json .
taper stage ls
taper stage rm unwanted/path
taper stage plan --name photos
# If plan reports a locked container, enter its password without echoing it.
taper stage unlock --remember 'backup.7z'
# This example allocates a 1 GiB write pool. Size it for the measured source rate.
taper stage write --device /dev/sg1 --memory 1073741824

taper archive ls
taper archive browse --archive ARCHIVE_ID
taper catalog export --dest /private/catalog-snapshot
```

Keep source files unchanged from `stage add` through planning and all replica
writes. Taper checks source identity, metadata and streaming hashes; it does not
freeze files or create filesystem snapshots.

A hidden prompt unlocks the catalog. `--pass-file` reads a local passphrase file
for unattended use. The default catalog is `~/.local/share/taper/catalog.db`.
Keep credentials and catalog exports outside the source tree. Flags may precede
or follow positional paths; `--memory` takes an integer byte count. Output is
human-readable by default. Use `--json` for automation; results go to stdout and
progress goes to stderr. Redirected output contains no terminal styling.
The catalog uses schema version 1. Opening another version fails; taper never
upgrades it or creates missing tables implicitly.

Hardware compression defaults to on when registering a tape; use
`tape register --hardware-compression=false ...` to save an off preference.
Registration saves that choice in the catalog. Each write applies it to the drive
and checks the resulting setting before writing records. Optional software
compression (`stage write --zstd`) defaults to off and is independent of hardware
compression. Capacity planning always assumes no compression gain.

## Staging and tape choice

One catalog has one staging area with one source root. `stage add` records files;
adding a directory again refreshes that selection. `stage rm` changes the
selection, and `stage rm .` clears it. Neither deletes source files.

`stage plan` hashes the selection, deduplicates identical contents within the
archive, recursively indexes user-created archive files, computes exact
uncompressed encoded bytes, and recommends registered tapes using
saved capacity and health information. It proposes a new tape when none fit.
It does not move, split, or automatically write the data. Repeating a current
plan reuses it; interrupted hashing can resume from saved checkpoints.

Deep indexing follows packaging layers: `backup.zip!/game.rar!/OST.zip!/track.flac`.
Only conventional archive suffixes enter this recursion. Programs (including
self-extracting EXE files), APK/JAR packages, installers, documents, media and disk
images remain ordinary files at every level. Taper does not inspect arbitrary
files for archive signatures. It preserves all original bytes regardless of
whether it builds an inner tree; these rules do not exclude files from backup.

If an archive is encrypted, planning records its visible state and continues.
Use `stage unlock PATH` with a hidden prompt or `--password-file`; add
`--remember` only when the password should be retained in the encrypted catalog.

`stage write` appends a replica to the explicitly supplied drive. A replica is
committed after its COMMIT record and synchronous filemark succeed. With
`stage plan --copies N`, the stage stays until N distinct tapes have committed
copies. Once those copies exist, the stage clears. `archive ls` shows committed
archives; a committed copy is not necessarily verified.

Rules contain explicit include/exclude patterns. There are no built-in trash,
software, or course-material exclusions:

```json
{"include": [], "exclude": ["path/to/exclude/**"]}
```

A nonempty include list limits the selection; exclusions subtract from it.
`**` matches directory components, and `*` and `?` match within a component.
File selection, SSD caching, cloud transfers, source cleanup, disk formatting,
and process supervision remain external operations.

## Browse and verify

`archive browse` displays committed archives from the catalog without a tape.
Use `stage ls` for selections that have not been written. `archive browse --tui`
currently opens a small numbered terminal menu for one archive; it is not a full
backup interface. Command names have one spelling, with no aliases.
There is no command to extract archived files yet. `tape reindex` rebuilds the catalog,
not a restored filesystem tree.

`tape verify` checks a whole tape by default. Give one full archive ID to check
one archive, or two IDs to check the inclusive contiguous range between them.
The IDs must have unique committed replicas on the loaded tape. The preview
lists every selected archive, its logical object range, boundary checks and EOD
check. It prompts once; `--yes` (`-y`) skips the prompt and `--dry-run` (`-n`)
shows the plan without moving the tape. Non-interactive execution needs `--yes`.

```sh
taper tape verify --device /dev/sg1 --dry-run FIRST_ID LAST_ID
GOMEMLIMIT=10GiB taper tape verify --device /dev/sg1 --memory 8589934592 --yes FIRST_ID LAST_ID
# Exact scope and results for scripts:
taper tape verify --device /dev/sg1 --dry-run --json FIRST_ID LAST_ID
```

Each verification uses one locate and one forward read. The reader is bounded
at the selected last filemark; when that is the last committed archive, it makes
one additional read to require EOD. It never reads on into an unexpected tail.
Only a completely successful range updates its selected replicas. Failures stop
without retry. Full verification also checks the public label at BOT.

`tape reindex` remains a complete BOT-to-EOD recovery pass. Neither command runs
automatically after writing. Use an appropriate read pool; on the qualified host,
8 GiB with a 10 GiB Go runtime soft limit works. Terminal progress displays rate,
elapsed time and a bounded progress bar; redirected progress is periodic text.
Tape labels use an IBM-style color strip when the terminal supports color;
`NO_COLOR=1` disables it. JSON always contains plain strings.

## Format and recovery limits

Each replica fits one tape and has no content dependencies on another archive.
MAM cartridge memory identifies initialized media during append, so an append
never falls back to rewinding and reading the tape label.

Tape sidecars preserve paths and lengths. Content hashes are recomputed when
reading and checked through the archive digest. File mtimes, original source
information and selection rules live in the external catalog. Permissions,
ownership, empty directories and sparse-file layout are not preserved by v1.

Keep the encrypted catalog export and tape recovery key separately. The catalog
passphrase and tape key have different roles: changing the former does not
change the latter, and recovering an encrypted tape without its key is impossible.

- [Command reference](docs/COMMANDS.md)
- [Operation and recovery](docs/OPERATIONS.md)
- [Tape format v1](docs/FORMAT.md)
- [Validation and measured limits](docs/VALIDATION.md)
- [Proposed CLI/TUI design](docs/CLI-DESIGN.md)
