# CLI and remaining TUI design

The grouped CLI is implemented with Cobra, using its argument validation, flag
parsing, help and shell completion. There is no hand-maintained command whitelist,
compatibility alias, plugin/configuration framework or internal worker.
[COMMANDS.md](COMMANDS.md) describes the current interface.

Human-readable results are the default; `--json` is explicit. English prompts,
IBM-style tape label cells and compact tables share a small rendering layer.
Long operations use schollz/progressbar on a terminal and periodic plain status
otherwise. Atomic progress counters isolate rendering from tape I/O. A terminal
recording is an external asciinema/tmux operation, never a taper dependency.

Verification presents the exact selected tape and contiguous archive interval,
then asks once. `--dry-run` is non-moving and `--yes` supports automation. Other
important device/catalog actions may adopt the same scope preview when useful,
but routine reads, staging, exports and existing password prompts should not
accumulate confirmation steps.

The existing numbered `archive browse --tui` is retained. A future full TUI could
use Bubble Tea and Bubbles with three views: Archives, Stage and Tapes. It would
reuse the engine and rendering data, display one active operation with progress,
and keep all source selection policy and cloud operations external. That future
interface is not implemented. No framework migration, new TUI command or restore
workflow is implied by the current CLI changes.
