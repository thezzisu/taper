# Operations

## Qualification

1. Run unit tests, race detection, decoder fuzzing, vet, and the simulated CLI
   workflow. Simulation never opens the real device.
2. Probe the real device and check MAM capacities, record limits and encryption
   capability. Capacity is planned in raw native bytes with a 1% / 2 GiB reserve.
3. Register a cartridge read-only, preserving its independent
   recovery key and encrypted catalog outside the source disk.
4. Use staging to plan synthetic files with aliases and record boundaries.
   The first write requires an empty tape and writes the media label. Qualification
   is one planned sequence: write the cases, then one full `verify` or `reindex`
   pass as needed. Avoid repeated positioning or separate per-archive readbacks.
5. Qualify hardware encryption and optional zstd on the same initialized tape.
   Every batch on that tape uses its original hardware key. Device key material
   is cleared when the worker closes.

The native backend currently supports Linux, partition 0, manually loaded tape,
and explicit SG_IO records. It issues READ POSITION long form and LOCATE(16).
The read/write roles are distinct. It does not issue erase, format, raw user CDBs,
or automatic tape swaps. Device errors are not blindly retried.

## Key handling

The master passphrase protects the encrypted SQLite catalog, including optional
stored independent tape keys. Argon2id parameters are fixed by unlock version 1:
64 MiB, 3 iterations, 4 lanes, random 16-byte salt. An XChaCha20-Poly1305 envelope
wraps a random database key. The encrypted database uses the established Adiantum
VFS. Rekey atomically replaces the unlock descriptor and syncs its directory.

A tape recovery card is indispensable if the catalog is lost: the master
passphrase does not derive a tape key. Keep catalog exports and recovery cards
outside both the source filesystem and the source repository. Do not place secret
arguments in process command lines. Private pass/key files and hidden terminal
prompts are supported.

## States and recovery

`stage add/rm` change a persistent selection. `stage plan` creates a complete
hashed manifest and later selection edits invalidate it. `archive list` includes
only archives with a successful commit. A replica transitions from
`writing` to `committed` after synchronous filemark success; failures record their
error separately. `verified` is an optional timestamp. Reindex checks the digest
and imports only complete committed replicas, while reporting corrupt/incomplete
batches. Existing committed replicas are unaffected by failed later appends.

Media state is `registered` before its first write, then `open`. Offline capacity
recommendations use the last measurement and are advisory. The loaded medium is
rechecked immediately before writing. Capacity gains from hardware compression
are measured after committing and available to future batches, never assumed in
the current batch budget. If the batch does not fit, manually split it or provide
another tape; a single replica never spans tapes.

## Continuous supply

The bounded pool supports 8 MiB through 64 GiB. Large allocation checks available
RAM with 2 GiB of headroom and releases dead Go heaps before allocating. A real
write begins only after prefill. If the complete selection fits, hashing and
buffering finish before any tape write. At low water the writer synchronously
flushes with WRITE FILEMARKS count zero, which adds no tape object, then refills
the pool before resuming. Cancellation joins the producer. Unplanned source
stalls still terminate the uncommitted replica. A larger pool reduces the
number of deliberate stop/start cycles when the source is persistently slow.

Before the first production write, complete a supply probe with the actual
immutable batch, intended pool size, and a consumption rate justified by the
drive qualification. Cache hits, background disk traffic and hardware compression
must be considered when interpreting this result. A finite pool cannot correct
a permanent producer deficit. The default 2-second stall limit bounds a failure;
it is not a claim that all physical drive backhitches can be prevented.

## Sequential tape I/O

`verify` and `reindex` use the same full-tape scanner. There is one initial locate
to BOT, followed by continuous record reads through EOD. A bounded reusable buffer
pool separates device I/O from parsing/hashing. If the pool approaches full,
reading waits for substantial free capacity before resuming instead of making
isolated single-record reads. Reindex commits to SQLite after
the tape pass, and parser lookahead never physically rewinds the tape.

SG_IO commands temporarily block asynchronous signals on their OS thread. Linux
sg can return ERESTARTSYS while a command is already executing; automatic syscall
restart would submit a tape command twice. Completion is checked without retries,
and final logical positions must match transferred record counts.
