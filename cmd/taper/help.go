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
  media init|list|probe              Register and inspect cartridges (read-only)
  stage add|rm|ls|plan|write          Select, plan and write the staging area
  archive list                      List stored archives
  key generate                      Generate an independent recovery key
  browse                            Browse the offline tree (--tui)
  verify                            Verify the entire loaded tape through EOD
  reindex                           Rebuild the catalog from the entire tape

Run taper COMMAND SUBCOMMAND --help for options.
`

var options = map[string]string{
	"init": "", "export": "dest", "import": "source", "rekey": "",
	"media-init": "device name key-file no-save-key key-backed-up",
	"media-list": "", "probe": "device", "archive-list": "",
	"stage-add": "source rules", "stage-rm": "", "stage-ls": "",
	"stage-plan":   "name copies frozen no-cache",
	"key-generate": "key-file keycard name",
	"stage-write":  "device key-file memory zstd frozen probe-mbps max-wait",
	"browse":       "archive tui",
	"verify":       "device key-file memory", "reindex": "device key-file memory", "device-worker": "device write",
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
	public := map[string]string{"init": "catalog init", "export": "catalog export", "import": "catalog import", "rekey": "catalog rekey", "probe": "media probe"}
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
