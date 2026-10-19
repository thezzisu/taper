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
	c.SetArgs([]string{"replica", "sync", "--catalog", db, "--pass-file", pass, "--from-device", tape, "--to-image", image, "--dry-run", "--json"})
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

func TestReplicaSyncEndpointWorkflow(t *testing.T) {
	original := openTape
	opens := 0
	openTape = func(p string, w bool) (media.Device, error) { opens++; return media.OpenSim(p, w) }
	defer func() { openTape = original }()
	dir := t.TempDir()
	db, pass := filepath.Join(dir, "catalog.db"), filepath.Join(dir, "pass")
	if e := os.WriteFile(pass, []byte("sync cli"), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("TAPER_CATALOG", db)
	t.Setenv("TAPER_PASS_FILE", pass)
	call := func(args ...string) (string, error) {
		c := newCommand()
		var out, stderr bytes.Buffer
		c.SetOut(&out)
		c.SetErr(&stderr)
		c.SetIn(strings.NewReader(""))
		c.SetArgs(args)
		err := c.Execute()
		return out.String(), err
	}
	must := func(args ...string) string {
		t.Helper()
		out, e := call(args...)
		if e != nil {
			t.Fatal(args, e)
		}
		if !json.Valid([]byte(out)) {
			t.Fatal("invalid JSON", out)
		}
		return out
	}
	must("catalog", "init", "--json")
	key := filepath.Join(dir, "key")
	must("key", "generate", "--key-file", key, "--keycard", filepath.Join(dir, "card"), "--json")
	must("replica", "create", "set", "--key-file", key, "--json")
	sourceTape, targetTape := filepath.Join(dir, "source.tape"), filepath.Join(dir, "target.tape")
	for _, file := range []string{sourceTape, targetTape} {
		if e := media.CreateSim(file, 8<<30, false); e != nil {
			t.Fatal(e)
		}
		must("replica", "add", "--device", file, "--json")
	}
	root := filepath.Join(dir, "source")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	data := bytes.Repeat([]byte("replica endpoint flow\n"), 150000)
	files := map[string][]byte{"hello": []byte("first archive"), "dir/内容.bin": data, "alias.bin": data, "empty": {}}
	for path, content := range files {
		if e := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(root, path), content, 0600); e != nil {
			t.Fatal(e)
		}
	}
	ssd, hdd, capture := filepath.Join(dir, "ssd.img"), filepath.Join(dir, "hdd.img"), filepath.Join(dir, "capture.img")
	var archives []mountArchive
	for _, name := range []string{"raw", "compressed"} {
		must("stage", "add", "--source", root, "--json")
		var plan struct{ Plan string }
		if e := json.Unmarshal([]byte(must("stage", "plan", "--name", name, "--json")), &plan); e != nil {
			t.Fatal(e)
		}
		args := []string{"stage", "write", "--image", ssd, "--memory", "8388608", "--json"}
		if name == "compressed" {
			args = append(args, "--zstd")
		}
		must(args...)
		archives = append(archives, mountArchive{id: plan.Plan, name: name})
	}
	before, _ := os.ReadFile(db)
	priorOpens := opens
	for _, args := range [][]string{
		{"replica", "sync", "--from-image", ssd, "--to-image", hdd, "-n", "--json"},
		{"replica", "sync", "--from-image", ssd, "--to-image", hdd, "-n"},
	} {
		out, e := call(args...)
		if e != nil {
			t.Fatal(e)
		}
		if !strings.Contains(out, ssd) || !strings.Contains(out, hdd) {
			t.Fatal("endpoints missing from preview", out)
		}
		if _, e = os.Stat(hdd); !os.IsNotExist(e) {
			t.Fatal("dry-run created destination")
		}
	}
	must("replica", "sync", "--from-image", ssd, "--to-image", hdd, "--memory", "8388608", "--json")
	after, _ := os.ReadFile(db)
	if opens != priorOpens || !bytes.Equal(before, after) {
		t.Fatal("image-only sync accessed tape or changed catalog")
	}
	must("replica", "sync", "--from-image", ssd, "--to-device", sourceTape, "-y", "--memory", "8388608", "--json")
	must("replica", "sync", "--from-device", sourceTape, "--to-device", targetTape, "-y", "--memory", "8388608", "--json")
	must("replica", "sync", "--from-device", targetTape, "--to-image", capture, "-y", "--memory", "8388608", "--json")
	// A second capture is a no-op and needs no wear confirmation even on pipes.
	must("replica", "sync", "--from-device", targetTape, "--to-image", capture, "--memory", "8388608", "--json")
	if _, e := call("replica", "sync", "--from-image", ssd, "--to-image", ssd, "-n"); e == nil {
		t.Fatal("same endpoint accepted")
	}
	link := filepath.Join(dir, "same.img")
	if e := os.Link(ssd, link); e != nil {
		t.Fatal(e)
	}
	if _, e := call("replica", "sync", "--from-image", ssd, "--to-image", link); e == nil {
		t.Fatal("hardlink endpoint accepted")
	}
	if _, e := call("replica", "sync", "--from-image", ssd, "--to-image", hdd, "--prune"); e == nil {
		t.Fatal("image-only prune accepted")
	}
	t.Run("kernel mount", func(t *testing.T) {
		checkImageMount(t, hdd, archives, files)
	})
}
