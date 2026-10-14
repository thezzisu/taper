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
