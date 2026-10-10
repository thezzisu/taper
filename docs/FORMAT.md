# Taper on-media format v1

All tape writes are single variable-length logical records. Logical object
addresses include filemarks; hardware-compressed byte positions are never used.
Only partition zero is supported. Maximum record length is 1,048,576 bytes.

## Record header (64 bytes)

Integers are little-endian. Reserved bytes must be zero when writing.

| Offset | Length | Meaning |
|---:|---:|---|
| 0 | 8 | ASCII `TAPERBLK` |
| 8 | 2 | Version, 1 |
| 10 | 1 | Kind: MEDIA=1, BEGIN=2, DATA=3, COMMIT=4 |
| 11 | 1 | Codec: raw=0, zstd=1 |
| 12 | 4 | CRC32C of the 64-byte header with this field zero |
| 16 | 16 | Batch UUID, or media UUID for MEDIA |
| 32 | 8 | Zero-based DATA sequence; zero in control records |
| 40 | 4 | Uncompressed payload length, maximum 1,048,512 |
| 44 | 4 | Stored payload length |
| 48 | 4 | First sidecar start in raw payload; `0xffffffff` if none |
| 52 | 4 | CRC32C of uncompressed payload |
| 56 | 8 | Reserved |

CRC32C uses the Castagnoli polynomial. A zstd payload is one independently
decodable frame. Decoding is limited to the advertised bounded raw length.
All DATA records except the final one contain exactly 1,048,512 raw bytes.

## Packed payload stream

The concatenated uncompressed DATA payloads contain consecutive groups:

```
uvarint metadata_body_length
  uvarint content_length
  uvarint path_count
  repeated: uvarint path_byte_length, UTF-8 path bytes
content_length raw content bytes
```

Uvarints use Go's unsigned base-128 encoding, at most 10 bytes. Each group has
at least one path. Limits: metadata body 256 MiB, 1,000,000 aliases, 65,536 bytes
per path. No alignment or per-file padding exists. Sidecars can cross records.
Paths are relative, `/` separated, case-sensitive UTF-8; empty components,
`.`/`..`, leading slashes, NUL and literal backslashes are rejected. Native Linux
`%`, backslash, control characters and invalid UTF-8 bytes are escaped as `%XX` without Unicode folding.

Duplicate contents share a group. There are no content object IDs, inter-group
references, or cross-batch dependencies. Nonempty directories follow from paths;
empty directories are omitted. Contents are normal byte streams, including
expanded sparse regions. Filesystem permissions and special files are outside v1.

## Controls and durable boundary

Control payloads are compact UTF-8 JSON. MEDIA contains `id`, `name`, cartridge
`serial`, `key_id`, and `version`. It is unencrypted, followed by one filemark.
Cartridge user-text MAM additionally holds `taper1:` followed by the JSON array
`[media_id,label,key_id]`; no file paths or secrets are stored in MAM.

BEGIN and COMMIT contain `batch`, `replica`, `media`, `name`, `logical`, `files`,
`blocks`, and `digest`. BEGIN has blocks=0. COMMIT repeats the fields with the
actual DATA record count, followed by one non-immediate filemark. Files are
recoverable without a catalog by sequentially decoding the group stream.

The SHA-256 manifest digest hashes each path in lexical order as:
`path-byte-length:path:content-size:content-sha256-hex\n`.
Per-file hashes are not stored in sidecars. The fixed-size batch digest validates
a full rebuild without creating a centralized directory area.

Within a replica, DATA logical object addresses are `data_start + sequence`;
there are no intervening filemarks. SQLite extents refer to uncompressed stream
offsets. For offset O, record sequence is O/1,048,512 and payload offset is the
remainder. These remain stable under either hardware or per-record compression.

An interrupted append is not a committed replica. No tail truncation or rollback
is allowed. Retry appends a new replica. A flush ambiguity is reported, and
reindex can reconcile a COMMIT/filemark present on media after a catalog failure.
