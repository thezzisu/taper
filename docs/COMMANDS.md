# Command reference

The CLI uses Cobra. All built-in text is English. Names have one spelling and no
command aliases. Use `taper --help`, `taper tape verify --help`, or the built-in
`completion` command. Help and completion do not open a catalog or a drive.

## Commands

| Command | Purpose | Tape access |
|---|---|---|
| `catalog init` | Create a new encrypted catalog | None |
| `catalog export --dest DIR` | Save an authenticated encrypted snapshot | None |
| `catalog import --source DIR` | Import a snapshot to a new catalog | None |
| `catalog rekey` | Change the catalog passphrase | None |
| `tape register --device PATH` | Save identity, key and compression preference | Non-moving device/MAM queries |
| `tape ls` | Show saved tapes and write budgets | None |
| `tape probe --device PATH` | Query loaded tape and drive capabilities | Non-moving queries |
| `tape verify --device PATH [FIRST_ID [LAST_ID]]` | Verify all, one, or a contiguous archive range | One bounded forward pass |
| `tape reindex --device PATH` | Rebuild catalog entries from tape | One full BOT-to-EOD pass |
| `stage add [PATH...]` | Add/refresh source paths | None |
| `stage rm PATH...` | Unstage paths; `.` clears stage | None |
| `stage ls [PATH...]` | List staged paths and byte sizes | None |
| `stage plan` | Hash, deduplicate, index archives and budget | None; reads source files as needed |
| `stage unlock PATH` | Retry an encrypted nested archive | None |
| `stage write --device PATH` | Write one replica of the current plan | One append; new tapes also get a BOT empty check |
| `archive ls` | List committed archives | None |
| `archive browse --archive ID [PATH...]` | Browse paths and nested archive trees | None |
| `key generate --key-file FILE --keycard FILE` | Create a key and printable recovery card | None |

Selection policy, cloud access/cleanup, caching, formatting, process management
and performance qualification remain operator tasks outside taper. There is no
restore/extract, erase, rollback, migration or supply-test command.

## Output and interaction

Human-readable tables and summaries are the default. `--json` produces structured
results on stdout, with empty collections as `[]`. Results keep integer bytes and
logical positions. Password prompts, progress and verification previews go to
stderr; a dry-run plan goes to stdout. Flags can occur before or after arguments;
`--` ends flag parsing for names beginning with a dash.

Tape labels use a consistent IBM-style digit palette in terminal headers, tape
lists, recommendations and the offline browser. Custom non-LTO labels remain
plain text. Redirected output, JSON, `TERM=dumb` and `NO_COLOR=1` have no color.
Live progress runs independently of I/O with a periodic refresh; redirected
progress uses occasional plain lines. Unknown totals use a spinner without an
invented percentage. Progress shows elapsed time and measured rate without an
ETA derived from coarse record updates. Reaching all data records still shows finalization
until boundary checks and catalog persistence finish.

Verification displays its checked scope before one default-no prompt. `--yes`
(`-y`) skips that prompt; non-interactive use requires it. `--dry-run` (`-n`)
shows the plan without reading or moving tape, and takes precedence over `--yes`.
It can be combined with `--json`. These flags apply only to verification; routine
listing, browsing, staging and snapshot exports do not acquire extra prompts.

The numbered `archive browse --tui` remains a compact offline browser. Its inputs
are a number, `u` (parent), `p` (details), or `q` (quit), followed by Enter. It starts
at the archive root and cannot be combined with `--json`.

## Options

Catalog-backed commands accept `--catalog PATH` (default
`~/.local/share/taper/catalog.db`) and `--pass-file FILE`; otherwise the catalog
passphrase is read without echo. `tape probe` and `key generate` need no catalog.

| Command | Other options |
|---|---|
| `tape register` | `--name`, `--key-file`, `--no-save-key`, `--hardware-compression` (true) |
| `tape verify` | `--key-file`, `--memory` (1073741824 bytes), `-y/--yes`, `-n/--dry-run` |
| `tape reindex` | `--key-file`, `--memory` (1073741824 bytes) |
| `stage add` | `--source`, `--rules` |
| `stage plan` | `--name`, `--copies`, `--no-cache` |
| `stage unlock` | `--password-file`, `--remember` |
| `stage write` | `--key-file`, `--memory` (1073741824 bytes), `--zstd` (false), `--max-wait` (2s) |
| `archive browse` | `--tui` |
| `key generate` | `--name` |

`--memory` accepts integer bytes. `--key-file` holds 64 hexadecimal digits for a
32-byte tape key. Keep the recovery card separately. Registration saves hardware
compression preference without changing the drive; the writer applies it.
Use `--hardware-compression=false` to disable it. Software zstd is independent
and defaults off. Planning assumes no compression gain.

A new plan defaults to one required replica; an existing plan retains its count
unless `--copies` is supplied. `--no-cache` requests rehashing. Keep source files
unchanged from staging through all writes. `stage rm` never deletes source files.

## Verification range

No IDs means BOT through the last catalog archive and EOD. One full ID means
that archive. Two full IDs include both endpoints and every committed archive
between them on this tape. Both must uniquely belong to the loaded tape; a
reversed interval is rejected. Unknown or overlapping positions are rejected.
Inter-archive gaps must be filemarks. Including the last archive automatically
checks for EOD immediately after its final filemark. The bounded producer does
not continue beyond an unexpected tail. Only the checked replicas receive new
verification timestamps, after the complete selected pass succeeds.

`tape reindex` retains full-tape recovery behavior. It neither accepts archive
selectors nor extracts files. An interrupted physical read counts as a pass;
retries and any further read require a deliberate operator decision.

## Current formats

Only the current catalog layout and version 1 are supported. Opening an old or
incomplete layout fails; there is no implicit conversion or compatibility alias.
Local conversions require a backup and remain outside the repository. Capacity
observations preserve raw responses and timestamps; conservative write budgets
are distinct from device counters. See [operations](OPERATIONS.md).
