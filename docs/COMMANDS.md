# Command reference

The Cobra CLI has English text and one spelling per command, with no aliases.
Help, `--version` and shell completion never open a catalog or a drive.

| Command | Purpose |
|---|---|
| `replica create NAME` | Create a logical replica set with optional encryption |
| `replica ls` | List sets and member counts |
| `replica sync [NAME]` | Synchronize a replica stream between devices and sparse images |
| `replica add [NAME] --device DEVICE` | Bind a cartridge using non-moving MAM queries |
| `replica rm [NAME] (--device DEVICE \| --tape IDENTIFIER)` | Forget a member and its physical replica records |
| `replica mount [NAME] MOUNTPOINT` | Mount original archived files read-only |
| `tape ls` | Show saved cartridge identity, replica set, notes, capacity and health |
| `tape probe --device DEVICE` | Query current drive/MAM facts without moving tape |
| `tape verify --device DEVICE [--archive FIRST[-LAST]]` | Verify the whole tape or one contiguous interval |
| `tape reindex --device DEVICE` | Rebuild catalog records in one BOT-to-EOD pass |
| `tape eject --device DEVICE` | Rewind and unload once |
| `stage add [PATH...]` | Add or refresh source selections |
| `stage rm PATH...` | Unstage files; `.` clears an unstarted stage |
| `stage ls [PATH...]` | List staged files |
| `stage plan` | Hash, deduplicate, index and budget against a replica set |
| `stage unlock PATH` | Supply a password for an indexed nested archive |
| `stage write [--device DEVICE] [--image FILE]` | Commit one complete archive, then clear the stage |
| `archive ls` | List committed archives and physical replica counts |
| `archive browse ARCHIVE [PATH...]` | Browse files and nested archive metadata offline |
| `catalog init` | Create an encrypted catalog with no implicit replica set |
| `catalog export --dest DIR` | Export an authenticated encrypted snapshot |
| `catalog import --source DIR` | Import a snapshot to a new catalog |
| `catalog rekey` | Change its passphrase |
| `key generate --key-file FILE --keycard FILE` | Generate an independent replica key and recovery card |

Omitting the set name in add/rm/mount/sync/plan selects it only when exactly one set
exists. A previously bound stage retains its set. Cartridge labels are distinct
from hardware serials and the optional human `--note`.

## Flags

Catalog-backed commands take `--catalog PATH` and `--pass-file FILE`; otherwise
the default catalog is `$XDG_DATA_HOME/taper/catalog.db` (or
`~/.local/share/taper/catalog.db`) and an existing secure `session.pass` is used
automatically. `TAPER_CATALOG` and `TAPER_PASS_FILE` override those defaults;
explicit flags take precedence. Passphrase, replica-key and archive-password
files must be mode 0600 and failures are reported. Device paths are Linux SG
nodes or persistent symlinks.
`tape probe`, `tape eject` and `key generate` need no catalog.

- `replica create`: `--key-file`, `--no-save-key`, `--hardware-compression` (true).
- `replica add`: `--label`, `--note`, `--key-file`.
- `replica rm`: `--tape` or `--device`; removing the last member warns and asks
  unless `-y/--yes` is supplied. `-n/--dry-run` never changes membership.
- `replica mount`: `--image`, `--device`, or both; `--archive`, `--min-blocks`
  (256), `--memory`, `--key-file`. Direct FUSE needs external mount permission.
- `replica sync`: exactly one of `--from-device` / `--from-image` and exactly one
  of `--to-device` / `--to-image`; `--archive`, `--memory`, `--key-file`.
  `--prune` requires image-to-device transfer and releases only batches durably
  present on all members. A new destination image is created after confirmation.
- `tape verify`: `--archive`, `--memory`, `--key-file`.
- `tape reindex`: `--memory`, `--key-file`; when recovering an unknown set,
  `--hardware-compression` must explicitly give its future write preference.
- `stage add`: `--source`, `--rules` (JSON allow/deny patterns).
- `stage plan`: `--replica`, `--name`, `--no-cache`.
- `stage write`: at least one of `--device` and `--image`; `--zstd` (false),
  `--memory`, `--key-file`, `--max-wait` (2s).
- `stage unlock`: `--password-file`, `--remember`.
- `archive browse`: `--tui`, a compact numbered offline browser. `ARCHIVE` is a full ID,
  unique ID prefix, or exact archive name.

Memory is integer bytes, default 1 GiB, supported stream-pool range 8 MiB–64 GiB.
Pool size is not a process RSS limit. Hardware compression and independent zstd
are separate settings. Sources must stay unchanged until image/tape preparation
finishes. Selection policy, cloud transfers, cleanup and disk formatting remain
external tasks.

## Selectors, output and confirmation

The optional range filter `--archive` accepts a full 32-hex ID or a unique prefix of at least six hex digits:
`FIRST`, `FIRST-LAST`, `FIRST-`, `-LAST`. Use `--archive=-LAST` when needed for
shell/flag clarity. Endpoints belong to the selected set or available tape
prefix, are inclusive, and cannot be reversed. A transfer to a device must start at the
member's next missing batch. Verify includes native inter-archive filemarks;
selecting the member's last archive adds exactly one EOD check.

Every moving operation previews its scope and asks once. `-y/--yes` supports
non-interactive use; `-n/--dry-run` takes precedence and makes no image/catalog
mutation, key/mode change or tape movement. Identity queries are permitted.
Offline image writes and mounts do not require a tape-wear prompt. Device-backed
mount confirms its whole allowed scope once, rather than prompting per read.

`--json` has clean structured stdout. Human previews/password prompts/progress
use stderr; a dry-run plan uses stdout. IBM label colors are shared throughout
human output and disabled for JSON, redirected output, `NO_COLOR`, and dumb
terminals. Progress rates update from actual producer samples, not redraw timers.
Unknown totals and positioning never receive an invented ETA.

Only current v1 schema/format and command names are supported. Conversion is an
explicit backed-up operation using the [migration script](../scripts/migrate_v1.sql).
See [images and replica sets](IMAGES.md) for synchronization and recovery details.
