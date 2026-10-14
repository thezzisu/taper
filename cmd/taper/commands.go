package main

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

type settings struct {
	catalog, passFile, device, source, name, rules, archive, dest string
	keyFile, keycard, passwordFile                                string
	copies                                                        int
	memory                                                        int64
	maxWait                                                       time.Duration
	json, yes, dryRun, noSaveKey, hardwareCompression, zstd       bool
	noCache, tui, remember                                        bool
}

func run(ctx context.Context, args []string) error {
	c := newCommand()
	c.SetArgs(args)
	return c.ExecuteContext(ctx)
}

func newCommand() *cobra.Command {
	o := &settings{}
	root := &cobra.Command{Use: "taper", Short: "Packed tape archives with an offline encrypted catalog", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().BoolVar(&o.json, "json", false, "emit structured JSON results")
	group := func(name, description string) *cobra.Command {
		c := &cobra.Command{Use: name, Short: description}
		root.AddCommand(c)
		return c
	}
	leaf := func(parent *cobra.Command, use, description string, args cobra.PositionalArgs, catalog bool) *cobra.Command {
		c := &cobra.Command{Use: use, Short: description, Args: args}
		c.RunE = func(c *cobra.Command, paths []string) error {
			return execute(c.Context(), parent.Name()+" "+c.Name(), paths, o, c)
		}
		if catalog {
			home, _ := os.UserHomeDir()
			c.Flags().StringVar(&o.catalog, "catalog", filepath.Join(home, ".local/share/taper/catalog.db"), "encrypted catalog path")
			c.Flags().StringVar(&o.passFile, "pass-file", "", "private catalog passphrase file; otherwise prompt")
		}
		parent.AddCommand(c)
		return c
	}
	device := func(c *cobra.Command) {
		c.Flags().StringVar(&o.device, "device", "", "Linux tape SG device or persistent symlink")
		_ = c.MarkFlagRequired("device")
	}
	key := func(c *cobra.Command) {
		c.Flags().StringVar(&o.keyFile, "key-file", "", "private file containing the 64-hex-digit tape key")
	}
	memory := func(c *cobra.Command) {
		c.Flags().Int64Var(&o.memory, "memory", 1<<30, "bounded stream pool size in bytes")
	}

	cat := group("catalog", "Manage the encrypted SQLite catalog")
	leaf(cat, "init", "Create a new catalog", cobra.NoArgs, true)
	c := leaf(cat, "export", "Export an authenticated encrypted snapshot", cobra.NoArgs, true)
	c.Flags().StringVar(&o.dest, "dest", "", "new export directory")
	_ = c.MarkFlagRequired("dest")
	c = leaf(cat, "import", "Import a snapshot into a new catalog", cobra.NoArgs, true)
	c.Flags().StringVar(&o.source, "source", "", "authenticated snapshot directory")
	_ = c.MarkFlagRequired("source")
	leaf(cat, "rekey", "Change the catalog passphrase", cobra.NoArgs, true)

	tape := group("tape", "Register, inspect and check tapes")
	c = leaf(tape, "register", "Register cartridge identity and compression preference", cobra.NoArgs, true)
	device(c)
	key(c)
	c.Flags().StringVar(&o.name, "name", "", "cartridge label")
	c.Flags().BoolVar(&o.noSaveKey, "no-save-key", false, "do not retain the tape key in the catalog")
	c.Flags().BoolVar(&o.hardwareCompression, "hardware-compression", true, "hardware compression preference, applied before writes")
	leaf(tape, "ls", "List tapes and saved capacity observations", cobra.NoArgs, true)
	c = leaf(tape, "probe", "Query the loaded tape without moving it", cobra.NoArgs, false)
	device(c)
	c = leaf(tape, "verify [ARCHIVE_ID [LAST_ID]]", "Verify a whole tape, one archive, or an inclusive archive range", cobra.MaximumNArgs(2), true)
	device(c)
	key(c)
	memory(c)
	c.Flags().BoolVarP(&o.yes, "yes", "y", false, "verify the displayed scope without prompting")
	c.Flags().BoolVarP(&o.dryRun, "dry-run", "n", false, "show the checked scope without reading tape records")
	c.Long = "Verify a whole tape with no IDs, one archive with one ID, or every archive from FIRST to LAST with two IDs. IDs must belong to the loaded tape. Ranges ending at the last archive also check EOD. Interrupted reads are never restarted automatically."
	c.Example = "  taper tape verify --device /dev/sg1 --dry-run FIRST_ID LAST_ID\n  taper tape verify --device /dev/sg1 --yes FIRST_ID LAST_ID"
	c = leaf(tape, "reindex", "Rebuild catalog entries in one complete tape read", cobra.NoArgs, true)
	device(c)
	key(c)
	memory(c)

	stage := group("stage", "Select, plan and write files; keep sources unchanged through all writes")
	c = leaf(stage, "add [PATH...]", "Add or refresh paths relative to the source root", cobra.ArbitraryArgs, true)
	c.Flags().StringVar(&o.source, "source", "", "source directory; reuse the stage root when omitted")
	c.Flags().StringVar(&o.rules, "rules", "", "JSON include/exclude rules")
	leaf(stage, "rm PATH...", "Unstage paths; use . to clear the stage", cobra.MinimumNArgs(1), true)
	leaf(stage, "ls [PATH...]", "List staged paths", cobra.ArbitraryArgs, true)
	c = leaf(stage, "plan", "Hash, index and budget the stage; keep source files unchanged", cobra.NoArgs, true)
	c.Flags().StringVar(&o.name, "name", "", "archive name")
	c.Flags().IntVar(&o.copies, "copies", 0, "required copies; 1 for a new plan, otherwise unchanged")
	c.Flags().BoolVar(&o.noCache, "no-cache", false, "rehash without saved scan progress")
	c = leaf(stage, "unlock PATH", "Retry an encrypted container, including outer.7z!/inner.zip", cobra.ExactArgs(1), true)
	c.Flags().StringVar(&o.passwordFile, "password-file", "", "private archive password file; otherwise prompt")
	c.Flags().BoolVar(&o.remember, "remember", false, "save archive passwords in the encrypted catalog")
	c = leaf(stage, "write", "Write one replica; keep selected source files unchanged", cobra.NoArgs, true)
	device(c)
	key(c)
	memory(c)
	c.Flags().BoolVar(&o.zstd, "zstd", false, "adaptive software zstd, independent of hardware compression")
	c.Flags().DurationVar(&o.maxWait, "max-wait", 2*time.Second, "stop if the source cannot supply another record within this time")

	archive := group("archive", "List and browse committed archives offline")
	leaf(archive, "ls", "List committed archives and copy counts", cobra.NoArgs, true)
	c = leaf(archive, "browse [PATH...]", "Browse stored paths and archive contents", cobra.ArbitraryArgs, true)
	c.Flags().StringVar(&o.archive, "archive", "", "committed archive ID")
	_ = c.MarkFlagRequired("archive")
	c.Flags().BoolVar(&o.tui, "tui", false, "interactive offline directory browser")
	c.MarkFlagsMutuallyExclusive("tui", "json")

	keys := group("key", "Create independent tape recovery keys")
	c = leaf(keys, "generate", "Create a tape key and printable recovery card", cobra.NoArgs, false)
	key(c)
	_ = c.MarkFlagRequired("key-file")
	c.Flags().StringVar(&o.keycard, "keycard", "", "new recovery card file")
	_ = c.MarkFlagRequired("keycard")
	c.Flags().StringVar(&o.name, "name", "", "recovery card label")
	return root
}
