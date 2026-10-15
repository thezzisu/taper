package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/thezzisu/taper/internal/engine"
	"github.com/thezzisu/taper/internal/media"
	"github.com/thezzisu/taper/internal/ui"
)

type archiveResult struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Created  string `json:"created"`
	Files    int64  `json:"files"`
	Bytes    int64  `json:"stream_bytes"`
	Replicas int64  `json:"committed_replicas"`
}

type browseResult struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Hash   string `json:"sha256,omitempty"`
	Format string `json:"format,omitempty"`
	State  string `json:"state,omitempty"`
}

func renderResult(w io.Writer, command, device string, value any, machine bool) error {
	if machine {
		e := json.NewEncoder(w)
		e.SetIndent("", "  ")
		return e.Encode(value)
	}
	color := ui.ColorEnabled(w)
	field := func(name string, v any) error {
		_, e := fmt.Fprintf(w, "%s: %s\n", name, ui.Safe(fmt.Sprint(v)))
		return e
	}
	switch v := value.(type) {
	case engine.TapeResult:
		status := "Complete"
		if !v.Complete {
			status = "Stopped"
		}
		if err := field(status, fmt.Sprintf("%d archives, %d objects, %d encoded bytes", v.Archives, v.Records, v.Bytes)); err != nil {
			return err
		}
		if v.EODChecked {
			if err := field("EOD confirmed", v.ReachedEOD); err != nil {
				return err
			}
		}
		for _, issue := range v.Issues {
			if err := field("Issue", issue); err != nil {
				return err
			}
		}
		return nil
	case media.Info:
		label := v.Barcode
		if label == "" {
			label = v.Serial
		}
		if err := ui.TapeHeader(w, label, v.Serial, device); err != nil {
			return err
		}
		rows := []ui.Row{
			{Cells: []string{"Reported total bytes", strconv.FormatInt(v.Total, 10)}},
			{Cells: []string{"Reported remaining bytes", strconv.FormatInt(v.Remaining, 10)}},
			{Cells: []string{"Hardware compression", strconv.FormatBool(v.Compression)}},
			{Cells: []string{"Encryption supported", strconv.FormatBool(v.Encryption)}},
			{Cells: []string{"Early warning", strconv.FormatBool(v.EarlyWarning)}},
			{Cells: []string{"Sampled", v.Sampled}},
		}
		if v.Position != nil {
			rows = append(rows, ui.Row{Cells: []string{"Logical position", strconv.FormatUint(*v.Position, 10)}})
		}
		return ui.Table(w, []string{"FIELD", "VALUE"}, rows)
	case engine.ReplicaSet:
		return ui.Table(w, []string{"REPLICA", "ID", "MEMBERS", "HARDWARE COMPRESSION"}, []ui.Row{{Cells: []string{ui.Safe(v.Name), v.ID, strconv.Itoa(v.Members), strconv.FormatBool(v.HardwareCompression)}}})
	case []engine.ReplicaSet:
		rows := []ui.Row{}
		for _, r := range v {
			rows = append(rows, ui.Row{Cells: []string{ui.Safe(r.Name), r.ID, strconv.Itoa(r.Members), strconv.FormatBool(r.HardwareCompression)}})
		}
		return ui.Table(w, []string{"REPLICA", "ID", "MEMBERS", "HARDWARE COMPRESSION"}, rows)
	case engine.MediaMember:
		if e := ui.TapeHeader(w, v.Label, v.Serial, device); e != nil {
			return e
		}
		if e := field("Replica", v.Replica); e != nil {
			return e
		}
		return field("Note", v.Note)
	case engine.SyncResult:
		return field("Synchronized", fmt.Sprintf("%d archives, %d objects, %d encoded bytes; pruned through %d", len(v.Archives), v.Objects, v.Bytes, v.PrunedEnd))
	case []map[string]any:
		rows := make([]ui.Row, 0, len(v))
		for _, tape := range v {
			budget := "unavailable"
			if tape["write_budget_bytes"] != nil {
				budget = fmt.Sprint(tape["write_budget_bytes"])
			}
			rows = append(rows, ui.Row{Cells: []string{ui.TapeStrip(tape["label"].(string), color), ui.Safe(tape["serial"].(string)), budget, ui.Safe(tape["state"].(string)), ui.Safe(tape["sampled"].(string)), ui.Safe(fmt.Sprint(tape["note"]))}})
		}
		return ui.Table(w, []string{"LABEL", "SERIAL", "WRITE BUDGET (B)", "STATE", "SAMPLED", "NOTE"}, rows)
	case []archiveResult:
		rows := make([]ui.Row, 0, len(v))
		for _, a := range v {
			rows = append(rows, ui.Row{Cells: []string{a.ID, strconv.FormatInt(a.Files, 10), strconv.FormatInt(a.Bytes, 10), strconv.FormatInt(a.Replicas, 10), ui.Safe(a.Name)}})
		}
		return ui.Table(w, []string{"ARCHIVE ID", "FILES", "STREAM BYTES", "REPLICAS", "NAME"}, rows)
	case []engine.InventoryFile:
		rows := make([]ui.Row, 0, len(v))
		for _, f := range v {
			rows = append(rows, ui.Row{Cells: []string{strconv.FormatInt(f.Size, 10), ui.Safe(f.Path)}})
		}
		return ui.Table(w, []string{"BYTES", "PATH"}, rows)
	case []browseResult:
		rows := make([]ui.Row, 0, len(v))
		for _, f := range v {
			rows = append(rows, ui.Row{Cells: []string{strconv.FormatInt(f.Size, 10), ui.Safe(f.State), ui.Safe(f.Path)}})
		}
		return ui.Table(w, []string{"BYTES", "INDEX", "PATH"}, rows)
	case engine.ArchiveIndexSummary:
		return field("Archive index", fmt.Sprintf("%d containers, %d members, %d locked, %d issues", v.Containers, v.Members, len(v.Locked), len(v.Issues)))
	case map[string]any:
		if command == "stage plan" {
			for _, k := range []string{"plan", "files", "stream_bytes", "encoded_bytes", "data_records"} {
				if err := field(strings.ReplaceAll(k, "_", " "), v[k]); err != nil {
					return err
				}
			}
			if err := renderResult(w, command, device, v["index"], false); err != nil {
				return err
			}
			rows := []ui.Row{}
			for _, r := range v["recommendations"].([]engine.Recommendation) {
				label, budget := ui.TapeStrip(r.Label, color), strconv.FormatInt(r.Remaining, 10)
				if r.ID == "" {
					label, budget = "<New tape>", "-"
				}
				rows = append(rows, ui.Row{Cells: []string{label, budget, ui.Safe(r.Reason)}})
			}
			return ui.Table(w, []string{"TAPE", "WRITE BUDGET (B)", "RECOMMENDATION"}, rows)
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := field(strings.ReplaceAll(k, "_", " "), v[k]); err != nil {
				return err
			}
		}
		return nil
	case map[string]string:
		values := make(map[string]any, len(v))
		for k, value := range v {
			values[k] = value
		}
		return renderResult(w, command, device, values, false)
	case map[string]int:
		values := make(map[string]any, len(v))
		for k, value := range v {
			values[k] = value
		}
		return renderResult(w, command, device, values, false)
	default:
		return fmt.Errorf("no human renderer for %T", value)
	}
}
