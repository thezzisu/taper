# taper

A Go archive for manually loaded LTO tapes. Files are packed together into 1 MiB
records; a separately encrypted SQLite catalog stays searchable while tapes are
offline. Minimal path/length sidecars allow a sequential catalog rebuild.

## Build and test

Go 1.26 or later, Linux amd64/arm64. The SQLite engine is compiled Go; no C SQLite
or SQLCipher installation is needed.

```sh
go build -o taper ./cmd/taper
go test ./...
go test -race ./...
go vet ./...
```

`docs/FORMAT.md` specifies the on-media format. `docs/OPERATIONS.md` covers
real-drive qualification, key recovery, failure states, and the HDD migration.
`docs/VALIDATION.md` records local measurements and their limits.

## Offline demonstration

The example passphrase is **only for synthetic tests**. Catalog passphrase files
and recovery cards should be private. Omit `--pass-file` for a hidden terminal
prompt. Input directories must be read-only or explicitly frozen.

```sh
mkdir -m 700 /tmp/taper-demo
printf 'synthetic-test-password\n' > /tmp/taper-demo/pass
chmod 600 /tmp/taper-demo/pass
./taper catalog init --catalog /tmp/taper-demo/catalog.db --pass-file /tmp/taper-demo/pass
./taper sim create --sim /tmp/taper-demo/test.tape --capacity 10737418240
./taper media init --catalog /tmp/taper-demo/catalog.db --pass-file /tmp/taper-demo/pass \
  --sim /tmp/taper-demo/test.tape --name DEMO
./taper batch scan --catalog /tmp/taper-demo/catalog.db --pass-file /tmp/taper-demo/pass \
  --source /path/to/synthetic/files --name demo
# Use the returned batch UUID below.
./taper batch plan --catalog /tmp/taper-demo/catalog.db --pass-file /tmp/taper-demo/pass --batch UUID
./taper backup --catalog /tmp/taper-demo/catalog.db --pass-file /tmp/taper-demo/pass \
  --sim /tmp/taper-demo/test.tape --batch UUID --frozen --memory 67108864
./taper browse --catalog /tmp/taper-demo/catalog.db --pass-file /tmp/taper-demo/pass --batch UUID --tui
./taper restore --catalog /tmp/taper-demo/catalog.db --pass-file /tmp/taper-demo/pass \
  --sim /tmp/taper-demo/test.tape --batch UUID --dest /tmp/taper-demo/restored
./taper verify --catalog /tmp/taper-demo/catalog.db --pass-file /tmp/taper-demo/pass \
  --sim /tmp/taper-demo/test.tape --batch UUID
```

Each batch is an immutable file tree. Same-content paths in a batch share one
adjacent content extent. A batch can have replicas on multiple tapes; v1 creates
another replica from the still-present frozen source using the same batch UUID.
Cross-batch duplicate hashes are indexed, but contents are written independently.

## Rules and selection

```json
{
  "include": [],
  "exclude": ["Tmp/**", "$RECYCLE.BIN/**", "**/*.downloading", "**/*.vmem"]
}
```

Rules use `/` separators. `**` matches directory components, `*` and `?` match
within a component. A directory rule includes its descendants. Nonempty include
rules define a whitelist; exclusions subtract from it. Nothing is excluded by
default. Rules are chosen externally; `batch scan --rules rules.json` freezes the
resulting manifest. The program does not guess what counts as garbage. Unsupported entries
must be explicitly excluded or materialized into regular files.

`batch check` inspects names, types and sizes without hashing or opening the
catalog. `batch scan` saves completed hashes and per-file checkpoints in the
encrypted catalog when the source is read-only or acknowledged frozen. Resume
requires matching filesystem, file identity, size and modification/change times;
`--no-cache` forces a fresh hash. Replaced/unmounted roots and empty inputs fail.

## Supply tests

Use `batch probe --batch UUID --memory 68719476736 --probe-mbps 160` to simulate
a consumer at 160 MB/s using the actual source, hashing, packing and bounded
pool. It reports prefill time, actual consumer wait, queue depth and bytes.
The test never opens a tape. For a writable test input, acknowledge `--frozen`.
`batch spool --batch UUID --dest /nvme/stream.bin --memory 268435456` tests the
same producer against a regular file, checks free-space headroom, and only
publishes the raw diagnostic stream after source hashes and fsync succeed.

Writing pre-fills the ring before the first tape record. `backup --probe-mbps N`
can run a complete supply probe before starting and refuses a measured stall.
`--max-wait` defaults to 2 seconds and stops an uncommitted replica on source
starvation. A buffer absorbs transient deficits; a persistent source/consumer
rate mismatch requires a different operating point or faster staging storage.
Hardware speed matching and compression must be qualified on the actual drive.

## Real drives

Replace `--sim` with `--device /dev/sgN`. The process requests PolicyKit
authorization for a typed device worker, which validates a sequential-access
device. It uses SG_IO for both record I/O and positioning; it never opens the
rewinding `/dev/stN` device. `media probe` queries capabilities without locating or
reading tape records. Real tape support must be qualified on the actual drive
before treating it as a backup system.

Media initialization requires an empty tape and never erases an existing tape.
Encrypted media use independent random AES-256 hardware keys. Generate a key
first with `key generate --name LABEL --key-file /private/TAPE.key --keycard /private/TAPE.keycard`,
print/save the recovery card, then initialize with
`media init --name LABEL --key-file /private/TAPE.key --keycard /private/TAPE.keycard --key-backed-up`.
The key is retained in the encrypted catalog by default.
`--no-save-key` requires `--key-file` on subsequent operations. A supplied key
file contains 64 hexadecimal digits. Initialization fixes the tape's key policy.
The simulation models key identity and wrong-key errors; simulated tape contents
are plaintext test data and do not provide encryption.

Writing ends with a synchronous filemark. A committed replica has been accepted
and flushed by the drive. It is marked verified only by the separate `verify`
command or a complete reindex. No automatic readback occurs.
`batch status` counts distinct committed and verified physical tapes, and reports
historical content duplicates without creating cross-batch dependencies.

`restore --plan` works offline. `restore` accepts optional trailing path prefixes,
reads in increasing tape order, reuses shared records, checks SHA-256, and creates
independent files for aliases. Existing destination files cause a preflight error.
Restored names use the canonical archive spelling; `%`, literal backslashes, and
invalid UTF-8 bytes are reversibly escaped. Permissions, timestamps and empty
directories are not part of the portable tape sidecars.

## Catalog and recovery

The catalog and `.unlock` descriptor belong on a disk separate from the source.
`catalog export --dest /private/snapshot` checkpoints and copies the encrypted catalog,
unlock descriptor and authentication MAC. Use `catalog import --source /private/snapshot --catalog /private/new.db` to create a working catalog from a snapshot. `catalog rekey` changes the catalog passphrase,
without changing any tape key. Adiantum provides database confidentiality, not
per-page authentication; exported snapshots have an HMAC.

If the catalog is lost, initialize a new one and run `reindex` for each tape,
supplying its independent recovery key as necessary. Tape identities are repeated
in cartridge MAM and the initial record. Reindex sequentially hashes contents,
checks the batch digest and durable filemark, and registers replicas. Corrupt or
incomplete batches are reported; complete later batches are still imported.
Physical corruption can lose affected files: CRC and LTO ECC detect errors, and
additional replicas supply recovery copies.

No automatic erasure, filesystem formatting, source deletion, tape splitting,
or tape library control is exposed by the program. The first HDD migration is
an explicitly reviewed operation described in the operations document.
