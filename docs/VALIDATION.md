# Local validation

Measured on 2026-10-10, Linux, Go 1.27.1, Ryzen 9 5900X, 94 GiB usable RAM.
These results cover the host implementation. Actual drive qualification is pending.

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
