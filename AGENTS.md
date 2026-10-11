# Working constraints

- Protect tape life. Each approved write may have at most one tape reading pass.
  An interrupted pass counts. Do not restart verification, add a reindex pass,
  or perform spot reads after it. Stop device work and investigate locally.
- Qualify the complete I/O path without a tape: worker transport, decoding,
  hashing, allocation, garbage collection and peak process memory. Packer-only
  or direct-simulator throughput tests do not qualify the worker read path.
- Keep taper minimal. Selection policy, source cleanup, disk migration,
  filesystem formatting and temporary operational helpers stay outside taper.
- Keep the CLI under the ordinary user and privilege only the typed device
  worker. The operator must be able to cancel the main process immediately.
- Never commit private file inventories, catalogs, passphrases or tape keys.
