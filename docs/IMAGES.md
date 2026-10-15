# Replica sets and sparse images (v1)

A ReplicaSet owns one ordered archive stream and one encryption/compression
policy. Each physical cartridge has its own ID, hardware serial, label, note,
capacity observations and committed prefix. A disk image is a sparse cache of
that stream, not another cartridge. SQLite never stores image paths or coverage.

## Native tape identity

The existing on-tape format stays version 1. MAM 0803 and the MEDIA record at
LBA 0 use the ReplicaSet ID. The MEDIA label/serial describe that cartridge.
In every BEGIN/COMMIT, `media` is the set ID and `replica` is the archive's
immutable `stream_id`. DATA and both controls are copied without recompression;
all batch records at LBA >= 2 are identical on every member and in the image
(after image decryption). Each cartridge gets its own MEDIA record and MAM label.

Each archive occupies BEGIN, DATA records, COMMIT and one native filemark. New
plans append immediately after the previous archive. Stored positions are
explicit and authoritative; any recorded inter-archive gap contains native
filemarks and is reproduced on other members. Conversion must never renumber
positions on an existing tape.

The next archive has `seq = previous.seq + 1`, `data_pos = begin_pos + 1`,
`blocks = 1 + (logical_bytes - 1) / 1048512`, `commit_pos = data_pos + blocks`,
and `end_pos = commit_pos + 2`. A physical member can receive only its next
complete archive. An interrupted write is an unresolved tail, never a resumable
partial archive or permission to overwrite it.

## Disk representation

One `.img` file contains:

| Offset | Contents |
|---|---|
| 0–4095 | Superblock: `TAPERIMG`, version 1, MaxPos, replica/key identity, CRC32C |
| 4096–1048575 | Two bits per logical object: absent=00, record=01, filemark=10 |
| `(LBA + 1) << 20` | One fixed 1 MiB slot containing the original variable-length record |

The bitmap addresses 4,177,920 logical objects. Unwritten slots and unused record
tails are sparse. MaxPos is an exclusive logical high-water mark, not a physical
EOD observation or a measurement of disk/tape free capacity. A missing object
below MaxPos is an unavailable range, not EOF or a zero-filled file.

The 4096-byte superblock uses little-endian fields: magic at 0 (8 bytes), version
at 8 (4), CRC32C at 12 (4, zeroed during calculation), MaxPos at 16 (8), replica
UUID bytes at 32 (16), and NUL-padded key ID at 48 (32). Remaining bytes are zero.

Encrypted images use the replica's 32-byte key with XChaCha20. The 24-byte nonce
is `replica UUID || uint64_le(LBA)`. Only bytes 44–47 (stored payload length)
remain plaintext. Ciphertext occupies exactly the original record length;
header/payload CRCs are checked after decryption. This is not an AEAD format.
Records and stream identities are immutable, including across interrupted stage
writes; changing plaintext under an existing key/LBA nonce is prohibited.

Slot bytes are written first. At a bounded checkpoint (64 records, a filemark,
Flush or Close), fdatasync completes before bitmap/superblock publication and
fsync. Pending coverage may be served in the same process. No per-record header
rewrite or SQLite checkpoint is needed. Invalid superblocks/headers fail closed;
an interrupted unpublished tail must be inspected locally before retrying.
Hole punching first durably withdraws bitmap coverage, then uses
`FALLOC_FL_PUNCH_HOLE | FALLOC_FL_KEEP_SIZE`, preventing published entries from
pointing to discarded data after a crash. Images hold an exclusive writer lock
or shared read lock. Open/close never silently reformats an existing file.

## Workflow

```sh
taper replica create cold --key-file replica.key
taper replica add cold --device /dev/sgN --label 123456L6 --note 'Shelf 2'
taper stage add --source /stable/source
taper stage plan --replica cold --name first
taper stage write --image /cache/cold.img
taper tape sync --device /dev/sgN --from /cache/cold.img
```

`stage write` accepts an image, a device, or both. With both, it prepares the
whole image archive before streaming that same encoding to tape; source files
are read once. A committed image archive advances the set stream but creates no
physical replica. The stage clears immediately after successful completion.
A reserved stream with an interrupted output remains blocked against restaging.

Insert another registered member and repeat `tape sync --from`. Default selection
is its missing committed suffix. `--archive FIRST[-LAST]` must start at that
member's next missing batch; all selected image batches must be complete.
Each successful filemark commits a separate physical replica in SQLite. Existing
prefixes are not reread. Neither verification nor ejection is automatic.

`tape sync --to` opens SQLite read-only and captures the selected available
archives into an image. It reads once from the first incomplete image archive
through the last incomplete one, validating controls, DATA CRCs, content digests
and filemarks. Cached intervening archives are passed through in that same read.
A fully cached selection is a no-op. No capacity or verification observations
are written to SQLite by this operation.

`--prune` applies only to `--from` and releases image batches committed on every
current member. Membership alone is not proof of a prefix: per-replica positions
and archive IDs must match. A missing member prefix prevents pruning. Pruning
uses durable-write evidence, not an assertion that every replica was read-verified.

## Budgets and settings

A set's write budget is the smallest member's remaining budget after subtracting
reserve and every committed archive that member still owes. Use exact encoded
bytes when available, otherwise the format's uncompressed upper bound. Unknown
capacity, early warning, unhealthy state or incomplete writes contribute zero.
A set without physical members has zero planning budget. No compression gain is
assumed; reported device observations remain separate from the calculated budget.

MAM 0401 supplies the hardware serial. Binding uses an existing matching taper
MAM label, otherwise barcode 0806, `--label`, then serial. `--note` is independent
of the label. Members inherit the set key and hardware-compression preference.
Registration changes no drive mode and writes no tape records.

## Read-only FUSE

```sh
taper replica mount cold /mount/point --image /cache/cold.img
taper replica mount cold /mount/point --device /dev/sgN --image /cache/cold.img
taper replica mount cold /mount/point --device /dev/sgN
```

All modes use one read-through image path. Device-only mode uses an unlinked
sparse temporary file in TMPDIR; choose a filesystem with sufficient space.
Without a device, missing requested data returns ENODATA and corruption returns
EIO. A hole is never returned as successful EOF or zero bytes. Stat/readdir use
the read-only catalog and do not move tape. Original files are exposed; indexed
members inside compressed archives remain browsing metadata.

One selected archive is mounted directly at the root; multiple archives use
`seq_shortid_name` directories. File offsets map directly to DATA LBAs using
1048512-byte raw payloads. A bounded decoded-block LRU serves repeated reads.
Overlapping misses share the persistent image; a single scheduler serves finite
forward-LBA rounds. It stops acquiring on error and does not retry a failed pass.

`--min-blocks M` counts DATA records. Find the contiguous missing gap containing
the request, choose at least M records (or the whole shorter gap), and absorb
any remainder shorter than M on either side. Do this independently inside each
archive; no padding read crosses a cached or archive boundary. Native boundary
records may accompany a DATA window when its head/tail is reached. Producer
read-ahead is bounded at the planned end.

The Linux server speaks `/dev/fuse` directly with no FUSE library or subprocess.
Mount permission must be supplied by the operator, for example in a user/mount
namespace or by elevating taper externally. There is no internal fusermount,
pkexec or worker. An actual kernel mount is qualified separately from wire tests.

## Catalog conversion

`scripts/migrate_v1.sql` is an explicit offline conversion, not runtime code.
Start from an authenticated backup and use a candidate database. It maps old media
IDs to set IDs and old physical replica IDs to immutable batch stream IDs while
preserving actual positions and all existing physical replica rows. It rejects
unknown compression, ambiguous streams, unfinished plans and invalid geometry.
Review abandoned operational rows before conversion. Compare retained values,
foreign keys and integrity before installing the candidate. Schema stays 1.
