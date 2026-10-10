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
	"github.com/thezzisu/taper/internal/format"
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
func device(sim, sg string, write bool) (media.Device, error) {
	if sim != "" && sg != "" {
		return nil, errors.New("choose --sim or --device")
	}
	if sim != "" {
		return media.OpenSim(sim, write)
	}
	if sg != "" {
		return media.OpenWorker(sg, write)
	}
	return nil, errors.New("provide --sim or --device /dev/sgN")
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
		"media":   {"init": "media-init", "list": "media-list", "seal": "media-seal", "probe": "probe", "inspect": "inspect"},
		"batch":   {"scan": "scan", "check": "check", "plan": "plan", "status": "status", "list": "batch-list", "probe": "probe-supply", "spool": "spool"},
		"key":     {"generate": "key-generate"}, "sim": {"create": "sim-create"},
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
	sim := f.String("sim", "", "simulated tape path")
	sg := f.String("device", "", "Linux /dev/sgN tape device")
	source := f.String("source", "", "frozen source directory")
	name := f.String("name", "batch", "batch name / media label")
	mediumID := f.String("media", "", "media UUID for catalog operations")
	rulesFile := f.String("rules", "", "JSON include/exclude rules")
	batch := f.String("batch", "", "batch UUID")
	dest := f.String("dest", "", "restore / export / rules destination")
	keyName := f.String("key-file", "", "private 64-hex-digit tape key file")
	card := f.String("keycard", "", "private generated recovery card path")
	generate := f.Bool("generate-key", false, "generate independent tape key")
	noSave := f.Bool("no-save-key", false, "do not retain tape key in catalog")
	confirmed := f.Bool("key-backed-up", false, "confirm recovery key was saved/printed")
	capacity := f.Int64("capacity", 10<<30, "simulator native bytes")
	compress := f.Bool("compression", true, "simulated hardware compression")
	zstd := f.Bool("zstd", false, "adaptive per-record zstd")
	memory := f.Int64("memory", 1<<30, "bounded write ring bytes")
	probeMBps := f.Float64("probe-mbps", 0, "read the complete source through the ring at this rate before tape I/O; refuse on starvation")
	maxWait := f.Duration("max-wait", 2*time.Second, "stop writing when the source cannot supply another record within this time")
	frozen := f.Bool("frozen", false, "acknowledge source has stopped changing")
	noCache := f.Bool("no-cache", false, "rehash the source without saved scan progress")
	writeRole := f.Bool("write", false, "device worker write role")
	replica := f.String("replica", "", "replica UUID; default best available on loaded tape")
	savePass := f.String("save-pass", "", "explicitly save session passphrase privately after init")
	tui := f.Bool("tui", false, "interactive offline directory browser")
	restorePlan := f.Bool("plan", false, "show restore plan without accessing a tape")
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
	if command == "restore" && *restorePlan {
		command = "restore-plan"
	}
	if command == "device-worker" {
		if os.Geteuid() != 0 {
			return errors.New("device worker needs PolicyKit authorization")
		}
		return media.Serve(*sg, *writeRole, os.Stdin, os.Stdout)
	}
	if command == "sim-create" {
		if *sim == "" {
			return errors.New("--sim required")
		}
		return media.CreateSim(*sim, *capacity, *compress)
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
	if command == "probe" || command == "inspect" {
		d, e := device(*sim, *sg, false)
		if e != nil {
			return e
		}
		defer d.Close()
		i, e := d.Info()
		if e != nil {
			return e
		}
		if command == "inspect" {
			if e = d.Locate(0); e != nil {
				return e
			}
			b, readErr := d.ReadRecord()
			if readErr == media.ErrEnd {
				return output(map[string]any{"empty": true, "info": i})
			}
			result := map[string]any{"empty": false, "info": i, "record_bytes": len(b)}
			if frame, e := format.DecodeFrame(b, nil); e == nil && frame.Kind == format.Media {
				result["format"] = "taper"
			} else if len(b) >= 262 && string(b[257:262]) == "ustar" {
				result["format"] = "tar"
				if len(b) >= 100 {
					result["first_entry"] = strings.TrimRight(string(b[:100]), "\x00")
				}
			} else if len(b) >= 4 && string(b[:4]) == "VOL1" {
				result["format"] = "volume label"
			} else {
				result["format"] = "unrecognized"
			}
			if readErr != nil {
				result["read_error"] = readErr.Error()
			}
			return output(result)
		}
		return output(i)
	}
	if command == "check" {
		if *source == "" {
			return errors.New("--source required")
		}
		rules, e := policy.Load(*rulesFile)
		if e != nil {
			return e
		}
		v, e := engine.InspectFiles(ctx, *source, rules)
		if e != nil {
			return e
		}
		return output(v)
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
		if *savePass != "" {
			file, e := os.OpenFile(*savePass, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				return e
			}
			_, e = file.Write(pass)
			if e == nil {
				e = file.Sync()
			}
			file.Close()
			if e != nil {
				return e
			}
		}
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
	if command == "media-seal" {
		if *mediumID == "" && len(f.Args()) == 1 {
			*mediumID = f.Args()[0]
		}
		if *mediumID == "" {
			return errors.New("media UUID required")
		}
		r, e := s.DB.Exec("UPDATE media SET state='sealed' WHERE id=?", *mediumID)
		if e != nil {
			return e
		}
		n, _ := r.RowsAffected()
		if n != 1 {
			return errors.New("unknown media")
		}
		return nil
	}
	if command == "scan" {
		if *source == "" {
			return errors.New("--source required")
		}
		rules, e := policy.Load(*rulesFile)
		if e != nil {
			return e
		}
		last := time.Now()
		start := last
		var cache *engine.HashCache
		if !*noCache && (*frozen || engine.ReadOnly(*source) == nil) {
			cache, e = engine.NewHashCache(s, *source)
			if e != nil {
				return e
			}
		}
		m, e := engine.ScanCached(ctx, *source, *name, rules, func(n int64) {
			if time.Since(last) > 15*time.Second {
				fmt.Fprintf(os.Stderr, "scanned %.2f GiB, average %.1f MB/s\n", float64(n)/(1<<30), float64(n)/time.Since(start).Seconds()/1e6)
				last = time.Now()
			}
		}, cache)
		if e != nil {
			return e
		}
		if e = engine.Save(s, m); e != nil {
			return e
		}
		return output(map[string]any{"batch": m.ID.String(), "files": m.Files, "stream_bytes": m.Logical, "bytes_read": m.ReadBytes, "resumed_bytes": m.ResumedBytes, "cached_files": m.CachedFiles, "native_budget": engine.Budget(m), "directories": m.Summaries(), "excluded": m.Excluded})
	}
	key, e := keyFile(*keyName)
	if e != nil {
		return e
	}
	defer clear(key)
	if command == "media-init" {
		if *generate {
			if *card == "" {
				return errors.New("--keycard required for generated key")
			}
			if len(key) > 0 {
				return errors.New("choose generated or provided key")
			}
			key = make([]byte, 32)
			rand.Read(key)
			defer clear(key)
		}
		if len(key) > 0 && !*confirmed {
			return errors.New("save/print recovery key, then use --key-backed-up; generated key requires --keycard")
		}
		// Create card before any encrypted write; include identity after initialization.
		if len(key) > 0 && *card != "" {
			if existing, e := os.ReadFile(*card); e == nil {
				if !strings.Contains(string(existing), "Key ID: "+media.KeyID(key)+"\n") {
					return errors.New("existing keycard is for a different key")
				}
			} else {
				out, e := os.OpenFile(*card, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if e != nil {
					return e
				}
				fmt.Fprintf(out, "TAPER RECOVERY KEY\nLabel: %s\nKey ID: %s\nKey: %s\n", *name, media.KeyID(key), hex.EncodeToString(key))
				e = out.Sync()
				out.Close()
				if e != nil {
					return e
				}
			}
		}
		d, e := device(*sim, *sg, true)
		if e != nil {
			return e
		}
		defer d.Close()
		l, e := engine.LabelMedia(s, d, *name, key, !*noSave)
		if e != nil {
			return e
		}
		if *card != "" {
			out, e := os.OpenFile(*card, os.O_APPEND|os.O_WRONLY, 0600)
			if e != nil {
				return e
			}
			fmt.Fprintln(out, "Media UUID:", l.ID)
			out.Sync()
			out.Close()
		}
		return output(l)
	}
	if command == "reindex" {
		d, e := device(*sim, *sg, false)
		if e != nil {
			return e
		}
		defer d.Close()
		n, e := engine.Reindex(ctx, s, d, key)
		output(map[string]int{"imported_replicas": n})
		return e
	}
	if command == "batch-list" {
		rows, e := s.DB.Query("SELECT id,name,files,logical_bytes,state FROM batches ORDER BY created")
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var id, n, state string
			var count, size int64
			if e = rows.Scan(&id, &n, &count, &size, &state); e != nil {
				return e
			}
			fmt.Printf("%s  %s  %d files  %.2f GiB  %s\n", id, n, count, float64(size)/(1<<30), state)
		}
		return rows.Err()
	}
	if *batch == "" {
		return errors.New("--batch required")
	}
	m, e := engine.Load(s, *batch)
	if e != nil {
		return e
	}
	switch command {
	case "status":
		v, e := engine.BatchStatus(s, m)
		if e != nil {
			return e
		}
		return output(v)
	case "probe-supply":
		if *probeMBps <= 0 {
			return errors.New("--probe-mbps required")
		}
		if !*frozen {
			if e = engine.ReadOnly(m.Source); e != nil {
				return e
			}
		}
		r, e := engine.ProbeSupply(ctx, m, *memory, *probeMBps)
		if e != nil {
			return e
		}
		return output(r)
	case "spool":
		if *dest == "" {
			return errors.New("--dest required")
		}
		if !*frozen {
			if e = engine.ReadOnly(m.Source); e != nil {
				return e
			}
		}
		n, b, e := engine.Spool(ctx, m, *dest, *memory)
		if e != nil {
			return e
		}
		return output(map[string]any{"bytes": n, "blocks": b, "path": *dest})
	case "plan":
		r, e := engine.Recommend(s, m)
		if e != nil {
			return e
		}
		v, e := engine.BatchStatus(s, m)
		if e != nil {
			return e
		}
		return output(map[string]any{"native_budget": engine.Budget(m), "recommendations": r, "status": v})
	case "browse":
		if *tui {
			reps, e := engine.Replicas(s, *batch)
			if e != nil {
				return e
			}
			return ui.Browse(m, reps, os.Stdin, os.Stdout)
		}
		for _, c := range m.Contents {
			for _, file := range c.Files {
				if len(f.Args()) == 0 || strings.HasPrefix(file.Path, f.Args()[0]) {
					fmt.Printf("%s\t%d\t%s\n", file.Path, c.Size, c.Hash)
				}
			}
		}
		return nil
	case "backup":
		d, e := device(*sim, *sg, true)
		if e != nil {
			return e
		}
		defer d.Close()
		id, e := engine.Backup(ctx, s, d, m, key, engine.WriteOptions{Memory: *memory, Zstd: *zstd, AllowFrozen: *frozen, ProbeMBps: *probeMBps, MaxWait: *maxWait, Underflow: func(wait time.Duration) { fmt.Fprintf(os.Stderr, "source supply waited %s\n", wait) }, Progress: func(n uint64) {
			if n%1024 == 0 {
				fmt.Fprintf(os.Stderr, "wrote %d records\n", n)
			}
		}})
		output(map[string]string{"replica": id})
		return e
	case "restore-plan":
		r, e := engine.Replicas(s, *batch)
		if e != nil {
			return e
		}
		plan, e := engine.PlanRestore(m, f.Args(), r)
		if e != nil {
			return e
		}
		return output(plan)
	case "restore", "verify":
		if command == "restore" && *dest == "" {
			return errors.New("--dest required")
		}
		reps, e := engine.Replicas(s, *batch)
		if e != nil {
			return e
		}
		if len(reps) == 0 {
			return errors.New("no committed replica")
		}
		d, e := device(*sim, *sg, false)
		if e != nil {
			return e
		}
		defer d.Close()
		l, e := engine.ReadLabel(d)
		if e != nil {
			return e
		}
		var chosen *engine.Replica
		for i := range reps {
			r := &reps[i]
			if r.Media == l.ID && (*replica == "" || *replica == r.ID) {
				chosen = r
				break
			}
		}
		if chosen == nil {
			return fmt.Errorf("insert a tape from restore-plan")
		}
		n, seeks, e := engine.Restore(ctx, s, d, m, *chosen, *dest, f.Args(), key, command == "verify")
		output(map[string]uint64{"records_read": n, "locates": seeks})
		return e
	default:
		return fmt.Errorf("unknown command %s", command)
	}
}
