# taper

A Go archive for manually loaded LTO tapes. Files share packed 1 MiB records;
a separate encrypted SQLite catalog stays searchable while tapes are offline.
Adjacent path/length sidecars allow a sequential catalog rebuild.

## Build and test

Go 1.26+, Linux amd64/arm64. SQLite runs in Go without a C SQLite installation.
The tests use a tape simulator and never open a real drive.

```sh
go build -o taper ./cmd/taper
go test ./...
go test -race ./...
go vet ./...
```

## Use

```sh
taper catalog init
taper key generate --name TAPE001 --key-file /private/TAPE001.key --keycard /private/TAPE001.keycard
# Save/print the recovery card. Registration only reads the drive and cartridge memory.
taper media init --device /dev/sg1 --name TAPE001 --key-file /private/TAPE001.key --key-backed-up

taper stage add --source /mnt --rules /private/rules.json .
taper stage ls
taper stage rm unwanted/path
taper stage plan --name archive
taper stage write --device /dev/sg1 --memory 68719476736

taper archive list
taper browse --archive UUID --tui
GOMEMLIMIT=10GiB taper verify --device /dev/sg1 --memory 8589934592
taper catalog export --dest /private/catalog-snapshot
```

A hidden prompt unlocks the catalog; `--pass-file` supports unattended operation.
The default catalog is `~/.local/share/taper/catalog.db`. Flags precede path arguments.

The staging area belongs to the catalog and has one source root. `add` records
selected regular files; adding a directory again refreshes that selection.
`rm` only unstages paths; `rm .` clears the stage, allowing another source root or
rule set. Source files are never deleted. `ls` works without the source mounted.

`plan` hashes exactly the staged files, deduplicates identical contents, calculates
native capacity and recommends tapes. It requires a read-only source or an explicit
`--frozen` acknowledgment. Source changes require another `add` and `plan`.
Hash checkpoints allow interrupted plans to resume; `--no-cache` forces rehashing.
Repeated plans keep the same identity until the staging area changes.

`write` requires a current plan. It creates an archive only after a durable tape
commit. Failed writes retain the stage. `plan --copies N` requires N distinct tape
copies: the stage remains until they exist. `archive list` shows stored archives,
never unwritten plans. File metadata remains available through offline `browse`.

Rules are explicit; nothing is excluded by default:

```json
{"include": [], "exclude": ["path/to/exclude/**"]}
```

`**` matches directory components; `*` and `?` match within a component. A nonempty
include list is a whitelist, and exclusions subtract from it. Rules belong to the
current stage. Selection decisions and cleanup are outside taper.

## Storage and recovery

Files share packed 1 MiB records without per-file padding. Within an archive,
identical files share an adjacent extent. Replicas share one catalog
file tree; cross-archive hashes provide duplicate suggestions without creating
content dependencies. Each replica fits one tape. There is no automatic splitting.

`media init` only queries and registers the cartridge. An empty tape gets its
public format label during its first `stage write`, after source prefill.
Occupied, unformatted tapes are refused; taper exposes no erasure operation.
`media list` is offline and `media probe` queries device capabilities and capacity.

Writing pre-fills a bounded pool; a selection that fits is completely buffered
before tape writes. Low water triggers a synchronous, zero-filemark flush and
a deliberate refill pause, then another sustained write burst. This preserves
logical record positions and avoids repeatedly waiting on an empty buffer. `stage write --probe-mbps N` can first check the
complete source at a specified consumption rate and refuse starvation.
`--zstd` enables independent per-record compression; otherwise bytes go to the
configured drive compression. Native budgets do not assume compression savings.
The default maximum supply wait is two seconds. Source throughput, buffer size
and drive speed matching must be qualified for the intended workload.

Both `verify` and `reindex` read the entire loaded taper tape from BOT to EOD.
They use a bounded read-ahead pool (`--memory`) with refill watermarks, check record CRCs, sidecars,
content hashes and commit boundaries, and do catalog writes after tape reading.
Parser lookahead stays in memory. A corrupt archive is reported while later
records are still read; an unrecoverable device I/O error stops the scan.

`verify` compares all recorded replicas with the catalog. `reindex` rebuilds
complete archives and replicas after catalog loss. A matching recovery key is
needed for encrypted tapes. Neither command has archive or path filters.
There is no automatic full readback after a write.

Each encrypted tape has an independent hardware key. The encrypted catalog stores
it by default; `--no-save-key` requires a private key file for later operations.
Recovery cards are necessary if the catalog is lost. `catalog export` creates an
authenticated encrypted snapshot, `catalog import` restores one, and `catalog rekey`
changes the catalog passphrase without changing tape keys.

Native I/O uses a PolicyKit-authorized typed SG_IO worker. Tape erasure, source
cleanup, filesystem formatting, and migration utilities are outside this program.
The SQLite catalog records media identity/capacity/health, archive file trees and
hashes, exclusions, source identity, replica positions/states and optional keys.
Permissions and empty directories are omitted; timestamps are catalog metadata.

See [FORMAT.md](docs/FORMAT.md) for the tape format,
[OPERATIONS.md](docs/OPERATIONS.md) for operation and key recovery, and
[VALIDATION.md](docs/VALIDATION.md) for measured tests and their limits.
