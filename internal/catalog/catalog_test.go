package catalog

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func checkIntegrity(s *Store) error {
	var result string
	if e := s.DB.QueryRow("PRAGMA integrity_check").Scan(&result); e != nil {
		return e
	}
	if result != "ok" {
		return fmt.Errorf("catalog integrity: %s", result)
	}
	return nil
}

func TestOpenRejectsUnsupportedSchemaWithoutChangingIt(t *testing.T) {
	name := filepath.Join(t.TempDir(), "catalog.db")
	pass := []byte("schema test")
	s, e := Create(name, pass)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec("UPDATE schema_version SET version=999"); e != nil {
		t.Fatal(e)
	}
	export := filepath.Join(t.TempDir(), "export")
	if e = s.Export(export); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadFile(name)
	if e != nil {
		t.Fatal(e)
	}
	if s, e = Open(name, pass); e == nil {
		s.Close()
		t.Fatal("unsupported schema accepted")
	}
	after, e := os.ReadFile(name)
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("opening an unsupported schema changed the catalog", e)
	}
	destination := filepath.Join(t.TempDir(), "imported.db")
	if e = Import(export, destination, pass); e == nil {
		t.Fatal("unsupported snapshot imported")
	}
	if _, e = os.Stat(destination); !os.IsNotExist(e) {
		t.Fatal("unsupported snapshot created a destination", e)
	}
}

func TestEncryptionRekeyExport(t *testing.T) {
	name := filepath.Join(t.TempDir(), "catalog.db")
	s, e := Create(name, []byte("correct"))
	if e != nil {
		t.Fatal(e)
	}
	secret := "PRIVATE_PATH_SHOULD_NOT_APPEAR"
	if _, e = s.DB.Exec("INSERT INTO replica_sets VALUES(?,'test','',NULL,0)", secret); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec("INSERT INTO media(id,replica_id,label,serial,total,remaining,sampled,state,key_id,key,hardware_compression) VALUES(?,?,?,?,?,?,?,?,?,?,?)", secret, secret, secret, "serial", 999, 999, "today", "open", "", nil, false); e != nil {
		t.Fatal(e)
	}
	if e = checkIntegrity(s); e != nil {
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
	if e = checkIntegrity(s); e != nil {
		t.Fatal(e)
	}
	imported := filepath.Join(t.TempDir(), "restored.db")
	if e = Import(export, imported, []byte("next")); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(imported + ".mac"); e != nil {
		t.Fatalf("authenticated MAC was not imported: %v", e)
	}
	copy, e := Open(imported, []byte("next"))
	if e != nil {
		t.Fatal(e)
	}
	if e = checkIntegrity(copy); e != nil {
		t.Fatal(e)
	}
	var compression bool
	if e = copy.DB.QueryRow("SELECT hardware_compression FROM media WHERE serial='serial'").Scan(&compression); e != nil || compression {
		t.Fatal("hardware compression preference lost in export/import", compression, e)
	}
	copy.Close()
}

func TestOpenReadOnlyDoesNotWrite(t *testing.T) {
	name := filepath.Join(t.TempDir(), "catalog.db")
	s, err := Create(name, []byte("readonly"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := OpenReadOnly(name, []byte("readonly"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err = r.DB.Exec("CREATE TABLE should_fail(x)"); err == nil {
		t.Fatal("read-only catalog accepted a write")
	}
}
