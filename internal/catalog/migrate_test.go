package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ncruces/go-sqlite3/driver"
)

func oldFixture(t *testing.T, compression any) (*Store, []byte) {
	t.Helper()
	db, e := driver.Open("file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "old.db")) + "?mode=rwc")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	schema, e := os.ReadFile("testdata/v1.sql")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(string(schema)); e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec("INSERT INTO media VALUES('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','CARTRIDGE','SERIAL',100,90,'now','open','',NULL,?)", compression)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec(`INSERT INTO batches VALUES('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb','old','/source','now','{}','digest',1,1,1,'committed');
 INSERT INTO replicas VALUES('cccccccccccccccccccccccccccccccc','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',2,3,4,1,100,3,6,'committed','',NULL);
 INSERT INTO contents VALUES('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',0,1,'hash',0,0);
 INSERT INTO files VALUES('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb','data',0,'data',NULL);
 INSERT INTO media_health VALUES('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',0,'{}','now');
 INSERT INTO media_samples VALUES(1,'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','now','{}');`)
	if e != nil {
		t.Fatal(e)
	}
	script, e := os.ReadFile(filepath.Join("..", "..", "scripts", "migrate_v1.sql"))
	if e != nil {
		t.Fatal(e)
	}
	return &Store{DB: db}, script
}
func TestMigrateV1PreservesNativeIdentityAndForeignKeys(t *testing.T) {
	s, script := oldFixture(t, 0)
	if _, e := s.DB.Exec(string(script)); e != nil {
		t.Fatal(e)
	}
	if e := checkVersion(s.DB); e != nil {
		t.Fatal(e)
	}
	var set, stream, member string
	var seq int
	var begin, end uint64
	if e := s.DB.QueryRow("SELECT replica_id,stream_id,seq,begin_pos,end_pos FROM batches").Scan(&set, &stream, &seq, &begin, &end); e != nil {
		t.Fatal(e)
	}
	if set != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || stream != "cccccccccccccccccccccccccccccccc" || seq != 0 || begin != 2 || end != 6 {
		t.Fatal("on-tape addresses or IDs changed")
	}
	var label, serial string
	var compression bool
	if e := s.DB.QueryRow("SELECT replica_id,label,serial,hardware_compression FROM media").Scan(&member, &label, &serial, &compression); e != nil || member != set || label != "CARTRIDGE" || serial != "SERIAL" || compression {
		t.Fatal("physical cartridge facts changed", e)
	}
	rows, e := s.DB.Query("PRAGMA foreign_key_check")
	if e != nil {
		t.Fatal(e)
	}
	if rows.Next() {
		t.Fatal("migration broke child foreign keys")
	}
	rows.Close()
	if e = checkIntegrity(s); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec("DELETE FROM batches"); e == nil {
		t.Fatal("foreign keys not enforced after conversion")
	}
}
func TestMigrateV1RejectsUnknownSettingsAndAmbiguousStreams(t *testing.T) {
	for _, kind := range []string{"compression", "multiple", "unassigned"} {
		t.Run(kind, func(t *testing.T) {
			var compression any = 0
			if kind == "compression" {
				compression = nil
			}
			s, script := oldFixture(t, compression)
			if kind == "multiple" {
				s.DB.Exec("INSERT INTO replicas SELECT 'dddddddddddddddddddddddddddddddd',batch_id,media_id,begin_pos,data_pos,commit_pos,blocks,confirmed_bytes,confirmed_records,end_pos,state,error,verified FROM replicas")
			}
			if kind == "unassigned" {
				s.DB.Exec("UPDATE batches SET state='planned'")
			}
			if _, e := s.DB.Exec(string(script)); e == nil {
				t.Fatal("ambiguous conversion succeeded")
			}
			s.DB.Exec("ROLLBACK")
			var n int
			if e := s.DB.QueryRow("SELECT COUNT(*) FROM media").Scan(&n); e != nil || n != 1 {
				t.Fatal("rollback lost old data", e)
			}
		})
	}
}
