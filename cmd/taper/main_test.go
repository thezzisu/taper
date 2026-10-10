package main

import (
	"context"
	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/media"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIOfflineWorkflow(t *testing.T) {
	original := openTape
	openTape = func(path string, write bool) (media.Device, error) { return media.OpenSim(path, write) }
	defer func() { openTape = original }()
	dir := t.TempDir()
	pass := filepath.Join(dir, "session.pass")
	os.WriteFile(pass, []byte("synthetic CLI password"), 0600)
	db := filepath.Join(dir, "catalog.db")
	base := []string{"--catalog", db, "--pass-file", pass}
	call := func(cmd string, args ...string) {
		t.Helper()
		a := append(strings.Fields(cmd), base...)
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
	call("media init", "--device", sim, "--name", "TEST")
	root := filepath.Join(dir, "source")
	os.Mkdir(root, 0700)
	os.WriteFile(filepath.Join(root, "hello"), []byte("hello CLI"), 0600)
	call("stage add", "--source", root)
	call("stage ls")
	call("stage plan", "--frozen")
	s, e := catalog.Open(db, []byte("synthetic CLI password"))
	if e != nil {
		t.Fatal(e)
	}
	var id string
	s.DB.QueryRow("SELECT id FROM batches").Scan(&id)
	s.Close()
	call("stage plan", "--frozen")
	call("stage write", "--device", sim, "--frozen", "--memory", "8388608")
	call("archive list")
	call("browse", "--archive", id)
	call("verify", "--device", sim, "--memory", "8388608")
	call("reindex", "--device", sim, "--memory", "8388608")
	call("catalog export", "--dest", filepath.Join(dir, "export"))
	call("key generate", "--key-file", filepath.Join(dir, "key"), "--keycard", filepath.Join(dir, "keycard"), "--name", "TEST2")
}
