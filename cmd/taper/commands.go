package main

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

type settings struct {
	catalog, passFile, device, source, name, rules, archive, dest                          string
	replica, image, label, note, from, to, tape                                            string
	keyFile, keycard, passwordFile                                                         string
	memory                                                                                 int64
	minBlocks                                                                              uint64
	maxWait                                                                                time.Duration
	json, yes, dryRun, noSaveKey, hardwareCompression, zstd, noCache, tui, remember, prune bool
}

func run(ctx context.Context, args []string) error {
	c := newCommand()
	c.SetArgs(args)
	return c.ExecuteContext(ctx)
}

// dataHome follows the XDG data directory convention while preserving taper's
// existing ~/.local/share/taper location when XDG_DATA_HOME is unset.
func dataHome() string {
	if v := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(v) {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".local", "share")
}

func defaultCatalogPath() string {
	if v := os.Getenv("TAPER_CATALOG"); v != "" {
		return v
	}
	return filepath.Join(dataHome(), "taper", "catalog.db")
}

func defaultPassFile() string {
	if v := os.Getenv("TAPER_PASS_FILE"); v != "" {
		return v
	}
	path := filepath.Join(dataHome(), "taper", "session.pass")
	if _, err := os.Stat(path); err == nil || !os.IsNotExist(err) {
		return path
	}
	return ""
}

func newCommand() *cobra.Command {
	o := &settings{}
	root := &cobra.Command{Use: "taper", Short: "Replica sets, sparse images and packed tape archives", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().BoolVar(&o.json, "json", false, "emit structured JSON results")
	group := func(name, desc string) *cobra.Command {
		c := &cobra.Command{Use: name, Short: desc}
		root.AddCommand(c)
		return c
	}
	leaf := func(parent *cobra.Command, use, desc string, args cobra.PositionalArgs, db bool) *cobra.Command {
		c := &cobra.Command{Use: use, Short: desc, Args: args}
		c.RunE = func(c *cobra.Command, paths []string) error {
			return execute(c.Context(), parent.Name()+" "+c.Name(), paths, o, c)
		}
		if db {
			c.Flags().StringVar(&o.catalog, "catalog", defaultCatalogPath(), "encrypted catalog path (TAPER_CATALOG overrides the default)")
			c.Flags().StringVar(&o.passFile, "pass-file", defaultPassFile(), "private catalog passphrase file (TAPER_PASS_FILE or secure default)")
		}
		parent.AddCommand(c)
		return c
	}
	device := func(c *cobra.Command, required bool) {
		c.Flags().StringVar(&o.device, "device", "", "Linux tape SG device or persistent symlink")
		if required {
			_ = c.MarkFlagRequired("device")
		}
	}
	key := func(c *cobra.Command) {
		c.Flags().StringVar(&o.keyFile, "key-file", "", "file containing the 64-hex-digit replica key")
	}
	memory := func(c *cobra.Command) {
		c.Flags().Int64Var(&o.memory, "memory", 1<<30, "stream pool budget in bytes (8 MiB–64 GiB)")
	}
	consent := func(c *cobra.Command) {
		c.Flags().BoolVarP(&o.yes, "yes", "y", false, "perform the displayed operation without prompting")
		c.Flags().BoolVarP(&o.dryRun, "dry-run", "n", false, "show the scope without mutation or tape movement")
	}
	selector := func(c *cobra.Command) {
		c.Flags().StringVar(&o.archive, "archive", "", "archive ID/prefix or FIRST-LAST, FIRST-, -LAST")
	}
	image := func(c *cobra.Command) { c.Flags().StringVar(&o.image, "image", "", "sparse replica image (.img)") }
	cat := group("catalog", "Manage the encrypted SQLite catalog")
	leaf(cat, "init", "Create an empty encrypted catalog", cobra.NoArgs, true)
	c := leaf(cat, "export", "Export an authenticated encrypted snapshot", cobra.NoArgs, true)
	c.Flags().StringVar(&o.dest, "dest", "", "new snapshot directory")
	_ = c.MarkFlagRequired("dest")
	c = leaf(cat, "import", "Import a snapshot into a new catalog", cobra.NoArgs, true)
	c.Flags().StringVar(&o.source, "source", "", "snapshot directory")
	_ = c.MarkFlagRequired("source")
	leaf(cat, "rekey", "Change the catalog passphrase", cobra.NoArgs, true)
	rep := group("replica", "Manage replica sets and mount their files")
	c = leaf(rep, "create NAME", "Create a replica set", cobra.ExactArgs(1), true)
	key(c)
	c.Flags().BoolVar(&o.noSaveKey, "no-save-key", false, "keep only the key ID in the catalog")
	c.Flags().BoolVar(&o.hardwareCompression, "hardware-compression", true, "hardware compression preference for all members")
	leaf(rep, "ls", "List replica sets", cobra.NoArgs, true)
	c = leaf(rep, "add [NAME]", "Bind the loaded cartridge without moving tape", cobra.MaximumNArgs(1), true)
	device(c, true)
	key(c)
	c.Flags().StringVar(&o.label, "label", "", "fallback cartridge barcode/label")
	c.Flags().StringVar(&o.note, "note", "", "human-readable cartridge note")
	c = leaf(rep, "rm [NAME]", "Forget a cartridge and its physical replica records", cobra.MaximumNArgs(1), true)
	device(c, false)
	c.Flags().StringVar(&o.tape, "tape", "", "member label, hardware serial or ID")
	c.MarkFlagsOneRequired("device", "tape")
	c.MarkFlagsMutuallyExclusive("device", "tape")
	consent(c)
	c = leaf(rep, "mount [NAME] MOUNTPOINT", "Mount stored files read-only; direct FUSE needs mount permission", cobra.RangeArgs(1, 2), true)
	device(c, false)
	image(c)
	c.MarkFlagsOneRequired("device", "image")
	selector(c)
	key(c)
	memory(c)
	consent(c)
	c.Flags().Uint64Var(&o.minBlocks, "min-blocks", 256, "minimum DATA records per missing read window")
	tape := group("tape", "Inspect, synchronize and verify physical tapes")
	leaf(tape, "ls", "List saved physical cartridge facts", cobra.NoArgs, true)
	c = leaf(tape, "probe", "Read drive and MAM facts without movement", cobra.NoArgs, false)
	device(c, true)
	c = leaf(tape, "verify", "Verify a whole tape or inclusive archive range", cobra.NoArgs, true)
	device(c, true)
	selector(c)
	key(c)
	memory(c)
	consent(c)
	c = leaf(tape, "reindex", "Rebuild catalog records in one BOT-to-EOD pass", cobra.NoArgs, true)
	device(c, true)
	key(c)
	memory(c)
	consent(c)
	c.Flags().BoolVar(&o.hardwareCompression, "hardware-compression", false, "explicit future write preference when recovering an unknown set")
	c = leaf(tape, "sync", "Copy complete batches between a tape and sparse image", cobra.NoArgs, true)
	device(c, true)
	key(c)
	memory(c)
	selector(c)
	consent(c)
	c.Flags().StringVar(&o.from, "from", "", "source image for writing tape")
	c.Flags().StringVar(&o.to, "to", "", "destination image for reading tape")
	c.MarkFlagsOneRequired("from", "to")
	c.MarkFlagsMutuallyExclusive("from", "to")
	c.Flags().BoolVar(&o.prune, "prune", false, "release image batches committed on every member (--from only)")
	c = leaf(tape, "eject", "Rewind and unload the cartridge", cobra.NoArgs, false)
	device(c, true)
	consent(c)
	stage := group("stage", "Select and write one complete archive; keep sources unchanged")
	c = leaf(stage, "add [PATH...]", "Add or refresh source paths", cobra.ArbitraryArgs, true)
	c.Flags().StringVar(&o.source, "source", "", "source root")
	c.Flags().StringVar(&o.rules, "rules", "", "JSON include/exclude rules")
	leaf(stage, "rm PATH...", "Unstage paths; . clears the stage", cobra.MinimumNArgs(1), true)
	leaf(stage, "ls [PATH...]", "List staged files", cobra.ArbitraryArgs, true)
	c = leaf(stage, "plan", "Hash, index and budget for a replica set", cobra.NoArgs, true)
	c.Flags().StringVar(&o.replica, "replica", "", "replica name (automatic when only one exists)")
	c.Flags().StringVar(&o.name, "name", "", "archive name")
	c.Flags().BoolVar(&o.noCache, "no-cache", false, "rehash selected files")
	c = leaf(stage, "unlock PATH", "Supply a password for a staged nested archive", cobra.ExactArgs(1), true)
	c.Flags().StringVar(&o.passwordFile, "password-file", "", "private archive password file")
	c.Flags().BoolVar(&o.remember, "remember", false, "save the password in encrypted SQLite")
	c = leaf(stage, "write", "Write a complete batch to an image, tape, or both", cobra.NoArgs, true)
	device(c, false)
	image(c)
	c.MarkFlagsOneRequired("device", "image")
	key(c)
	memory(c)
	consent(c)
	c.Flags().BoolVar(&o.zstd, "zstd", false, "independent per-record software zstd")
	c.Flags().DurationVar(&o.maxWait, "max-wait", 2*time.Second, "maximum unplanned source wait")
	archive := group("archive", "Browse committed archive metadata offline")
	leaf(archive, "ls", "List committed archives and physical replica counts", cobra.NoArgs, true)
	c = leaf(archive, "browse [PATH...]", "Browse files and indexed nested archives", cobra.ArbitraryArgs, true)
	c.Flags().StringVar(&o.archive, "archive", "", "full archive ID")
	_ = c.MarkFlagRequired("archive")
	c.Flags().BoolVar(&o.tui, "tui", false, "interactive offline directory browser")
	c.MarkFlagsMutuallyExclusive("tui", "json")
	keys := group("key", "Create independent recovery keys")
	c = leaf(keys, "generate", "Generate a key and printable recovery card", cobra.NoArgs, false)
	key(c)
	_ = c.MarkFlagRequired("key-file")
	c.Flags().StringVar(&o.keycard, "keycard", "", "new recovery-card file")
	_ = c.MarkFlagRequired("keycard")
	c.Flags().StringVar(&o.name, "name", "", "recovery-card label")
	return root
}
