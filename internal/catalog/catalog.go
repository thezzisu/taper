package catalog

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/vfs/adiantum"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

type Store struct {
	DB   *sql.DB
	Path string
	key  []byte
}
type unlock struct {
	Version int    `json:"version"`
	Salt    []byte `json:"salt"`
	Nonce   []byte `json:"nonce"`
	Wrapped []byte `json:"wrapped"`
}

func wrap(pass, key []byte) ([]byte, error) {
	u := unlock{Version: 1, Salt: make([]byte, 16), Nonce: make([]byte, 24)}
	rand.Read(u.Salt)
	rand.Read(u.Nonce)
	k := argon2.IDKey(pass, u.Salt, 3, 64*1024, 4, 32)
	defer clear(k)
	a, _ := chacha20poly1305.NewX(k)
	u.Wrapped = a.Seal(nil, u.Nonce, key, []byte("taper-catalog-v1"))
	return json.Marshal(u)
}
func unwrap(pass, b []byte) ([]byte, error) {
	var u unlock
	if e := json.Unmarshal(b, &u); e != nil {
		return nil, e
	}
	if u.Version != 1 || len(u.Salt) != 16 || len(u.Nonce) != 24 || len(u.Wrapped) != 48 {
		return nil, errors.New("invalid unlock descriptor")
	}
	k := argon2.IDKey(pass, u.Salt, 3, 64*1024, 4, 32)
	defer clear(k)
	a, _ := chacha20poly1305.NewX(k)
	key, e := a.Open(nil, u.Nonce, u.Wrapped, []byte("taper-catalog-v1"))
	if e != nil {
		return nil, errors.New("wrong passphrase or damaged unlock descriptor")
	}
	return key, nil
}
func Create(name string, pass []byte) (*Store, error) {
	if len(pass) == 0 {
		return nil, errors.New("empty passphrase")
	}
	if _, e := os.Stat(name); !os.IsNotExist(e) {
		return nil, errors.New("catalog already exists")
	}
	if e := os.MkdirAll(filepath.Dir(name), 0700); e != nil {
		return nil, e
	}
	key := make([]byte, 32)
	if _, e := rand.Read(key); e != nil {
		return nil, e
	}
	b, e := wrap(pass, key)
	if e != nil {
		return nil, e
	}
	f, e := os.OpenFile(name+".unlock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return nil, e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	f.Close()
	if e != nil {
		return nil, e
	}
	return connect(name, key, true)
}
func Open(name string, pass []byte) (*Store, error) {
	b, e := os.ReadFile(name + ".unlock")
	if e != nil {
		return nil, e
	}
	key, e := unwrap(pass, b)
	if e != nil {
		return nil, e
	}
	if _, e = os.Stat(name); e != nil {
		clear(key)
		return nil, e
	}
	if mac, e := os.ReadFile(name + ".mac"); e == nil {
		db, e := os.ReadFile(name)
		if e != nil {
			clear(key)
			return nil, e
		}
		h := hmac.New(sha256.New, key)
		h.Write([]byte("taper-export-v1"))
		h.Write(b)
		h.Write(db)
		if !hmac.Equal(h.Sum(nil), mac) {
			clear(key)
			return nil, errors.New("export authentication failed")
		}
	}
	return connect(name, key, false)
}
func openDatabase(name string, key []byte, mode string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(name), RawQuery: "vfs=adiantum&mode=" + mode}
	db, e := driver.Open(u.String(), func(c *sqlite3.Conn) error { return c.Exec("PRAGMA hexkey='" + hex.EncodeToString(key) + "'") })
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
func checkVersion(db *sql.DB) error {
	var count, low, high int
	if e := db.QueryRow("SELECT COUNT(*),COALESCE(MIN(version),0),COALESCE(MAX(version),0) FROM schema_version").Scan(&count, &low, &high); e != nil {
		return e
	}
	if count != 1 || low != 1 || high != 1 {
		return errors.New("unsupported catalog schema version; expected 1")
	}
	rows, e := db.Query("SELECT c.ordinal,e.entry,e.path,e.kind,e.size,e.format,e.state,e.message,e.encrypted,e.password,e.volumes,r.confirmed_bytes,r.confirmed_records,r.end_pos,m.sampled,m.info FROM archive_scans c,archive_entries e,replicas r,media_samples m LIMIT 0")
	if e != nil {
		return errors.New("unsupported catalog layout; explicitly convert a backed-up catalog before opening it with this build")
	}
	rows.Close()
	var invalid int
	if e = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('media') WHERE name='hardware_compression' AND ("notnull"!=0 OR dflt_value IS NOT NULL)`).Scan(&invalid); e != nil || invalid != 0 {
		return errors.New("unsupported catalog layout; hardware compression must preserve unknown values")
	}
	if e = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('replicas') WHERE name IN ('data_pos','blocks','confirmed_bytes','confirmed_records','end_pos') AND ("notnull"!=0 OR dflt_value IS NOT NULL)`).Scan(&invalid); e != nil || invalid != 0 {
		return errors.New("unsupported catalog layout; replica observations must preserve unknown values")
	}
	return nil
}
func connect(name string, key []byte, create bool) (*Store, error) {
	mode := "rw"
	if create {
		mode = "rwc"
	}
	db, e := openDatabase(name, key, mode)
	if e != nil {
		return nil, e
	}
	s := &Store{DB: db, Path: name, key: key}
	fail := func(e error) (*Store, error) { s.Close(); return nil, e }
	if !create {
		if e = checkVersion(db); e != nil {
			return fail(e)
		}
	}
	if _, e = db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON; PRAGMA temp_store=MEMORY;"); e != nil {
		return fail(e)
	}
	if create {
		tx, e := db.Begin()
		if e != nil {
			return fail(e)
		}
		if _, e = tx.Exec(schema); e != nil {
			tx.Rollback()
			return fail(e)
		}
		if e = tx.Commit(); e != nil {
			return fail(e)
		}
	}
	os.Chmod(name, 0600)
	return s, nil
}
func (s *Store) Close() error { clear(s.key); return s.DB.Close() }
func (s *Store) Export(dst string) error {
	if _, e := os.Stat(dst); !os.IsNotExist(e) {
		return errors.New("export destination exists")
	}
	if e := os.Mkdir(dst, 0700); e != nil {
		return e
	}
	target := filepath.Join(dst, "catalog.db")
	uri := url.URL{Scheme: "file", Path: target}
	params := url.Values{"vfs": []string{"adiantum"}, "hexkey": []string{hex.EncodeToString(s.key)}}
	uri.RawQuery = params.Encode()
	conn, e := s.DB.Conn(context.Background())
	if e != nil {
		return e
	}
	e = conn.Raw(func(raw any) error { return raw.(driver.Conn).Raw().Backup("main", uri.String()) })
	conn.Close()
	if e != nil {
		return errors.New("encrypted snapshot backup failed")
	}
	b, e := os.ReadFile(target)
	if e != nil {
		return e
	}

	u, e := os.ReadFile(s.Path + ".unlock")
	if e != nil {
		return e
	}
	if e = os.WriteFile(filepath.Join(dst, "catalog.db.unlock"), u, 0600); e != nil {
		return e
	}
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte("taper-export-v1"))
	h.Write(u)
	h.Write(b)
	if e = os.WriteFile(filepath.Join(dst, "catalog.db.mac"), h.Sum(nil), 0600); e != nil {
		return e
	}
	for _, name := range []string{"catalog.db", "catalog.db.unlock", "catalog.db.mac"} {
		file, e := os.OpenFile(filepath.Join(dst, name), os.O_RDWR, 0600)
		if e != nil {
			return e
		}
		os.Chmod(filepath.Join(dst, name), 0600)
		e = file.Sync()
		file.Close()
		if e != nil {
			return e
		}
	}
	dir, e := os.Open(dst)
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *Store) Rekey(pass []byte) error {
	if len(pass) == 0 {
		return errors.New("empty passphrase")
	}
	b, e := wrap(pass, s.key)
	if e != nil {
		return e
	}
	tmp := s.Path + ".unlock.new"
	if e = os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	f, e := os.OpenFile(tmp, os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	e = f.Sync()
	f.Close()
	if e != nil {
		return e
	}
	if e = os.Rename(tmp, s.Path+".unlock"); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(s.Path))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}

const schema = `
CREATE TABLE schema_version(version INTEGER NOT NULL);
INSERT INTO schema_version VALUES(1);
CREATE TABLE media(id TEXT PRIMARY KEY,label TEXT NOT NULL,serial TEXT NOT NULL UNIQUE,total INTEGER NOT NULL,remaining INTEGER NOT NULL,sampled TEXT NOT NULL,state TEXT NOT NULL,key_id TEXT NOT NULL,key BLOB,hardware_compression INTEGER CHECK(hardware_compression IN (0,1)));
CREATE TABLE batches(id TEXT PRIMARY KEY,name TEXT NOT NULL,source TEXT NOT NULL,created TEXT NOT NULL,rules TEXT NOT NULL,digest TEXT NOT NULL,logical_bytes INTEGER NOT NULL,files INTEGER NOT NULL,required_replicas INTEGER NOT NULL DEFAULT 1,state TEXT NOT NULL);
CREATE TABLE contents(batch_id TEXT NOT NULL REFERENCES batches(id),ordinal INTEGER NOT NULL,size INTEGER NOT NULL,hash TEXT NOT NULL,meta_offset INTEGER NOT NULL,data_offset INTEGER NOT NULL,PRIMARY KEY(batch_id,ordinal));
CREATE INDEX content_hash ON contents(hash,size);
CREATE TABLE archive_scans(batch_id TEXT NOT NULL,ordinal INTEGER NOT NULL,PRIMARY KEY(batch_id,ordinal),FOREIGN KEY(batch_id,ordinal) REFERENCES contents(batch_id,ordinal) ON DELETE CASCADE);
CREATE TABLE archive_entries(batch_id TEXT NOT NULL,ordinal INTEGER NOT NULL,entry TEXT NOT NULL,path TEXT NOT NULL,kind TEXT NOT NULL,size INTEGER NOT NULL,format TEXT NOT NULL,state TEXT NOT NULL,message TEXT NOT NULL,encrypted INTEGER NOT NULL,password BLOB,volumes TEXT NOT NULL,PRIMARY KEY(batch_id,ordinal,entry),FOREIGN KEY(batch_id,ordinal) REFERENCES contents(batch_id,ordinal) ON DELETE CASCADE);
CREATE TABLE files(batch_id TEXT NOT NULL,path TEXT NOT NULL,ordinal INTEGER NOT NULL,source_path TEXT NOT NULL,mtime INTEGER,PRIMARY KEY(batch_id,path),FOREIGN KEY(batch_id,ordinal) REFERENCES contents(batch_id,ordinal));
CREATE TABLE exclusions(batch_id TEXT NOT NULL REFERENCES batches(id),path TEXT NOT NULL,size INTEGER NOT NULL,reason TEXT NOT NULL);
CREATE TABLE replicas(id TEXT PRIMARY KEY,batch_id TEXT NOT NULL REFERENCES batches(id),media_id TEXT NOT NULL REFERENCES media(id),begin_pos INTEGER NOT NULL,data_pos INTEGER,commit_pos INTEGER NOT NULL DEFAULT -1,blocks INTEGER,confirmed_bytes INTEGER,confirmed_records INTEGER,end_pos INTEGER,state TEXT NOT NULL,error TEXT NOT NULL DEFAULT '',verified TEXT);
CREATE INDEX replicas_batch ON replicas(batch_id,state);
CREATE TABLE scan_cache(root TEXT NOT NULL,path TEXT NOT NULL,fsid TEXT NOT NULL,device INTEGER NOT NULL,inode INTEGER NOT NULL,size INTEGER NOT NULL,mtime INTEGER NOT NULL,ctime INTEGER NOT NULL,hash TEXT NOT NULL,PRIMARY KEY(root,path));
CREATE TABLE scan_checkpoints(root TEXT NOT NULL,path TEXT NOT NULL,fsid TEXT NOT NULL,device INTEGER NOT NULL,inode INTEGER NOT NULL,size INTEGER NOT NULL,mtime INTEGER NOT NULL,ctime INTEGER NOT NULL,offset INTEGER NOT NULL,state BLOB NOT NULL,PRIMARY KEY(root,path));
CREATE TABLE batch_sources(batch_id TEXT PRIMARY KEY REFERENCES batches(id),device INTEGER NOT NULL,inode INTEGER NOT NULL,fsid TEXT NOT NULL);
CREATE TABLE media_health(media_id TEXT PRIMARY KEY REFERENCES media(id),unhealthy INTEGER NOT NULL,info TEXT NOT NULL,sampled TEXT NOT NULL);
CREATE TABLE media_samples(id INTEGER PRIMARY KEY,media_id TEXT NOT NULL REFERENCES media(id),sampled TEXT NOT NULL,info TEXT NOT NULL);
CREATE TABLE stage(id INTEGER PRIMARY KEY CHECK(id=1),source TEXT NOT NULL,identity TEXT NOT NULL,rules TEXT NOT NULL,plan_id TEXT REFERENCES batches(id));
CREATE TABLE stage_files(path TEXT PRIMARY KEY,source_path TEXT NOT NULL,size INTEGER NOT NULL,mtime INTEGER NOT NULL);
CREATE TABLE stage_exclusions(path TEXT PRIMARY KEY,size INTEGER NOT NULL,reason TEXT NOT NULL);
`
