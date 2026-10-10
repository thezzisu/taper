package main

import (
	"context"
	"github.com/thezzisu/taper/internal/catalog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIOfflineWorkflow(t *testing.T) {
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
	call("sim create", "--sim", sim, "--capacity", "10737418240")
	call("media init", "--sim", sim, "--name", "TEST")
	root := filepath.Join(dir, "source")
	os.Mkdir(root, 0700)
	os.WriteFile(filepath.Join(root, "hello"), []byte("hello CLI"), 0600)
	call("batch scan", "--source", root)
	s, e := catalog.Open(db, []byte("synthetic CLI password"))
	if e != nil {
		t.Fatal(e)
	}
	var id string
	s.DB.QueryRow("SELECT id FROM batches").Scan(&id)
	s.Close()
	call("batch plan", "--batch", id)
	call("backup", "--sim", sim, "--batch", id, "--frozen", "--memory", "8388608")
	call("browse", "--batch", id)
	call("restore", "--plan", "--batch", id)
	call("verify", "--sim", sim, "--batch", id)
	dest := filepath.Join(dir, "restored")
	call("restore", "--sim", sim, "--batch", id, "--dest", dest)
	b, e := os.ReadFile(filepath.Join(dest, "hello"))
	if e != nil || string(b) != "hello CLI" {
		t.Fatal(e)
	}
	call("catalog export", "--dest", filepath.Join(dir, "export"))
	call("key generate", "--key-file", filepath.Join(dir, "key"), "--keycard", filepath.Join(dir, "keycard"), "--name", "TEST2")
}
