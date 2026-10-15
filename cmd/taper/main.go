package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	container "github.com/thezzisu/taper/internal/archive"
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
		fmt.Fprintln(os.Stderr, "taper:", e)
		os.Exit(1)
	}
}
func password(file string) ([]byte, error) {
	if file != "" {
		st, e := os.Stat(file)
		if e != nil {
			return nil, e
		}
		if st.Mode().Perm()&0077 != 0 {
			return nil, fmt.Errorf("passphrase file %s must be owner-only (mode 0600; current mode %04o)", file, st.Mode().Perm())
		}
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
var openTape = func(name string, write bool) (media.Device, error) { return media.OpenSG(name, write) }

func device(name string, write bool) (media.Device, error) {
	if name == "" {
		return nil, errors.New("provide --device /dev/sgN")
	}
	return openTape(name, write)
}
func execute(ctx context.Context, command string, paths []string, o *settings, c *cobra.Command) error {
	db, passFile := o.catalog, o.passFile
	sg, source, name, rulesFile := &o.device, &o.source, &o.name, &o.rules
	dest := &o.dest
	keyName, card := &o.keyFile, &o.keycard
	memory, noCache, tui := &o.memory, &o.noCache, &o.tui
	archivePasswordFile, remember := &o.passwordFile, &o.remember
	output := func(v any) error { return renderResult(c.OutOrStdout(), command, *sg, v, o.json) }
	if o.json && *tui {
		return errors.New("--json cannot be combined with --tui")
	}
	if (command == "tape verify" || command == "tape reindex" || command == "stage write" || command == "tape sync" || command == "replica mount") && (*memory < 8<<20 || *memory > engine.MaxMemory) {
		return errors.New("--memory must be between 8388608 and 68719476736 bytes")
	}
	if command == "key generate" {
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
	if command == "tape probe" {
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
	if command == "tape eject" {
		d, e := device(*sg, !o.dryRun)
		if e != nil {
			return e
		}
		defer d.Close()
		i, e := d.Info()
		if e != nil {
			return e
		}
		if o.dryRun {
			return output(i)
		}
		label := i.Barcode
		if label == "" {
			label = i.Serial
		}
		if e = ui.TapeHeader(c.ErrOrStderr(), label, i.Serial, *sg); e != nil {
			return e
		}
		if !o.yes {
			if e = ui.Confirm(c.InOrStdin(), c.ErrOrStderr(), ui.IsTerminal(c.InOrStdin())); e != nil {
				return e
			}
		}
		if e = d.Eject(); e != nil {
			return e
		}
		return output(map[string]any{"serial": i.Serial, "ejected": true})
	}
	pass, e := password(passFile)
	if e != nil {
		return e
	}
	defer clear(pass)
	if command == "catalog import" {
		if *source == "" {
			return errors.New("--source export directory required")
		}
		if e = catalog.Import(*source, db, pass); e != nil {
			return e
		}
		return output(map[string]string{"catalog": db})
	}
	var s *catalog.Store
	if command == "catalog init" {
		s, e = catalog.Create(db, pass)
	} else if o.dryRun || command == "tape sync" && o.to != "" || command == "replica mount" || command == "tape ls" || command == "replica ls" || strings.HasPrefix(command, "archive ") || command == "stage ls" {
		s, e = catalog.OpenReadOnly(db, pass)
	} else {
		s, e = catalog.Open(db, pass)
	}
	if e != nil {
		return e
	}
	defer s.Close()
	if command == "catalog init" {
		return output(map[string]string{"catalog": db})
	}
	if command == "catalog export" {
		if *dest == "" {
			return errors.New("--dest required")
		}
		if e = s.Export(*dest); e != nil {
			return e
		}
		return output(map[string]string{"export": *dest})
	}
	if command == "catalog rekey" {
		next, e := password("")
		if e != nil {
			return e
		}
		defer clear(next)
		if e = s.Rekey(next); e != nil {
			return e
		}
		return output(map[string]string{"catalog": db, "status": "passphrase changed"})
	}
	if command == "tape ls" {
		rows, e := s.DB.Query("SELECT id,label,note,serial,total,remaining,sampled,state,key_id,hardware_compression,COALESCE((SELECT info FROM media_health WHERE media_id=media.id),'{}'),(SELECT COUNT(*) FROM replicas WHERE media_id=media.id AND state IN ('writing','failed')),COALESCE((SELECT unhealthy FROM media_health WHERE media_id=media.id),0) FROM media ORDER BY label")
		if e != nil {
			return e
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var id, l, note, serial, sampled, state, k, savedInfo string
			var total, remaining int64
			var incomplete int
			var compression *bool
			var unhealthy bool
			if e = rows.Scan(&id, &l, &note, &serial, &total, &remaining, &sampled, &state, &k, &compression, &savedInfo, &incomplete, &unhealthy); e != nil {
				return e
			}
			var info media.Info
			if e = json.Unmarshal([]byte(savedInfo), &info); e != nil {
				return e
			}
			info.Total, info.Remaining = total, remaining
			var budget any
			if limit, available, err := info.Budget(); err == nil && incomplete == 0 && !unhealthy && compression != nil && (state == "open" || state == "registered") {
				budget = max(0, available-engine.Reserve(limit))
			}
			out = append(out, map[string]any{"id": id, "label": l, "note": note, "serial": serial, "reported_total_bytes": total, "reported_remaining_bytes": remaining, "native_limit_bytes": info.NativeLimit, "write_budget_bytes": budget, "logical_position": info.Position, "sampled": sampled, "state": state, "key_id": k, "hardware_compression": compression, "early_warning": info.EarlyWarning, "incomplete_writes": incomplete, "unhealthy": unhealthy, "tape_alerts": info.Alerts})
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
	if command == "replica create" || command == "replica ls" || command == "replica add" || command == "replica rm" || command == "replica mount" || command == "tape sync" || command == "tape verify" || command == "tape reindex" || command == "stage write" {
		return executeMedia(ctx, command, paths, o, c, s, key)
	}
	if command == "archive ls" {
		rows, e := s.DB.Query("SELECT id,name,created,files,logical_bytes,(SELECT COUNT(DISTINCT media_id) FROM replicas WHERE batch_id=batches.id AND state='committed') FROM batches WHERE state='committed' ORDER BY created")
		if e != nil {
			return e
		}
		defer rows.Close()
		out := []archiveResult{}
		for rows.Next() {
			var item archiveResult
			if e = rows.Scan(&item.ID, &item.Name, &item.Created, &item.Files, &item.Bytes, &item.Replicas); e != nil {
				return e
			}
			out = append(out, item)
		}
		if e = rows.Err(); e != nil {
			return e
		}
		return output(out)
	}
	switch command {
	case "stage add":
		var rules *policy.Rules
		if *rulesFile != "" {
			r, e := policy.Load(*rulesFile)
			if e != nil {
				return e
			}
			rules = &r
		}
		n, e := engine.StageAdd(ctx, s, *source, paths, rules)
		if e != nil {
			return e
		}
		return output(map[string]int{"staged_files": n})
	case "stage rm":
		n, e := engine.StageRemove(s, paths)
		if e != nil {
			return e
		}
		return output(map[string]int{"unstaged_files": n})
	case "stage ls":
		st, e := engine.LoadStage(s)
		if e != nil {
			return e
		}
		out := []engine.InventoryFile{}
		for _, file := range st.Files {
			if selectedPath(file.Path, paths) {
				out = append(out, file)
			}
		}
		return output(out)
	case "stage plan":

		progress := ui.NewProgress(c.ErrOrStderr(), "Planning", -1)
		m, e := engine.StagePlan(ctx, s, *name, o.replica, *noCache, func(n int64) {
			progress.Update(uint64(n), n)
		})
		progress.Close(e == nil)
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
		index, e := engine.SummarizeArchiveIndex(s, m)
		if e != nil {
			return e
		}
		return output(map[string]any{"plan": m.ID.String(), "files": m.Files, "stream_bytes": m.Logical, "encoded_bytes": engine.Budget(m), "data_records": 1 + (m.Logical-1)/format.PayloadSize, "recommendations": recommendations, "status": status, "directories": m.Summaries(), "index": index})
	case "stage unlock":
		if len(paths) != 1 {
			return errors.New("provide one staged container path; inner containers use outer.7z!/inner.zip")
		}
		target := paths[0]
		prompt := func(path string) ([]byte, error) {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return nil, nil
			}
			fmt.Fprintf(os.Stderr, "Archive password for %s (empty skips): ", path)
			p, e := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			if len(p) == 0 {
				return nil, e
			}
			return p, e
		}
		var archivePass []byte
		if *archivePasswordFile != "" {
			archivePass, e = os.ReadFile(*archivePasswordFile)
			archivePass = []byte(strings.TrimRight(string(archivePass), "\r\n"))
		} else {
			archivePass, e = prompt(target)
		}
		if e != nil {
			return e
		}
		defer clear(archivePass)
		if archivePass == nil {
			return errors.New("provide --password-file or enter the archive password in a terminal")
		}
		e = engine.StageUnlock(ctx, s, target, archivePass, *remember, prompt)
		if e != nil {
			return e
		}
		st, e := engine.LoadStage(s)
		if e != nil {
			return e
		}
		m, e := engine.Load(s, st.Plan)
		if e != nil {
			return e
		}
		index, e := engine.SummarizeArchiveIndex(s, m)
		if e != nil {
			return e
		}
		return output(index)
	case "archive browse":
		selector := paths[0]
		browsePaths := paths[1:]
		m, e := engine.LoadArchive(s, selector)
		if e != nil {
			return e
		}
		index, e := engine.ArchiveEntries(s, m.ID.String())
		if e != nil {
			return e
		}
		if *tui {
			reps, e := engine.Replicas(s, m.ID.String())
			if e != nil {
				return e
			}
			return ui.Browse(m, reps, index, c.InOrStdin(), c.OutOrStdout())
		}
		out := []browseResult{}
		for _, c := range m.Contents {
			for _, file := range c.Files {
				if selectedPath(file.Path, browsePaths) {
					out = append(out, browseResult{Path: file.Path, Size: c.Size, Hash: c.Hash})
				}
			}
		}
		for _, entry := range index {
			for _, file := range m.Contents[entry.Ordinal].Files {
				if !container.IsArchiveName(file.Path) {
					continue
				}
				path := file.Path + "!/" + entry.Path
				if selectedPath(path, browsePaths) {
					out = append(out, browseResult{Path: path, Size: entry.Size, Format: entry.Format, State: entry.State})
				}
			}
		}
		return output(out)
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
