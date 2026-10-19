# Validation

This describes the current single-process Linux SG_IO implementation. Tests use
local simulated media and never open the physical tape drive. Source inventories,
keys, passphrases and machine-specific operation scripts stay outside the repo.

## Local checks

```sh
go test ./...
go test -race ./...
go vet ./...
```

Existing tests cover packed record boundaries, duplicate paths sharing content,
raw and zstd records, explicit selection rules, source changes, interrupted writes,
independent replicas, encrypted catalog export/import and rekey, and sequential
whole-tape and range verification, and reindexing after corruption. Range tests
exercise middle-archive stops, inclusive intervals, inter-archive filemarks, exact
EOD checks, trailing data/filemarks, corruption, cancellation and wrong-cartridge
selection. The CLI simulation covers staging, offline browsing, JSON dry-runs,
combined short flags and confirmation rejection without a terminal.

Archive indexing tests run the embedded 7-Zip WASI module without a tape and cover
encrypted 7z, encrypted ZIP, encrypted RAR5 headers and data, RAR multi-volume,
nested ZIP/TAR.GZ members, 5 GiB source offsets, password retry preservation, and
catalog password retention rules. The [format support matrix](ARCHIVE-SUPPORT.md)
records the complete registry, native/embedded extraction comparisons, additional
ZIP/CAB/WIM/legacy RAR decoder tests and a bounded decode/hash/RSS qualification.
The extended audit compares 373 decoded inputs across 58 handlers, plus dedicated
Split tests; Hxs remains without a qualified sample. Automatic indexing is narrower:
only user archive layers recurse. Programs, packages, documents, media and images
remain leaf files, with nested OST filenames and identical-content aliases covered
by end-to-end local planning and offline browsing tests.
The bridge was compiled from the pinned 7-Zip
source with wasi-sdk 34 and executed under wazero without filesystem preopens.

Compression tests exercise SSC mode-page parsing, preservation of decompression
and buffering settings, configuration readback and failures through a simulated
SCSI transport. Engine tests check that registration changes no drive settings,
each write applies the saved preference, and a configuration failure prevents
record writes. These tests do not issue commands to physical hardware.

Keep tests focused on behavior and data integrity. A deleted command does not
need a test just to assert that it is absent. Help wording and screenshots are
not substitutes for preservation of the archive and catalog invariants.

Source throughput qualification uses external tools. Local tests still exercise
the actual writer, bounded pools, refill, cancellation and timeout handling on
simulated media. Qualify a changed I/O path before physical use; measurements are
not an extra automatic pass over each backup source.

## Direct reader qualification

Measured on 2026-10-11 using the direct Linux SG_IO backend, a Ryzen 9 5900X and
approximately 94 GiB of usable RAM, with tape format v1.

| Check | Result |
|---|---|
| Complete bounded reader, decoder and hashing on synthetic tmpfs media | 1,025 files, 12.88 GB, 12,294 objects; reached EOD |
| Synthetic input paced at 220 MB/s | 219.86 MB/s; no sink-buffer waits |
| Synthetic 8 GiB pool with `GOMEMLIMIT=10GiB` | Peak process RSS 8.61 GB |
| One physical BOT-to-EOD pass on HP LTO-6 | 5 archives, 1,801,246 objects; all digests and replica layouts matched |
| Physical streaming rate and elapsed time | About 161.7 MB/s; 195.2 minutes including positioning and catalog checks |
| Physical 8 GiB pool with `GOMEMLIMIT=10GiB` | Peak sampled RSS 8.97 GB; no sink-buffer waits |
| Privileges during the physical scan | Ordinary UID, no effective capabilities or helper process |

After scanning, the encrypted catalog was exported and the five offline file
trees matched the saved manifests. These checks used local catalog data and did
not reread the tape.

## Range-reader qualification

The 2026-10-14 local qualification uses the current verifier, including its hard
producer bound, normal decoder/content hashing, catalog comparisons and progress
callback. Three synthetic archives are created; only the last two and their
intervening filemark are read, followed by one EOD check.

| Check | Result |
|---|---|
| Selected encoded bytes | 10,738,173,268 |
| Read operations | 10,249 objects plus one EOD response; one locate |
| Pool / runtime soft limit | 8 GiB / 10 GiB |
| Input paced at 220 MB/s | 219.99 MB/s; zero sink waits |
| Peak RSS / GC / swap | 8,406,640 KiB / 13 GC cycles / zero swap |
| Physical tape operations | None; synthetic tmpfs data |

The preparation run was interrupted by the operator before completing the local
qualification. The completed run reused those synthetic files. This was never a
physical tape pass. Frame/group fuzzing, range-boundary regressions and the complete
race/vet checks passed before the authorized physical read.

One authorized physical range pass on the same host then checked the final two
archives and their intervening filemark, with exactly one EOD check at the end:

| Check | Result |
|---|---|
| Encoded bytes read and content hashed | 564,821,381,278 |
| Logical objects read | 538,663; earlier archives not read |
| Result | Both archive digests/layouts and the boundary matched; EOD immediately followed the final filemark |
| Elapsed time / average including positioning | 58m 36.12s / 160.6 MB/s |
| Peak RSS / swap | 8,652,456 KiB / zero swap |
| Sink-buffer waits | None |

Offline comparison of authenticated before/after catalog exports preserved all
other catalog values, including the deep archive trees and saved passwords.
The private terminal recording and operation evidence remain outside the repo.

## Interpretation

The measured throughput and memory figures describe this workload and host, not
all tape drives or sources. A zero software buffer-wait count does not directly
measure mechanical backhitches. Read-pool memory is only part of process memory;
qualify the complete path locally before changing it. Physical tape is not a
benchmark loop, and an interrupted verification is not an invitation to repeat it.

## Replica synchronization and image writes

Local tests cover all four device/image endpoint combinations, source/destination
identity rejection, sparse image merging with a read-only catalog, encrypted and
zstd records, one bounded source pass for two simulated drives, target prefix and
capacity checks, source failure without retries, and pruning only a common durable
physical prefix. Image tests cover reads from pending buffers, nonadjacent writes,
short sparse slots, filemarks and unpublished data after simulated interruption.

On 2026-10-16 the image writer was changed to reuse a 4 MiB aggregation buffer,
request asynchronous Linux writeback, and publish coverage after a 4 GiB durable
checkpoint or an explicit Flush/filemark/Close. An external synthetic record source
exercised the complete bounded reader, frame decoding, content SHA-256, encryption,
image checkpoints and final flush on XFS. No physical tape was opened.

| Target and path | Result |
|---|---|
| NVMe SSD, 17,179,869,856 encoded bytes, input paced at 160 MB/s, 8 GiB pool | 159.93 MB/s including final flush; zero sink-buffer waits |
| Same SSD run, peak RSS / swap / GC cycles | 8,418,808 KiB / 0 / 13 |
| NVMe SSD, 8,589,935,261 bytes, input paced at 200 MB/s, 128 MiB pool | 194.11 MB/s; maximum sink-buffer wait 0.44 s |
| HDD writer alone, 8 GiB of encrypted full slots, including final flush | 191.16 MB/s |
| HDD complete decode/hash/encrypt/write path, 8,589,935,261 bytes, 128 MiB pool | 162.78 MB/s; maximum sink-buffer wait 1.44 s |

Before asynchronous writeback, the same 8 GiB checkpoint workload on the SSD
measured 154.69 MB/s with a 13.98 s sink-buffer wait. A short test that fits before
the first checkpoint did not reveal that stall. Use the complete path and actual
pool configuration for qualification. These samples do not guarantee whole-volume
speed across different disk regions, competing I/O or SSD cache exhaustion. HDD
throughput has little margin over LTO-6, so SSD capture followed by an offline
image-to-image transfer remains the preferred workflow on this host.

## Complete-image acceptance

On 2026-10-19 a completed 2.45 TB image containing seven archives was mounted
read-only through the Linux kernel. This acceptance used only the image and a
read-only catalog; no physical tape was opened.

| Check | Result |
|---|---|
| Original file trees | 58,776 files and 3,143 directories; all paths, sizes, modes and owners matched |
| Whole-file SHA-256 samples | 22 files, 6,306,212 bytes; matched the catalog |
| Large-file reads | Small beginning, middle, boundary and tail windows in every archive; repeated/concurrent results matched and EOF was correct |
| Read-only enforcement | Write-open, create, mkdir, chmod, truncate, rename and unlink returned EROFS |
| Preservation | Catalog and authentication sidecar hashes, image header hash, size, allocation and modification timestamps unchanged |
| Resource bounds | HDD reads capped at 4 MiB/s and 8 IOPS; real-image acceptance cgroup peaked at 854,188,032 bytes including page cache |

This is a directory comparison and bounded content sample, not a second complete
image verification. Source capture and image merging already decoded records and
checked archive/content digests in their sequential transfer passes.

The opt-in kernel suite also exercises the CLI path from staged files through
encrypted raw/zstd archives, image merging and both single-archive and whole-set
mounts. It checks deduplicated aliases, Unicode names, empty files, missing image
data (ENODATA), corrupted payloads (EIO), read-only private mmap and clean unmount.
All device endpoints in these tests are simulators. Run in an isolated namespace:

```sh
unshare --user --map-root-user --mount env TAPER_FUSE_KERNEL=1 \
  go test -race ./cmd/taper ./internal/fusefs \
  -run 'TestReplicaSyncEndpointWorkflow|TestKernel' -count=1
```

Kernel testing caught buffered FUSE reads collapsing ENODATA into EIO. Regular
files now use FUSE direct I/O while retaining the bounded decoded-block cache and
buffered image backend. This changes neither the image layout nor the tape format.
The ZIP64 large-offset fixture uses the standard ZIP writer instead of a raw
header that omitted required local metadata with Go 1.26. The same offset and
extracted bytes are checked with both Go 1.26 and Go 1.27.
