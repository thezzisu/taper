# Operations

## Device access

Use the Linux SG node for the intended sequential-access drive. The CLI opens it
directly under the current account, normally with access through the `tape` group.
It never starts a privilege helper. If a host requires elevation, the operator
chooses it outside taper. Long commands can run inside tmux.

`/dev/sgN` numbering can change. A system-managed persistent symlink to an SG node
is accepted, but taper does not install udev rules or discover a drive by serial.
The common `/dev/tape/by-id/*-nst` links point at the `st` driver, which is not a
taper backend. Drive identity and cartridge identity are different: MAM serial,
barcode and taper media ID describe the loaded cartridge.

`tape probe` queries capabilities, cartridge capacity and TapeAlert flags.
`replica add` registers that cartridge in SQLite using device/MAM queries. These
commands do not read tape records, seek, erase, or write a tape label. Members inherit the replica set's hardware-compression preference without changing the drive. It can succeed on occupied media; the first write refuses an uninitialized
tape unless its empty check succeeds.

On the first write to a new tape, taper performs that empty check at BOT, writes
the public label and MAM identity, and then starts the archive. Later appends use
MAM identity and locate the end of data. Missing or invalid MAM stops the append;
it never triggers a fallback read from BOT. Device I/O errors are not blindly retried.

## Choose, plan, write

1. Prepare a stable local source and explicit include/exclude rules. Keep complete
   related file groups together when choosing what fits on a cartridge.
2. `stage add` records paths and metadata. `stage rm` removes them from the stage.
3. `stage plan` reads file contents as needed to build hashes, record-byte budgets,
   duplicate information and tape recommendations. Reuse a valid existing plan;
   `--no-cache` explicitly requests rehashing.
4. `stage write` checks the source identity and metadata, pre-fills the write pool,
   and appends to the drive supplied with `--device`. It recomputes content hashes
   while supplying the write; source contents must remain unchanged.
5. Export the catalog after a committed write. Check `verified` separately from
   `committed` before deciding whether to release source data.

Keep selected files unchanged from `stage add` through planning and all replica
writes. Taper does not require a read-only mount or create a snapshot. Source
identity, metadata and streaming hashes detect changes; the operator supplies a
stable source.

Replica-set budgets use the smallest member's saved capacity/health budget after
subtracting every committed batch that member still lacks. Missing measurements,
unhealthy members and unresolved tails produce zero budget. Physical writes
recheck the loaded cartridge. Budgets assume no hardware compression gain and
reserve the larger of 1% of the native limit or 2 GiB.

`reported_total_bytes` and `reported_remaining_bytes` are the device's exact
integer counters converted from MiB (2^20 bytes). Raw MAM and READ POSITION
responses, logical position, flags and the query time are retained in immutable
`media_samples` rows. The latest measurements are also available offline. They
describe that observation, not later writes by another program.
The counters' resolution remains 1 MiB; conversion to bytes does not turn them
into a byte-resolution measurement of the tape's physical free space.

`write_budget_bytes` is separate: native limit minus reported consumed bytes,
then minus the explicit reserve. It never discounts bytes already consumed.
The current SG backend recognizes LTO-6 density 0x5a and its documented
2,500,000,000,000-byte native limit. Other densities have no budget until their
native limit is supported explicitly. An end-of-partition early warning prohibits
further archive writes, regardless of the remaining-capacity counter. Incomplete
writes or unknown capacity evidence produce no offline write recommendation.
The budget is a conservative admission rule, not a promise of the exact physical
end-of-tape location. A committed write invalidates its pre-write capacity sample
in the same catalog transaction; only a saved post-write sample restores it.

`stage plan` reports exact `encoded_bytes` and `data_records` for the uncompressed
archive stream, including its BEGIN and COMMIT records. Filemarks are logical
objects, not invented byte costs. Hardware framing/ECC costs are reflected in
device capacity consumption; the reserve remains explicit. Software zstd can
reduce encoded size and never increases a record's stored length.

Each replica records confirmed host-to-drive bytes and records, DATA block count,
and observed logical end position. Counters are accumulated in memory and saved
after the stream, not with a SQLite transaction per record. If a write fails,
confirmed counters exclude the ambiguous failing command; the full sense and
transfer residual are retained separately. A crashed process or historical record
without sufficient evidence has NULL counters, never a guessed zero. Verification
and reindexing measure encoded record lengths in their single sequential pass.

Tapes with an unresolved `writing` or `failed` replica are excluded from
recommendations and refuse further appends. Their tail requires explicit
operator investigation; neither registration nor a retry erases it. Sense errors
retain the command, deferred-error flag, information field and transfer residual
for diagnosis. An EOM response is not automatically retried or treated as a
successful archive commit.

A physical replica never spans tapes. A complete batch can be synchronized to
any number of registered members from one image. Failed writes retain a blocked
tail; there is no automatic retry, split or overwrite.

## Continuous I/O

The reusable pool accepts byte counts from 8 MiB through 64 GiB; the CLI default
is 1 GiB. This bounds the pool, not total process RSS. Write prefill happens before
tape writes. If the source falls below the low watermark, a synchronous flush
with zero filemarks precedes a deliberate refill pause. Unplanned supply stalls
terminate the unfinished replica. A finite pool cannot fix a sustained source
rate below the drive's consumption rate.

Use prior measurements for an unchanged source and I/O path. Measure locally when
the path, storage layout or workload materially changes. Throughput qualification
uses external tools; taper has no source-speed test or preparatory benchmarking
pass. Software zstd is per record and optional; drive compression can change
consumption rate. Neither compression ratio nor a large pool proves that physical
backhitches cannot occur.

The writer applies the registered hardware compression preference before writing
records. It queries the SSC Data Compression mode page, changes DCE only when
needed, then queries the page again to check the result. A failure stops the write.
These are drive-setting commands, not tape-record reads or seeks. Decompression,
the drive's compression algorithm and buffered mode are preserved.

Hardware compression defaults to on and software zstd to off. They can be enabled
independently; taper does not automatically switch one when the other is selected.
Data already compressed as 7z, JPEG or video generally gains little from another
layer. Highly compressible input may need a higher host supply rate to sustain
tape motion. Native budgets continue to assume no compression gain.

`tape verify` and `tape reindex` share the bounded record pool, decoder and hash
parser. Verification plans from the catalog before moving the tape. It checks
cartridge identity, unique archive-to-tape associations, ordered bounds, every
intervening committed archive and consistent positions. An omitted `--archive` selects the whole tape. `--archive ID` selects one archive;
`--archive FIRST-LAST` selects an inclusive range, with unique prefixes accepted. Reversed IDs, missing positions, overlaps or unresolved
writes intersecting the range are rejected.

The English preview displays the tape label, cartridge serial, device and full
archive IDs with inclusive object ranges. Gray rows show label, gap and EOD
checks. The JSON plan uses explicit start and exclusive end positions.
`--dry-run` (`-n`) only queries MAM/device identity and the catalog; it performs
no record reads, positioning, key changes or catalog sampling writes. It takes
precedence over `--yes` (`-y`). The default confirmation is `Continue? [y/N]`.
Non-interactive callers must supply `--yes`; it never bypasses identity or bounds
checks. The plan and identity are checked again before the first locate.

Verification locates once to the selected BEGIN (BOT for a whole tape), then
reads forward. Inter-archive gaps must contain filemarks. Its producer enforces
the scope limit, including read-ahead. A range containing the last committed
archive reads exactly one additional object and requires EOD there; trailing
records or an additional filemark fail this check. An unrelated blank-check
sense is not accepted as EOD. Other ranges stop after the selected durable
filemark without touching the next archive. There is no automatic retry.

Content, boundary and EOD checks must all pass before verification timestamps and
observed byte/record counts update in one transaction for the selected replicas.
Reindex remains a whole-tape recovery scan that can recover later intact archives
past a damaged one. Catalog writes follow streaming, never per record.

The measured read setting on the qualification host is:

```sh
GOMEMLIMIT=10GiB taper tape verify --device /dev/sg1 --memory 8589934592 --yes --archive FIRST_ID-LAST_ID
```

The runtime limit is soft. Do not choose a 64 GiB read pool merely because a
64 GiB write pool worked. See [validation](VALIDATION.md) for evidence and limits.

SG_IO temporarily blocks asynchronous signals on the issuing OS thread to avoid
ambiguous syscall restarts submitting a command twice. Cancellation can wait for
the in-flight command to return; an operation is never automatically restarted.

## What verification establishes

Verification checks frame CRCs, record order, sidecar layout, file counts, content
SHA-256 hashes, BEGIN/COMMIT agreement and the durable filemark. It recomputes the
archive digest from the decoded paths, sizes and content hashes and compares it
with the digest stored on tape. `tape verify` also compares that digest and the replica
layout with SQLite. A range result describes only its selected archives, not
unread archives elsewhere on the tape.

This is not a fresh comparison against the original source disk and does not
validate the contents of an encrypted RAR or other nested archive. Source mtimes
and permissions are not part of the on-tape digest.

Schedule verification to avoid unnecessary rereads. For the operating policy of
one write and at most one read pass, an interrupted physical scan counts; do not
follow it with another scan, reindex, or spot reads. Taper does not store or enforce
a lifetime read-pass quota: that scheduling belongs to the operator. Investigate
failures with saved logs and local fixtures before deciding what to do next.

## Catalog and key recovery

Only the current catalog schema (version 1) is accepted. `catalog init` creates
it; opening a catalog does not create missing tables or migrate older versions.
Import also checks the version before creating a destination. Version mismatches
are errors. There is no migration command in this release.

`catalog export` creates an authenticated encrypted snapshot containing
`catalog.db`, `catalog.db.unlock` and `catalog.db.mac`. Keep all three together,
outside the source disk. Use `catalog import --source EXPORT_DIR --catalog NEW_DB`
to create a catalog at a new destination. Export uses SQLite's backup API rather
than copying a live database file and overlooking its journal.

The passphrase wraps a random database key using Argon2id and XChaCha20-Poly1305;
the SQLite VFS uses Adiantum. `catalog rekey` changes the passphrase envelope.
It does not change any tape's independent hardware-encryption key.

If the catalog is lost, create a new catalog and run `tape reindex` with the saved tape
key. That reads the whole tape once and imports complete archives. It can rebuild
paths, content hashes, packed extents and replicas, but not catalog-only source
mtimes, selection history or hardware compression preference. Newly reconstructed
tapes have an unknown compression preference; explicitly register a preference
before writing. Hardware
decompression works independently of this preference. There is currently no
file-extraction command.
