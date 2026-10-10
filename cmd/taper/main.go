package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/engine"
	"github.com/thezzisu/taper/internal/media"
	"github.com/thezzisu/taper/internal/policy"
	"github.com/thezzisu/taper/internal/ui"
	"golang.org/x/term"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if e := run(ctx, os.Args[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "taper:", e)
		os.Exit(1)
	}
}
func output(v any) error { e := json.NewEncoder(os.Stdout); e.SetIndent("", "  "); return e.Encode(v) }
func password(file string) ([]byte, error) {
	if file != "" {
		b, e := os.ReadFile(file)
		return []byte(strings.TrimRight(string(b), "\r\n")), e
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, errors.New("use --pass-file or run in a terminal")
	}
	fmt.Fprint(os.Stderr, "Catalog passphrase: ")
	b, e := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return b, e
}
func keyFile(file string) ([]byte, error) {
	if file == "" {
		return nil, nil
	}
	b, e := os.ReadFile(file)
	if e != nil {
		return nil, e
	}
	key, e := hex.DecodeString(strings.TrimSpace(string(b)))
	if e != nil || len(key) != 32 {
		return nil, errors.New("tape key file must contain 64 hex digits")
	}
	return key, nil
}

// Tests inject the record-preserving simulator; the public CLI opens real tapes.
var openTape = func(name string, write bool) (media.Device, error) { return media.OpenWorker(name, write) }

func device(name string, write bool) (media.Device, error) {
	if name == "" {
		return nil, errors.New("provide --device /dev/sgN")
	}
	return openTape(name, write)
}
func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stdout, usage)
		return nil
	}
	if args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(os.Stdout, usage)
		return nil
	}
	groups := map[string]map[string]string{
		"catalog": {"init": "init", "export": "export", "import": "import", "rekey": "rekey"},
		"media":   {"init": "media-init", "list": "media-list", "probe": "probe"},
		"stage":   {"add": "stage-add", "rm": "stage-rm", "ls": "stage-ls", "plan": "stage-plan", "write": "stage-write"},
		"archive": {"list": "archive-list"},
		"key":     {"generate": "key-generate"},
	}
	if children, ok := groups[args[0]]; ok {
		if len(args) < 2 {
			return fmt.Errorf("%s requires a subcommand", args[0])
		}
		command, ok := children[args[1]]
		if !ok {
			return fmt.Errorf("unknown %s command %s", args[0], args[1])
		}
		args = append([]string{command}, args[2:]...)
	} else if args[0] != "browse" && args[0] != "verify" && args[0] != "reindex" && args[0] != "device-worker" {
		return fmt.Errorf("unknown command %s; use grouped commands such as media list", args[0])
	}
	command := args[0]
	if _, ok := options[command]; !ok {
		return fmt.Errorf("unknown command %s", command)
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.Usage = func() { help(f, command, os.Stdout) }
	home, _ := os.UserHomeDir()
	db := f.String("catalog", filepath.Join(home, ".local/share/taper/catalog.db"), "encrypted catalog path")
	passFile := f.String("pass-file", "", "private catalog passphrase file")
	sg := f.String("device", "", "Linux /dev/sgN tape device")
	source := f.String("source", "", "frozen source directory")
	name := f.String("name", "", "archive name / cartridge label")
	rulesFile := f.String("rules", "", "JSON include/exclude rules")
	archive := f.String("archive", "", "stored archive UUID")
	copies := f.Int("copies", 0, "required tape copies; 1 for a new plan, otherwise unchanged")
	dest := f.String("dest", "", "catalog export destination")
	keyName := f.String("key-file", "", "private 64-hex-digit tape key file")
	card := f.String("keycard", "", "private generated recovery card path")
	noSave := f.Bool("no-save-key", false, "do not retain tape key in catalog")
	confirmed := f.Bool("key-backed-up", false, "confirm recovery key was saved/printed")
	zstd := f.Bool("zstd", false, "adaptive per-record zstd")
	memory := f.Int64("memory", 1<<30, "bounded stream pool bytes")
	probeMBps := f.Float64("probe-mbps", 0, "read the complete source through the ring at this rate before tape I/O; refuse on starvation")
	maxWait := f.Duration("max-wait", 2*time.Second, "stop writing when the source cannot supply another record within this time")
	frozen := f.Bool("frozen", false, "acknowledge source has stopped changing")
	noCache := f.Bool("no-cache", false, "rehash the source without saved scan progress")
	writeRole := f.Bool("write", false, "device worker write role")
	tui := f.Bool("tui", false, "interactive offline directory browser")
	if e := f.Parse(args[1:]); e != nil {
		return e
	}
	var invalid string
	validOptions := allowed(command)
	f.Visit(func(v *flag.Flag) {
		if !validOptions[v.Name] {
			invalid = v.Name
		}
	})
	if invalid != "" {
		return fmt.Errorf("--%s does not apply to %s", invalid, command)
	}
	if command != "stage-add" && command != "stage-rm" && command != "stage-ls" && command != "browse" && len(f.Args()) > 0 {
		return errors.New("unexpected positional arguments")
	}
	if command == "device-worker" {
		if os.Geteuid() != 0 {
			return errors.New("device worker needs PolicyKit authorization")
		}
		return media.Serve(*sg, *writeRole, os.Stdin, os.Stdout)
	}
	if command == "key-generate" {
		if *keyName == "" || *card == "" {
			return errors.New("--key-file and --keycard required")
		}
		key := make([]byte, 32)
		if _, e := rand.Read(key); e != nil {
			return e
		}
		defer clear(key)
		file, e := os.OpenFile(*keyName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, e = fmt.Fprintln(file, hex.EncodeToString(key))
		if e == nil {
			e = file.Sync()
		}
		file.Close()
		if e != nil {
			return e
		}
		file, e = os.OpenFile(*card, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, e = fmt.Fprintf(file, "TAPER RECOVERY KEY\nLabel: %s\nKey ID: %s\nKey: %s\n", *name, media.KeyID(key), hex.EncodeToString(key))
		if e == nil {
			e = file.Sync()
		}
		file.Close()
		if e != nil {
			return e
		}
		return output(map[string]string{"key_id": media.KeyID(key), "keycard": *card})
	}
	if command == "probe" {
		d, e := device(*sg, false)
		if e != nil {
			return e
		}
		defer d.Close()
		i, e := d.Info()
		if e != nil {
			return e
		}
		return output(i)
	}
	pass, e := password(*passFile)
	if e != nil {
		return e
	}
	defer clear(pass)
	if command == "import" {
		if *source == "" {
			return errors.New("--source export directory required")
		}
		return catalog.Import(*source, *db, pass)
	}
	var s *catalog.Store
	if command == "init" {
		s, e = catalog.Create(*db, pass)
	} else {
		s, e = catalog.Open(*db, pass)
	}
	if e != nil {
		return e
	}
	defer s.Close()
	if command == "init" {
		return output(map[string]string{"catalog": *db})
	}
	if command == "export" {
		if *dest == "" {
			return errors.New("--dest required")
		}
		return s.Export(*dest)
	}
	if command == "rekey" {
		next, e := password("")
		if e != nil {
			return e
		}
		defer clear(next)
		return s.Rekey(next)
	}
	if command == "media-list" {
		rows, e := s.DB.Query("SELECT id,label,serial,total,remaining,sampled,state,key_id FROM media ORDER BY label")
		if e != nil {
			return e
		}
		defer rows.Close()
		var out []map[string]any
		for rows.Next() {
			var id, l, serial, sampled, state, k string
			var total, remaining int64
			if e = rows.Scan(&id, &l, &serial, &total, &remaining, &sampled, &state, &k); e != nil {
				return e
			}
			out = append(out, map[string]any{"id": id, "label": l, "serial": serial, "total": total, "remaining": remaining, "sampled": sampled, "state": state, "key_id": k})
		}
		if e = rows.Err(); e != nil {
			return e
		}
		return output(out)
	}
	key, e := keyFile(*keyName)
	if e != nil {
		return e
	}
	defer clear(key)
	if command == "media-init" {
		if len(key) > 0 && !*confirmed {
			return errors.New("save/print recovery key, then use --key-backed-up")
		}
		d, e := device(*sg, false)
		if e != nil {
			return e
		}
		defer d.Close()
		l, e := engine.RegisterMedia(s, d, *name, key, !*noSave)
		if e != nil {
			return e
		}
		return output(l)
	}
	if command == "verify" || command == "reindex" {
		d, e := device(*sg, false)
		if e != nil {
			return e
		}
		defer d.Close()
		result, e := engine.ScanTape(ctx, s, d, key, command == "reindex", engine.ReadOptions{Memory: *memory, Progress: func(n uint64) { fmt.Fprintf(os.Stderr, "read %d records\n", n) }})
		if oe := output(result); e == nil {
			e = oe
		}
		return e
	}
	if command == "archive-list" {
		rows, e := s.DB.Query("SELECT id,name,created,files,logical_bytes,(SELECT COUNT(DISTINCT media_id) FROM replicas WHERE batch_id=batches.id AND state='committed') FROM batches WHERE state='committed' ORDER BY created")
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var id, n, created string
			var count, size, copies int64
			if e = rows.Scan(&id, &n, &created, &count, &size, &copies); e != nil {
				return e
			}
			fmt.Printf("%s  %s  %s  %d files  %.2f GiB  %d copies\n", id, created, n, count, float64(size)/(1<<30), copies)
		}
		return rows.Err()
	}
	switch command {
	case "stage-add":
		var rules *policy.Rules
		if *rulesFile != "" {
			r, e := policy.Load(*rulesFile)
			if e != nil {
				return e
			}
			rules = &r
		}
		n, e := engine.StageAdd(ctx, s, *source, f.Args(), rules)
		if e != nil {
			return e
		}
		return output(map[string]int{"staged_files": n})
	case "stage-rm":
		n, e := engine.StageRemove(s, f.Args())
		if e != nil {
			return e
		}
		return output(map[string]int{"unstaged_files": n})
	case "stage-ls":
		st, e := engine.LoadStage(s)
		if e != nil {
			return e
		}
		for _, file := range st.Files {
			if selectedPath(file.Path, f.Args()) {
				fmt.Printf("%s\t%d\n", file.Path, file.Size)
			}
		}
		return nil
	case "stage-plan":
		start, last := time.Now(), time.Now()
		m, e := engine.StagePlan(ctx, s, *name, *copies, *frozen, *noCache, func(n int64) {
			if time.Since(last) > 15*time.Second {
				fmt.Fprintf(os.Stderr, "hashed %.2f GiB, %.1f MB/s\n", float64(n)/(1<<30), float64(n)/time.Since(start).Seconds()/1e6)
				last = time.Now()
			}
		})
		if e != nil {
			return e
		}
		recommendations, e := engine.Recommend(s, m)
		if e != nil {
			return e
		}
		status, e := engine.BatchStatus(s, m)
		if e != nil {
			return e
		}
		return output(map[string]any{"plan": m.ID.String(), "files": m.Files, "stream_bytes": m.Logical, "native_budget": engine.Budget(m), "recommendations": recommendations, "status": status, "directories": m.Summaries()})
	case "stage-write":
		d, e := device(*sg, true)
		if e != nil {
			return e
		}
		defer d.Close()
		id, rep, e := engine.StageWrite(ctx, s, d, key, engine.WriteOptions{Memory: *memory, Zstd: *zstd, AllowFrozen: *frozen, ProbeMBps: *probeMBps, MaxWait: *maxWait, Underflow: func(wait time.Duration) { fmt.Fprintf(os.Stderr, "source supply waited %s\n", wait) }, Pause: func() { fmt.Fprintln(os.Stderr, "source buffer refilled; continuing the next write burst") }, Progress: func(n uint64) {
			if n%1024 == 0 {
				fmt.Fprintf(os.Stderr, "wrote %d records\n", n)
			}
		}})
		if oe := output(map[string]string{"archive": id, "replica": rep}); e == nil {
			e = oe
		}
		return e
	case "browse":
		if *archive == "" {
			return errors.New("--archive required")
		}
		m, e := engine.Load(s, *archive)
		if e != nil {
			return e
		}
		if *tui {
			reps, e := engine.Replicas(s, *archive)
			if e != nil {
				return e
			}
			return ui.Browse(m, reps, os.Stdin, os.Stdout)
		}
		for _, c := range m.Contents {
			for _, file := range c.Files {
				if selectedPath(file.Path, f.Args()) {
					fmt.Printf("%s\t%d\t%s\n", file.Path, c.Size, c.Hash)
				}
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown command %s", command)
	}
}

func selectedPath(path string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, prefix := range prefixes {
		prefix = strings.TrimSuffix(prefix, "/")
		if prefix == "." || path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}
