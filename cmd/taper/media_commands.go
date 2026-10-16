package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/engine"
	"github.com/thezzisu/taper/internal/fusefs"
	"github.com/thezzisu/taper/internal/media"
	"github.com/thezzisu/taper/internal/ui"
)

func confirm(c *cobra.Command, o *settings) error {
	if o.yes {
		return nil
	}
	return ui.Confirm(c.InOrStdin(), c.ErrOrStderr(), ui.IsTerminal(c.InOrStdin()))
}
func scope(c *cobra.Command, o *settings, m *engine.MediaMember, replica, action string, batches []engine.BatchMeta) error {
	w := c.ErrOrStderr()
	if o.dryRun {
		w = c.OutOrStdout()
	}
	if m != nil {
		if e := ui.TapeHeader(w, m.Label, m.Serial, o.device); e != nil {
			return e
		}
	}
	fmt.Fprintf(w, "Action     : %s\n", ui.Safe(action))
	fmt.Fprintf(w, "Replica    : %s\n", ui.Safe(replica))
	if o.image != "" {
		fmt.Fprintf(w, "Image      : %s\n", ui.Safe(o.image))
	}
	var rows []ui.Row
	for _, b := range batches {
		rows = append(rows, ui.Row{Cells: []string{strconv.Itoa(b.Seq), b.ID, fmt.Sprintf("%d-%d", b.BeginPos, b.EndPos-1), ui.Safe(b.Name)}})
	}
	return ui.Table(w, []string{"SEQ", "ARCHIVE ID", "OBJECTS", "NAME"}, rows)
}

func replicaSyncScope(c *cobra.Command, o *settings, p engine.SyncPlan, from, to string) error {
	w := c.ErrOrStderr()
	if o.dryRun {
		w = c.OutOrStdout()
	}
	if p.FromTape != nil {
		if err := ui.TapeHeader(w, p.FromTape.Label, p.FromTape.Serial, from); err != nil {
			return err
		}
	}
	if p.ToTape != nil {
		if err := ui.TapeHeader(w, p.ToTape.Label, p.ToTape.Serial, to); err != nil {
			return err
		}
	}
	fmt.Fprintf(w, "Action     : Synchronize replica stream\n")
	fmt.Fprintf(w, "Replica    : %s\n", ui.Safe(p.Replica.Name))
	fmt.Fprintf(w, "From       : %s\n", ui.Safe(endpointText(p.From, from)))
	fmt.Fprintf(w, "To         : %s\n", ui.Safe(endpointText(p.To, to)))
	if p.To == "image" {
		fmt.Fprintf(w, "Allocation : At most %d additional bytes\n", p.ImageBytes)
	}
	if o.prune {
		fmt.Fprintln(w, "Prune      : Source image batches committed on every member")
	}
	if len(p.Archives) == 0 {
		_, err := fmt.Fprintln(w, "No archives require synchronization.")
		return err
	}
	rows := make([]ui.Row, 0, len(p.Archives))
	for _, b := range p.Archives {
		rows = append(rows, ui.Row{Cells: []string{strconv.Itoa(b.Seq), b.ID, fmt.Sprintf("%d-%d", b.BeginPos, b.EndPos-1), ui.Safe(b.Name)}})
	}
	return ui.Table(w, []string{"SEQ", "ARCHIVE ID", "OBJECTS", "NAME"}, rows)
}

func endpointText(kind, value string) string {
	if value == "" {
		return kind
	}
	return kind + " " + value
}
func executeMedia(ctx context.Context, command string, paths []string, o *settings, c *cobra.Command, s *catalog.Store, key []byte) error {
	output := func(v any) error { return renderResult(c.OutOrStdout(), command, o.device, v, o.json) }
	switch command {
	case "replica create":
		r, e := engine.ReplicaCreate(s, paths[0], key, !o.noSaveKey, o.hardwareCompression)
		if e != nil {
			return e
		}
		return output(r)
	case "replica ls":
		r, e := engine.ReplicaList(s)
		if e != nil {
			return e
		}
		return output(r)
	case "replica add":
		name := ""
		if len(paths) > 0 {
			name = paths[0]
		}
		d, e := device(o.device, false)
		if e != nil {
			return e
		}
		defer d.Close()
		m, e := engine.ReplicaAdd(s, d, name, o.label, o.note, key)
		if e != nil {
			return e
		}
		return output(m)
	case "replica rm":
		name := ""
		if len(paths) > 0 {
			name = paths[0]
		}
		var d media.Device
		var e error
		if o.device != "" {
			d, e = device(o.device, false)
			if e != nil {
				return e
			}
			defer d.Close()
		}
		p, e := engine.PlanReplicaRemoval(s, name, d, o.tape)
		if e != nil {
			return e
		}
		if o.dryRun {
			if o.json {
				return output(p)
			}
			return scope(c, o, &p.MediaMember, p.Replica, "Remove cartridge", nil)
		}
		if e = scope(c, o, &p.MediaMember, p.Replica, "Remove cartridge", nil); e != nil {
			return e
		}
		if p.Last {
			fmt.Fprintln(c.ErrOrStderr(), "Warning: removing the last cartridge from this replica set.")
			if e = confirm(c, o); e != nil {
				return e
			}
		}
		if e = engine.ReplicaRemove(s, p); e != nil {
			return e
		}
		return output(map[string]any{"removed": p.ID, "replica_id": p.Replica})
	case "tape verify":
		d, e := device(o.device, false)
		if e != nil {
			return e
		}
		defer d.Close()
		p, e := engine.PlanVerification(s, d, o.archive)
		if e != nil {
			return e
		}
		if o.dryRun {
			if o.json {
				return output(p)
			}
			return ui.VerificationPlan(c.OutOrStdout(), o.device, p)
		}
		if e = ui.VerificationPlan(c.ErrOrStderr(), o.device, p); e != nil {
			return e
		}
		if e = confirm(c, o); e != nil {
			return e
		}
		progress := ui.NewProgress(c.ErrOrStderr(), "Verifying", int64(p.End-p.Start))
		result, e := engine.VerifyTape(ctx, s, d, p, key, engine.ReadOptions{Memory: o.memory, Progress: progress.Update})
		progress.SetFinal(result.Records, result.Bytes)
		progress.Close(e == nil)
		if oe := output(result); e == nil {
			e = oe
		}
		return e
	case "tape reindex":
		d, e := device(o.device, false)
		if e != nil {
			return e
		}
		defer d.Close()
		l, e := engine.ReadLabel(d)
		if e != nil {
			return e
		}
		p := map[string]any{"replica_id": l.ID, "label": l.Name, "serial": l.Serial, "scope": "BOT through EOD"}
		if o.dryRun {
			if o.json {
				return output(p)
			}
			if e = ui.TapeHeader(c.OutOrStdout(), l.Name, l.Serial, o.device); e != nil {
				return e
			}
			fmt.Fprintln(c.OutOrStdout(), "Scope      : Entire tape, BOT through EOD")
			return nil
		}
		if e = ui.TapeHeader(c.ErrOrStderr(), l.Name, l.Serial, o.device); e != nil {
			return e
		}
		fmt.Fprintln(c.ErrOrStderr(), "Scope      : Entire tape, BOT through EOD")
		if e = confirm(c, o); e != nil {
			return e
		}
		var compression *bool
		if c.Flags().Changed("hardware-compression") {
			compression = &o.hardwareCompression
		}
		progress := ui.NewProgress(c.ErrOrStderr(), "Reindexing", -1)
		result, e := engine.ReindexTape(ctx, s, d, key, engine.ReadOptions{Memory: o.memory, Progress: progress.Update, HardwareCompression: compression})
		progress.SetFinal(result.Records, result.Bytes)
		progress.Close(e == nil)
		if oe := output(result); e == nil {
			e = oe
		}
		return e
	case "replica sync":
		return executeReplicaSync(ctx, o, c, s, key, paths)
	case "stage write":
		st, e := engine.LoadStage(s)
		if e != nil {
			return e
		}
		if st.Plan == "" {
			return errors.New("run stage plan first")
		}
		m, e := engine.Load(s, st.Plan)
		if e != nil {
			return e
		}
		r, e := engine.ResolveReplicaSet(s, m.ReplicaID)
		if e != nil {
			return e
		}
		var d media.Device
		var member *engine.MediaMember
		if o.device != "" {
			d, e = device(o.device, !o.dryRun)
			if e != nil {
				return e
			}
			defer d.Close()
			v, e := engine.CheckWriteTarget(s, d, engine.Meta(m))
			if e != nil {
				return e
			}
			member = &v
		}
		if o.dryRun {
			if o.json {
				return output(map[string]any{"replica": r, "tape": member, "image": o.image, "archives": []engine.BatchMeta{engine.Meta(m)}})
			}
			return scope(c, o, member, r.Name, "Write archive", []engine.BatchMeta{engine.Meta(m)})
		}
		if e = scope(c, o, member, r.Name, "Write archive", []engine.BatchMeta{engine.Meta(m)}); e != nil {
			return e
		}
		if d != nil {
			if e = confirm(c, o); e != nil {
				return e
			}
		}
		var img *media.Image
		if o.image != "" {
			img, e = media.OpenImage(o.image, true, r.ID, r.KeyID)
			if e != nil {
				return e
			}
			defer img.Close()
		}
		total := int64(m.Blocks)
		if d != nil && img != nil {
			total *= 2
		}
		progress := ui.NewProgress(c.ErrOrStderr(), "Writing", total)
		id, rep, e := engine.StageWrite(ctx, s, d, key, engine.WriteOptions{Image: img, Memory: o.memory, Zstd: o.zstd, MaxWait: o.maxWait, Progress: progress.Update})
		progress.Close(e == nil)
		if e != nil {
			return e
		}
		return output(map[string]any{"archive": id, "physical_replica": rep, "image": o.image})
	case "replica mount":
		return executeMount(ctx, o, c, s, key, paths)
	}
	return fmt.Errorf("unsupported operation %s", command)
}
func executeReplicaSync(ctx context.Context, o *settings, c *cobra.Command, s *catalog.Store, key []byte, paths []string) (err error) {
	name := ""
	if len(paths) == 1 {
		name = paths[0]
	}
	if o.prune && (o.fromImage == "" || o.toDevice == "") {
		return errors.New("--prune requires --from-image and --to-device")
	}
	r, err := engine.ResolveReplicaSet(s, name)
	if err != nil {
		return err
	}
	fromPath, toPath := o.fromDevice, o.toDevice
	if o.fromImage != "" {
		fromPath = o.fromImage
	}
	if o.toImage != "" {
		toPath = o.toImage
	}
	if fromPath == "" || toPath == "" {
		return errors.New("source and destination paths must not be empty")
	}
	fromInfo, err := os.Stat(fromPath)
	if err != nil {
		return err
	}
	toInfo, toErr := os.Stat(toPath)
	if toErr != nil && !os.IsNotExist(toErr) {
		return toErr
	}
	if toErr == nil && os.SameFile(fromInfo, toInfo) {
		return errors.New("source and destination refer to the same file or device")
	}
	var source, target media.Device
	if o.fromImage != "" {
		if fromInfo.Size() == 0 {
			return errors.New("source image is empty")
		}
		source, err = media.OpenImage(fromPath, o.prune && !o.dryRun, r.ID, r.KeyID)
	} else {
		source, err = device(fromPath, false)
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, source.Close()) }()
	if o.toDevice != "" {
		target, err = device(toPath, !o.dryRun)
	} else if toErr == nil && toInfo.Size() != 0 {
		target, err = media.OpenImage(toPath, !o.dryRun, r.ID, r.KeyID)
	}
	if err != nil {
		return err
	}
	defer func() {
		if target != nil {
			err = errors.Join(err, target.Close())
		}
	}()
	plan, err := engine.PlanSync(s, r.ID, source, target, o.archive)
	if err != nil {
		return err
	}
	if o.dryRun && o.json {
		return renderResult(c.OutOrStdout(), "replica sync", "", map[string]any{"plan": plan, "source": fromPath, "destination": toPath, "prune": o.prune}, true)
	}
	if err = replicaSyncScope(c, o, plan, fromPath, toPath); err != nil {
		return err
	}
	if o.dryRun {
		return nil
	}
	if len(plan.Archives) > 0 && (o.fromDevice != "" || o.toDevice != "") {
		if err = confirm(c, o); err != nil {
			return err
		}
	}
	if target == nil {
		if len(plan.Archives) == 0 {
			return renderResult(c.OutOrStdout(), "replica sync", "", engine.SyncResult{Archives: []string{}}, o.json)
		}
		target, err = media.OpenImage(toPath, true, r.ID, r.KeyID)
		if err != nil {
			return err
		}
	}
	progress := ui.NewProgress(c.ErrOrStderr(), "Synchronizing", int64(plan.Objects))
	out, err := engine.ReplicaSync(ctx, s, source, target, plan, key, o.prune, engine.ReadOptions{Memory: o.memory, Progress: progress.Update})
	progress.SetFinal(out.Objects, out.Bytes)
	progress.Close(err == nil)
	if e := renderResult(c.OutOrStdout(), "replica sync", "", out, o.json); err == nil {
		err = e
	}
	return err
}
func executeMount(ctx context.Context, o *settings, c *cobra.Command, s *catalog.Store, key []byte, paths []string) error {
	name := ""
	mountpoint := paths[len(paths)-1]
	if len(paths) == 2 {
		name = paths[0]
	}
	r, e := engine.ResolveReplicaSet(s, name)
	if e != nil {
		return e
	}
	all, e := engine.ListBatches(s, r.ID)
	if e != nil {
		return e
	}
	for len(all) > 0 && all[len(all)-1].State != "committed" {
		all = all[:len(all)-1]
	}
	selected, e := engine.ParseArchiveRange(o.archive, all)
	if e != nil {
		return e
	}
	var d media.Device
	var member *engine.MediaMember
	var onTape []engine.BatchMeta
	if o.device != "" {
		d, e = device(o.device, false)
		if e != nil {
			return e
		}
		defer d.Close()
		l, e := engine.ReadLabel(d)
		if e != nil {
			return e
		}
		if l.ID != r.ID || l.KeyID != r.KeyID {
			return errors.New("loaded tape is outside selected replica set")
		}
		m, e := engine.MemberForSerial(s, l.Serial)
		if e != nil {
			return e
		}
		member = &m
		prefix, e := engine.MemberPrefix(s, m.ID, all)
		if e != nil {
			return e
		}
		onTape = all[:prefix]
	}
	if o.dryRun {
		if o.json {
			return renderResult(c.OutOrStdout(), "replica mount", o.device, map[string]any{"replica": r, "tape": member, "archives": selected, "image": o.image, "mountpoint": mountpoint}, true)
		}
		return scope(c, o, member, r.Name, "Mount archive tree", selected)
	}
	if e = scope(c, o, member, r.Name, "Mount archive tree", selected); e != nil {
		return e
	}
	if d != nil {
		if e = confirm(c, o); e != nil {
			return e
		}
	}
	k, e := engine.ReplicaKey(s, r.ID, key)
	if e != nil {
		return e
	}
	defer clear(k)
	file := o.image
	temporary := false
	if file == "" {
		f, e := os.CreateTemp("", "taper-*.img")
		if e != nil {
			return e
		}
		file = f.Name()
		temporary = true
		f.Close()
		defer os.Remove(file)
	}
	img, e := media.OpenImage(file, d != nil, r.ID, r.KeyID)
	if e != nil {
		return e
	}
	defer img.Close()
	if temporary {
		if e = os.Remove(file); e != nil {
			return e
		}
	}
	if e = img.SetKey(k); e != nil {
		return e
	}
	if d != nil {
		if _, e = engine.UnlockMedia(s, d, k); e != nil {
			return e
		}
	}
	manifests := make([]engine.Manifest, 0, len(selected))
	for _, b := range selected {
		m, e := engine.Load(s, b.ID)
		if e != nil {
			return e
		}
		manifests = append(manifests, m)
	}
	reader, e := fusefs.NewReader(ctx, img, d, onTape, o.minBlocks, uint64(o.memory))
	if e != nil {
		return e
	}
	defer reader.Close()
	tree, e := fusefs.NewTree(manifests, reader)
	if e != nil {
		return e
	}
	return fusefs.Mount(ctx, mountpoint, tree)
}
