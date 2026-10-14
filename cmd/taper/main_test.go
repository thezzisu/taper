package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/media"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type cliTape struct {
	media.Device
	reads, locates, keys int
}

func (d *cliTape) ReadRecord() ([]byte, error)          { d.reads++; return d.Device.ReadRecord() }
func (d *cliTape) ReadRecordInto(b []byte) (int, error) { d.reads++; return d.Device.ReadRecordInto(b) }
func (d *cliTape) Locate(p uint64) error                { d.locates++; return d.Device.Locate(p) }
func (d *cliTape) SetKey(k []byte) error                { d.keys++; return d.Device.SetKey(k) }

func TestCLIOfflineWorkflow(t *testing.T) {
	original := openTape
	var last *cliTape
	openTape = func(path string, write bool) (media.Device, error) {
		d, e := media.OpenSim(path, write)
		if e != nil {
			return nil, e
		}
		last = &cliTape{Device: d}
		return last, nil
	}
	defer func() { openTape = original }()
	dir := t.TempDir()
	pass := filepath.Join(dir, "session.pass")
	os.WriteFile(pass, []byte("synthetic CLI password"), 0600)
	db := filepath.Join(dir, "catalog.db")
	base := []string{"--catalog", db, "--pass-file", pass}
	call := func(cmd string, args ...string) {
		t.Helper()
		a := strings.Fields(cmd)
		if cmd != "tape probe" && cmd != "key generate" {
			a = append(a, base...)
		}
		a = append(a, args...)
		if e := run(context.Background(), a); e != nil {
			t.Fatalf("%s: %v", cmd, e)
		}
	}
	call("catalog init")
	sim := filepath.Join(dir, "sim.tape")
	if e := media.CreateSim(sim, 10737418240, true); e != nil {
		t.Fatal(e)
	}
	call("tape register", "--device", sim, "--name", "TEST")
	call("tape ls")
	call("tape probe", "--device", sim)
	root := filepath.Join(dir, "source")
	os.Mkdir(root, 0700)
	os.WriteFile(filepath.Join(root, "hello"), []byte("hello CLI"), 0600)
	call("stage add", "--source", root)
	call("stage ls")
	call("stage plan")
	s, e := catalog.Open(db, []byte("synthetic CLI password"))
	if e != nil {
		t.Fatal(e)
	}
	var id string
	s.DB.QueryRow("SELECT id FROM batches").Scan(&id)
	s.Close()
	if e := run(context.Background(), append(append([]string{"archive", "browse"}, base...), "--archive", id)); e == nil {
		t.Fatal("uncommitted plan was presented as an archive")
	}
	call("stage plan")
	call("stage write", "--device", sim, "--memory", "8388608")
	call("archive ls")
	call("archive browse", "--archive", id)
	var out, stderr bytes.Buffer
	c := newCommand()
	c.SetOut(&out)
	c.SetErr(&stderr)
	c.SetIn(strings.NewReader(""))
	c.SetArgs(append(append([]string{"tape", "verify", id}, base...), "--device", sim, "-yn", "--json"))
	if e = c.ExecuteContext(context.Background()); e != nil {
		t.Fatal(e)
	}
	var plan struct {
		Archives []struct{ ID string }
		CheckEOD bool `json:"check_eod"`
	}
	if e = json.Unmarshal(out.Bytes(), &plan); e != nil || len(plan.Archives) != 1 || plan.Archives[0].ID != id || !plan.CheckEOD {
		t.Fatal("invalid dry-run JSON", out.String(), e)
	}
	if last.reads+last.locates+last.keys != 0 {
		t.Fatal("dry-run touched tape records", last)
	}
	c = newCommand()
	c.SetOut(&out)
	c.SetErr(&stderr)
	c.SetIn(strings.NewReader("yes\n"))
	c.SetArgs(append(append([]string{"tape", "verify"}, base...), "--device", sim, id))
	if e = c.ExecuteContext(context.Background()); e == nil || !strings.Contains(e.Error(), "requires a terminal") {
		t.Fatal("non-interactive confirmation was bypassed", e)
	}
	if last.reads+last.locates+last.keys != 0 {
		t.Fatal("confirmation failure moved tape", last)
	}
	call("tape verify", "--device", sim, "--memory", "8388608", "--yes")
	call("tape reindex", "--device", sim, "--memory", "8388608")
	call("catalog export", "--dest", filepath.Join(dir, "export"))
	call("key generate", "--key-file", filepath.Join(dir, "key"), "--keycard", filepath.Join(dir, "keycard"), "--name", "TEST2")
}
