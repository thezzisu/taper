# Local validation

Measured on 2026-10-10, Linux, Go 1.27.1, Ryzen 9 5900X, 94 GiB usable RAM.
The local tests below cover the host implementation. A real-drive result and an
operational failure are recorded separately below.

| Scenario | Result |
|---|---|
| Raw/zstd writes, aliases, cross-record files, reindexed content hashes | Passed |
| Empty files, explicit rules, unsafe paths and unsupported entries | Passed |
| Second-tape replica using the same logical batch | Passed |
| Write/flush ambiguity, source mutation, cancellation | Passed |
| Catalog loss and sequential reindex, failed append followed by successful retry | Passed |
| Catalog encryption, wrong password, rekey, authenticated snapshot import | Passed |
| Staging add/rm/plan/write, failed-write retention, independent replicas | Passed |
| Read-only registration and occupied-tape refusal | Passed |
| Full-tape verify/reindex, one forward pass, recovery after corrupt archives | Passed |
| Grouped CLI workflow and offline terminal browser | Passed |
| Race detector and `go vet` | Passed |
| Frame and sidecar fuzzing | About 1.9 million generated cases, passed |

## Pool and disk tests

The opt-in `TestLargePool` hashed a 70 GiB sparse file, then used the production
packer and a 64 GiB pool against a simulated 1,000 MB/s consumer. It transferred
75,161,927,699 logical bytes in 71,685 records, including sidecar bytes. Prefill
took 65.12 s, the supply phase including prefill took 140.29 s, and there were no
consumer waits. Peak process RSS was 67,223,132 KiB (about 64.1 GiB).

A separate real-source test used `Backups/ubuntu.vhdx` (111.6 GB logical,
77.0 GB allocated) with the production packer and a 64 GiB pool against a
160 MB/s consumer. Prefill took 323.09 s; the total run took 1,020.67 s.
No consumer wait was measured. This covers that file and operating point;
it does not certify a complete batch or variable hardware compression.

The pool stress test is opt-in:

```sh
TAPER_LARGE_POOL_TEST=1 go test ./internal/engine -run '^TestLargePool$' -count=1 -timeout=20m -v
```

Sparse-file tests establish pool correctness and host processing capacity. A
production source needs a separate, cold-source supply probe. Drive hardware
compression changes host consumption rate; zero local underflows at a specified
rate do not establish a universal guarantee about physical tape backhitches.

The data source inventory, full source hashes, capacity calculation, and supply
probe reports are private machine-specific artifacts kept outside the repository.

## Physical drive result and verification incident

On 2026-10-11, an HP LTO-6 drive committed one hardware-encrypted archive of
4,710 files, 259,018 DATA records and 271,582,441,477 logical stream bytes.
Hardware and software compression were disabled. The writer used a 64 GiB pool
and reported no supply waits or refill pauses. These software observations do
not measure mechanical backhitches directly.

The first verification incorrectly reused the 64 GiB write-pool setting without
qualifying the complete worker read path. Gob decoder message allocations added
to the live pool, causing severe host memory pressure. That scan was cancelled
after 128,597 consumed tape objects. It was then restarted, causing unnecessary
repeated tape reading. This was an operational mistake, not a successful
single-pass qualification.

The subsequent complete scan used an 8 GiB pool and `GOMEMLIMIT=10GiB`. It reached
EOD after 259,023 tape objects, validated the archive digest and durable commit,
and reported no issues or sink-buffer waits. Sampled throughput was 160.07 MB/s;
peak sampled process RSS was 10,416,275,456 bytes. The authenticated encrypted
catalog export was imported locally: the complete file tree, hashes, media
identity and verified replica positions matched the source manifest.

The transport's transient allocations still exist. The smaller pool and runtime
limit are the validated operating configuration; this result does not certify a
64 GiB read pool. Future qualification must cover IPC and process memory locally.
The operating constraint is one write and at most one read pass; a failed or
interrupted scan must not lead to another physical scan.
