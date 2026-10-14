# Working constraints

- Protect tape life. Each approved write may have at most one tape reading pass.
  An interrupted pass counts. Do not restart verification, add a reindex pass,
  or perform spot reads after it. Stop device work and investigate locally.
- Qualify the complete I/O path without a tape: bounded reads, decoding,
  hashing, allocation, garbage collection and peak process memory. Packer-only
  throughput tests do not qualify the read path.
- Keep taper minimal. Selection policy, source cleanup, disk migration,
  filesystem formatting and temporary operational helpers stay outside taper.
- Open the tape directly under the current user. Do not add a worker process,
  internal privilege escalation, or process supervision. The operator may use
  tmux or explicitly elevate taper outside the program when necessary.
- Never commit private file inventories, catalogs, passphrases or tape keys.
- Support only the current catalog schema and command spellings. Reject other
  versions explicitly; do not add automatic migration or compatibility aliases.
  One-off conversions of local data stay outside the repository. Add an explicit
  migration command only when it is actually needed.
- The project is unreleased. Keep taper's format and schema versions at 1;
  development edits do not create supported historical versions.
- Back up catalogs before local conversion and compare all existing values after
  it. Derive new settings from recorded evidence, not defaults for new tapes;
  stop a conversion if a required value is unknown.
