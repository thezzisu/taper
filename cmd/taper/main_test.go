package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/engine"
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

func TestCatalogEnvironmentAndSecureDefaults(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	taperDir := filepath.Join(data, "taper")
	if err := os.MkdirAll(taperDir, 0700); err != nil {
		t.Fatal(err)
	}
	passPath := filepath.Join(taperDir, "session.pass")
	if err := os.WriteFile(passPath, []byte("environment pass"), 0600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(taperDir, "catalog.db")
	s, err := catalog.Create(dbPath, []byte("environment pass"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	t.Run("secure XDG defaults", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", data)
		t.Setenv("TAPER_CATALOG", "")
		t.Setenv("TAPER_PASS_FILE", "")
		c := newCommand()
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetErr(&out)
		c.SetArgs([]string{"replica", "ls"})
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("environment overrides", func(t *testing.T) {
		t.Setenv("TAPER_CATALOG", dbPath)
		t.Setenv("TAPER_PASS_FILE", passPath)
		c := newCommand()
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetErr(&out)
		c.SetArgs([]string{"replica", "ls"})
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unsafe passfile is explicit", func(t *testing.T) {
		unsafePass := filepath.Join(t.TempDir(), "pass")
		if err := os.WriteFile(unsafePass, []byte("environment pass"), 0644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TAPER_CATALOG", dbPath)
		t.Setenv("TAPER_PASS_FILE", unsafePass)
		c := newCommand()
		c.SetOut(&bytes.Buffer{})
		c.SetErr(&bytes.Buffer{})
		c.SetArgs([]string{"replica", "ls"})
		if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "owner-only") {
			t.Fatalf("unsafe passfile was not rejected: %v", err)
		}
	})
}

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
	call("replica create", "TEST")
	call("replica add", "--device", sim, "--label", "TEST")
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
	if e := run(context.Background(), append(append([]string{"archive", "browse"}, base...), id)); e == nil {
		t.Fatal("uncommitted plan was presented as an archive")
	}
	call("stage plan")
	call("stage write", "--device", sim, "--memory", "8388608", "--yes")
	call("archive ls")
	call("archive browse", id)
	call("archive browse", "archive")
	var out, stderr bytes.Buffer
	c := newCommand()
	c.SetOut(&out)
	c.SetErr(&stderr)
	c.SetIn(strings.NewReader(""))
	c.SetArgs(append(append([]string{"tape", "verify", "--archive", id}, base...), "--device", sim, "-yn", "--json"))
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
	c.SetArgs(append(append([]string{"tape", "verify"}, base...), "--device", sim, "--archive", id))
	if e = c.ExecuteContext(context.Background()); e == nil || !strings.Contains(e.Error(), "requires a terminal") {
		t.Fatal("non-interactive confirmation was bypassed", e)
	}
	if last.reads+last.locates+last.keys != 0 {
		t.Fatal("confirmation failure moved tape", last)
	}
	call("tape verify", "--device", sim, "--memory", "8388608", "--yes")
	call("tape reindex", "--device", sim, "--memory", "8388608", "--yes")
	call("catalog export", "--dest", filepath.Join(dir, "export"))
	call("key generate", "--key-file", filepath.Join(dir, "key"), "--keycard", filepath.Join(dir, "keycard"), "--name", "TEST2")
}

func TestLastMemberRemovalRequiresExplicitConfirmation(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "catalog.db")
	pass := filepath.Join(dir, "pass")
	os.WriteFile(pass, []byte("remove test"), 0600)
	s, e := catalog.Create(db, []byte("remove test"))
	if e != nil {
		t.Fatal(e)
	}
	r, e := engine.ReplicaCreate(s, "only", nil, true, false)
	if e != nil {
		t.Fatal(e)
	}
	tape := filepath.Join(dir, "sim")
	media.CreateSim(tape, 8<<30, false)
	d, e := media.OpenSim(tape, true)
	if e != nil {
		t.Fatal(e)
	}
	member, e := engine.ReplicaAdd(s, d, r.ID, "111111L6", "note", nil)
	d.Close()
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	var out, errout bytes.Buffer
	c := newCommand()
	c.SetOut(&out)
	c.SetErr(&errout)
	c.SetIn(strings.NewReader("yes\n"))
	base := []string{"replica", "rm", "--catalog", db, "--pass-file", pass, "--tape", member.ID}
	c.SetArgs(base)
	if e = c.Execute(); e == nil || !strings.Contains(errout.String(), "last cartridge") {
		t.Fatal("last member removal did not warn/require terminal", e, errout.String())
	}
	s, e = catalog.OpenReadOnly(db, []byte("remove test"))
	if e != nil {
		t.Fatal(e)
	}
	var count int
	s.DB.QueryRow("SELECT COUNT(*) FROM media").Scan(&count)
	s.Close()
	if count != 1 {
		t.Fatal("confirmation failure removed member")
	}
	c = newCommand()
	c.SetOut(&out)
	c.SetErr(&errout)
	c.SetArgs(append(base, "--yes"))
	if e = c.Execute(); e != nil {
		t.Fatal(e)
	}
	s, e = catalog.OpenReadOnly(db, []byte("remove test"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	s.DB.QueryRow("SELECT COUNT(*) FROM media").Scan(&count)
	if count != 0 {
		t.Fatal("approved removal failed")
	}
}
func TestSyncDryRunDoesNotCreateImage(t *testing.T) {
	original := openTape
	var last *cliTape
	openTape = func(p string, w bool) (media.Device, error) {
		d, e := media.OpenSim(p, w)
		if e != nil {
			return nil, e
		}
		last = &cliTape{Device: d}
		return last, nil
	}
	defer func() { openTape = original }()
	dir := t.TempDir()
	db := filepath.Join(dir, "catalog.db")
	pass := filepath.Join(dir, "pass")
	os.WriteFile(pass, []byte("dry run"), 0600)
	s, e := catalog.Create(db, []byte("dry run"))
	if e != nil {
		t.Fatal(e)
	}
	r, e := engine.ReplicaCreate(s, "set", nil, true, false)
	if e != nil {
		t.Fatal(e)
	}
	tape := filepath.Join(dir, "sim")
	media.CreateSim(tape, 8<<30, false)
	d, e := media.OpenSim(tape, true)
	if e != nil {
		t.Fatal(e)
	}
	engine.ReplicaAdd(s, d, r.ID, "", "", nil)
	root := filepath.Join(dir, "source")
	os.Mkdir(root, 0700)
	os.WriteFile(filepath.Join(root, "data"), []byte("data"), 0600)
	engine.StageAdd(context.Background(), s, root, nil, nil)
	if _, e = engine.StagePlan(context.Background(), s, "archive", "", false, nil); e != nil {
		t.Fatal(e)
	}
	if _, _, e = engine.StageWrite(context.Background(), s, d, nil, engine.WriteOptions{Memory: 8 << 20}); e != nil {
		t.Fatal(e)
	}
	s.Close()
	d.Close()
	image := filepath.Join(dir, "must-not-exist.img")
	var out bytes.Buffer
	c := newCommand()
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"tape", "sync", "--catalog", db, "--pass-file", pass, "--device", tape, "--to", image, "--dry-run", "--json"})
	if e = c.Execute(); e != nil {
		t.Fatal(e)
	}
	if !json.Valid(out.Bytes()) {
		t.Fatal("invalid JSON", out.String())
	}
	if _, e = os.Stat(image); !os.IsNotExist(e) {
		t.Fatal("dry run created image")
	}
	if last.reads+last.locates+last.keys != 0 {
		t.Fatal("dry run moved tape")
	}
}
