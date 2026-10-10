package catalog

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptionRekeyExport(t *testing.T) {
	name := filepath.Join(t.TempDir(), "catalog.db")
	s, e := Create(name, []byte("correct"))
	if e != nil {
		t.Fatal(e)
	}
	secret := "PRIVATE_PATH_SHOULD_NOT_APPEAR"
	if _, e = s.DB.Exec("INSERT INTO media VALUES(?,?,?,?,?,?,?,?,?)", secret, secret, "serial", 999, 999, "today", "open", "", nil); e != nil {
		t.Fatal(e)
	}
	if e = s.Integrity(); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{name, name + "-wal"} {
		b, e := os.ReadFile(p)
		if e == nil && (bytes.Contains(b, []byte(secret)) || bytes.HasPrefix(b, []byte("SQLite format"))) {
			t.Fatal("plaintext leaked", p)
		}
	}
	if wrong, e := Open(name, []byte("wrong")); e == nil {
		wrong.Close()
		t.Fatal("wrong passphrase accepted")
	}
	if e = s.Rekey([]byte("next")); e != nil {
		t.Fatal(e)
	}
	export := filepath.Join(t.TempDir(), "export")
	if e = s.Export(export); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(filepath.Join(export, "catalog.db"), []byte("next"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.Integrity(); e != nil {
		t.Fatal(e)
	}
	imported := filepath.Join(t.TempDir(), "restored.db")
	if e = Import(export, imported, []byte("next")); e != nil {
		t.Fatal(e)
	}
	copy, e := Open(imported, []byte("next"))
	if e != nil {
		t.Fatal(e)
	}
	if e = copy.Integrity(); e != nil {
		t.Fatal(e)
	}
	copy.Close()
}
