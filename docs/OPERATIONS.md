# Operations

## Qualification

1. Run unit tests, race detection, decoder fuzzing, vet, and the simulated CLI
   workflow. Simulation never opens the real device.
2. Probe the real device and check MAM capacities, record limits and encryption
   capability. Capacity is planned in raw native bytes with a 1% / 2 GiB reserve.
3. Use an empty tape. Initialize a labeled medium, preserving its independent
   recovery key and encrypted catalog outside the source disk.
4. Archive a small synthetic batch containing small files, aliases, and files
   crossing record boundaries. Use `verify` and `restore`, inspect seek/read counts,
   then append a second batch and verify both. Exercise a new-catalog reindex.
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

`planned` batches have a complete hashed manifest. A replica transitions from
`writing` to `committed` after synchronous filemark success; failures record their
error separately. `verified` is an optional timestamp. Reindex checks the digest
and imports only complete committed replicas, while reporting corrupt/incomplete
batches. Existing committed replicas are unaffected by failed later appends.

Media state is `open` or `sealed`; sealed media remain readable. Offline capacity
recommendations use the last measurement and are advisory. The loaded medium is
rechecked immediately before writing. Capacity gains from hardware compression
are measured after committing and available to future batches, never assumed in
the current batch budget. If the batch does not fit, manually split it or provide
another tape; a single replica never spans tapes.

## Continuous supply

The bounded pool supports 8 MiB through 64 GiB. Large allocation checks available
RAM with 2 GiB of headroom and releases dead Go heaps before allocating. A real
write begins only after prefill. Cancellation joins the producer. Source stalls
are measured and an excessive wait terminates the uncommitted replica.

Before the first production write, complete a supply probe with the actual
immutable batch, intended pool size, and a consumption rate justified by the
drive qualification. Cache hits, background disk traffic and hardware compression
must be considered when interpreting this result. A finite pool cannot correct
a permanent producer deficit. The default 2-second stall limit bounds a failure;
it is not a claim that all physical drive backhitches can be prevented.

## First HDD migration

Identify the source HDD by manufacturer serial, size and filesystem UUID. Use
the stable `/dev/disk/by-id/` path, revalidating identity immediately before any
destructive operation. Record machine-specific identities in a private runbook.

The operational sequence is: read-only mount, concrete include/exclude review,
hashed manifest, native capacity check, committed tape replica, encrypted catalog
export and recovery-key retention, then GPT/XFS migration. No automatic batch
readback is required by the agreed workflow; manual `verify` remains available.

Before formatting, every retained manifest content must have a committed replica
on a registered tape and the source must remain frozen. Report excluded paths
and their sizes. Never infer approval to exclude personal data from a filename.
The program's core does not delete source files or format block devices.

The migration creates GPT with one aligned Linux filesystem partition covering
the HDD, formats it as XFS, updates `/etc/fstab` by new filesystem UUID, and mounts
at `/mnt` for future staging. Save the original partition table and fstab before
modification. The device identity and preservation checklist must be concrete
before execution; a stale `/dev/sda` name alone is insufficient.
