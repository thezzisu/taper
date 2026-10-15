# taper

Packed archives for manually loaded LTO tapes, with an encrypted SQLite catalog
that stays searchable while the tapes are offline. Replica sets share one immutable stream across physical
cartridges and sparse image caches. Small files share
1 MiB records, and complete batches can be prepared once then synchronized to
multiple cartridges.

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
taper replica create TAPE001 --key-file /private/TAPE001.key
taper replica add TAPE001 --device /dev/sg1 --key-file /private/TAPE001.key

taper stage add --source /data/snapshot --rules /private/rules.json .
taper stage ls
taper stage rm unwanted/path
taper stage plan --replica TAPE001 --name photos
# If plan reports a locked container, enter its password without echoing it.
taper stage unlock --remember 'backup.7z'
# This example allocates a 1 GiB write pool. Size it for the measured source rate.
taper stage write --device /dev/sg1 --memory 1073741824 --yes

taper archive ls
taper archive browse ARCHIVE_ID
taper catalog export --dest /private/catalog-snapshot
```

Keep source files unchanged from `stage add` through planning and all replica
writes. Taper checks source identity, metadata and streaming hashes; it does not
freeze files or create filesystem snapshots.

A hidden prompt unlocks the catalog. `--pass-file` reads a local passphrase file
for unattended use. The default catalog is `~/.local/share/taper/catalog.db`;
`TAPER_CATALOG` overrides it. `TAPER_PASS_FILE` supplies the passphrase file
through the environment, otherwise an existing `~/.local/share/taper/session.pass`
is used. Passphrase files must be owner-only (mode 0600), and an unsafe file
produces an explicit error.
Keep credentials and catalog exports outside the source tree. Flags may precede
or follow positional paths; `--memory` takes an integer byte count. Output is
human-readable by default. Use `--json` for automation; results go to stdout and
progress goes to stderr. Redirected output contains no terminal styling.
The catalog uses schema version 1. Opening another version fails; taper never
upgrades it or creates missing tables implicitly.

Hardware compression defaults to on when creating a replica set; use
`replica create NAME --hardware-compression=false` to disable it. Each physical
write applies the set preference. Optional software zstd defaults off and remains
independent. Planning assumes no hardware compression gain.

## Staging and tape choice

One catalog has one staging area with one source root. `stage add` records files;
adding a directory again refreshes that selection. `stage rm` changes the
selection, and `stage rm .` clears it. Neither deletes source files.

`stage plan` binds the selection to a replica set, hashes and deduplicates it,
indexes user archives and checks the smallest member budget after accounting
for missing physical replicas. It assigns an immutable stream ID and batch positions.
Repeating a current plan reuses it; interrupted hashing resumes from checkpoints.

Deep indexing follows packaging layers: `backup.zip!/game.rar!/OST.zip!/track.flac`.
Only conventional archive suffixes enter this recursion. Programs (including
self-extracting EXE files), APK/JAR packages, installers, documents, media and disk
images remain ordinary files at every level. Taper does not inspect arbitrary
files for archive signatures. It preserves all original bytes regardless of
whether it builds an inner tree; these rules do not exclude files from backup.

If an archive is encrypted, planning records its visible state and continues.
Use `stage unlock PATH` with a hidden prompt or `--password-file`; add
`--remember` only when the password should be retained in the encrypted catalog.

`stage write` accepts `--image`, `--device`, or both. With both, it completes the
image first and streams those same records to tape. A successful complete batch
clears the stage immediately. Insert another member and use `tape sync --from`
to copy its missing suffix. `tape sync --to` fills a sparse image without changing
SQLite. `--prune` releases only image batches committed on every member. A durable
physical replica is distinct from one that has been read-verified.

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

`archive browse ARCHIVE` displays a committed archive from the catalog without a tape.
`ARCHIVE` may be a full ID, a unique ID prefix, or the exact archive name. Use
`stage ls` for selections that have not been written. `archive browse ARCHIVE --tui`
currently opens a small numbered terminal menu for one archive; it is not a full
backup interface. Command names have one spelling, with no aliases.
`replica mount` exposes original archived files through read-only FUSE, from an
image, a device with an image, or a device with a temporary sparse cache.
`tape reindex` rebuilds catalog metadata in one full tape pass.

`tape verify` checks a whole tape by default. Use `--archive ID` for one archive or `--archive FIRST-LAST` for an inclusive
contiguous range; unique hex prefixes of at least six characters are accepted.
The IDs must have unique committed replicas on the loaded tape. The preview
lists every selected archive, its logical object range, boundary checks and EOD
check. It prompts once; `--yes` (`-y`) skips the prompt and `--dry-run` (`-n`)
shows the plan without moving the tape. Non-interactive execution needs `--yes`.

```sh
taper tape verify --device /dev/sg1 --dry-run --archive FIRST_ID-LAST_ID
GOMEMLIMIT=10GiB taper tape verify --device /dev/sg1 --memory 8589934592 --yes --archive FIRST_ID-LAST_ID
# Exact scope and results for scripts:
taper tape verify --device /dev/sg1 --dry-run --json --archive FIRST_ID-LAST_ID
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
- [Replica sets, sparse images and FUSE](docs/IMAGES.md)
