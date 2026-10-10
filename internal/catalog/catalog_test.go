package catalog

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestVersionOneMigrationPreservesCatalog(t *testing.T) {
	name := filepath.Join(t.TempDir(), "catalog.db")
	pass := []byte("migration test")
	s, e := Create(name, pass)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec("INSERT INTO media VALUES('id','label','serial',100,90,'time','open','key-id',?)", []byte{1, 2, 3}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec("DROP TABLE stage_files; DROP TABLE stage_exclusions; DROP TABLE stage; UPDATE schema_version SET version=1"); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(name, pass)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	var version, count int
	var key []byte
	if e = s.DB.QueryRow("SELECT version FROM schema_version").Scan(&version); e != nil || version != 2 {
		t.Fatal(version, e)
	}
	if e = s.DB.QueryRow("SELECT key FROM media WHERE serial='serial'").Scan(&key); e != nil || !bytes.Equal(key, []byte{1, 2, 3}) {
		t.Fatal("media/key lost in migration", e)
	}
	if e = s.DB.QueryRow("SELECT COUNT(*) FROM stage_files").Scan(&count); e != nil || count != 0 {
		t.Fatal(count, e)
	}
	if e = s.Integrity(); e != nil {
		t.Fatal(e)
	}
}

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
