package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

const usage = `taper — packed tape archives with an offline encrypted catalog

Usage: taper COMMAND [SUBCOMMAND] [OPTIONS]

  catalog init|export|import|rekey    Manage the encrypted SQLite catalog
  media init|list|seal|probe|inspect  Register and inspect cartridges
  batch scan|check|plan|status|list   Inspect and plan immutable batches
  batch probe|spool                  Test data supply without a tape
  key generate                      Generate an independent recovery key
  sim create                        Create a simulated tape
  backup                            Append a batch replica
  browse                            Browse the offline tree (--tui)
  restore [--plan]                   Plan or restore selected paths
  verify                            Manually read back a replica
  reindex                           Rebuild the catalog from sidecars

Run taper COMMAND SUBCOMMAND --help for options.
`

var options = map[string]string{
	"init": "save-pass", "export": "dest", "import": "source", "rekey": "",
	"media-init": "sim device name key-file keycard generate-key no-save-key key-backed-up",
	"media-list": "", "media-seal": "media", "probe": "sim device", "inspect": "sim device",
	"scan": "source rules name frozen no-cache", "check": "source rules", "plan": "batch", "status": "batch", "batch-list": "",
	"probe-supply": "batch memory probe-mbps frozen", "spool": "batch dest memory frozen",
	"key-generate": "key-file keycard name", "sim-create": "sim capacity compression",
	"backup": "sim device batch key-file memory zstd frozen probe-mbps max-wait",
	"browse": "batch tui", "restore": "sim device batch dest key-file replica plan", "restore-plan": "batch plan",
	"verify": "sim device batch key-file replica", "reindex": "sim device key-file", "device-worker": "device write",
}

func allowed(command string) map[string]bool {
	set := map[string]bool{"catalog": true, "pass-file": true}
	for _, s := range strings.Fields(options[command]) {
		set[s] = true
	}
	return set
}
func help(f *flag.FlagSet, command string, writer io.Writer) {
	set := allowed(command)
	public := map[string]string{"init": "catalog init", "export": "catalog export", "import": "catalog import", "rekey": "catalog rekey", "scan": "batch scan", "check": "batch check", "plan": "batch plan", "status": "batch status", "probe-supply": "batch probe", "spool": "batch spool", "probe": "media probe", "inspect": "media inspect"}
	label, ok := public[command]
	if !ok {
		label = strings.ReplaceAll(command, "-", " ")
	}
	fmt.Fprintf(writer, "Usage: taper %s [OPTIONS]\n", label)
	f.VisitAll(func(v *flag.Flag) {
		if set[v.Name] {
			fmt.Fprintf(writer, "  --%-18s %s (default %s)\n", v.Name, v.Usage, v.DefValue)
		}
	})
}
