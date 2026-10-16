package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
	"github.com/thezzisu/taper/internal/policy"
)

// This fixture uses the published v1 bytes and pre-refactor catalog layout.
// Conversion maps identities in SQLite only; no tape command participates.
func TestPhysicalV1FixtureAfterSQLMigration(t *testing.T) {
	dir := t.TempDir()
	db, e := driver.Open("file:" + filepath.Join(dir, "old.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	ddl, e := os.ReadFile("../catalog/testdata/v1.sql")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(string(ddl)); e != nil {
		t.Fatal(e)
	}
	tapePath := filepath.Join(dir, "original.tape")
	media.CreateSim(tapePath, 8<<30, false)
	d, e := media.OpenSim(tapePath, true)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	info, _ := d.Info()
	oldMedia := format.NewID().String()
	label := Label{ID: oldMedia, Name: "123456L6", Serial: info.Serial, Version: 1}
	id, _ := format.ParseID(oldMedia)
	if e = control(d, format.Media, id, label); e != nil {
		t.Fatal(e)
	}
	d.Filemark()
	identity, _ := json.Marshal([]string{oldMedia, label.Name, ""})
	d.WriteIdentity(append([]byte("taper1:"), identity...))
	s := &catalog.Store{DB: db, Path: filepath.Join(dir, "old.db")}
	_, e = db.Exec("INSERT INTO media(id,label,serial,total,remaining,sampled,state,key_id,key,hardware_compression) VALUES(?,?,?,?,?,?,'open','',NULL,0)", oldMedia, label.Name, info.Serial, info.Total, info.Remaining, info.Sampled)
	if e != nil {
		t.Fatal(e)
	}
	root := filepath.Join(dir, "source")
	os.Mkdir(root, 0700)
	os.WriteFile(filepath.Join(root, "data"), bytes.Repeat([]byte("native v1\n"), 1000), 0600)
	for seq := 0; seq < 2; seq++ {
		if seq == 1 {
			d.Filemark()
		}
		m, e := scanSource(context.Background(), root, "fixture", policy.Rules{})
		if e != nil {
			t.Fatal(e)
		}
		m.ReplicaID = oldMedia
		m.StreamID = format.NewID().String()
		m.Seq = seq
		m.BeginPos, _ = d.Position()
		m.DataPos = m.BeginPos + 1
		m.Blocks = uint64(1 + (m.Logical-1)/format.PayloadSize)
		m.CommitPos = m.DataPos + m.Blocks
		m.EndPos = m.CommitPos + 2
		p, e := supply(context.Background(), m, WriteOptions{Memory: 8 << 20})
		if e != nil {
			t.Fatal(e)
		}
		n, e := packWrite(p, d, m, WriteOptions{Memory: 8 << 20})
		p.Close()
		if e != nil {
			t.Fatal(e)
		}
		if _, e = db.Exec("INSERT INTO batches VALUES(?,?,?,?,?,?,?,?,1,'committed')", m.ID.String(), m.Name, m.Source, m.Created, "{}", m.Digest, m.Logical, m.Files); e != nil {
			t.Fatal(e)
		}
		if _, e = db.Exec("INSERT INTO batch_sources VALUES(?,?,?,?)", m.ID.String(), m.SourceIdentity.Device, m.SourceIdentity.Inode, m.SourceIdentity.FSID); e != nil {
			t.Fatal(e)
		}
		for i, c := range m.Contents {
			db.Exec("INSERT INTO contents VALUES(?,?,?,?,?,?)", m.ID.String(), i, c.Size, c.Hash, c.Meta, c.Data)
			for _, f := range c.Files {
				db.Exec("INSERT INTO files VALUES(?,?,?,?,?)", m.ID.String(), f.Path, i, f.Source, f.MTime)
			}
		}
		if _, e = db.Exec("INSERT INTO replicas(id,batch_id,media_id,begin_pos,data_pos,commit_pos,blocks,confirmed_bytes,confirmed_records,end_pos,state) VALUES(?,?,?,?,?,?,?,?,?,?,'committed')", m.StreamID, m.ID.String(), oldMedia, m.BeginPos, m.DataPos, m.CommitPos, m.Blocks, n, m.Blocks+2, m.EndPos); e != nil {
			t.Fatal(e)
		}
	}
	info, _ = d.Info()
	if e = recordMediaInfo(s, oldMedia, info); e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadFile(tapePath)
	if e != nil {
		t.Fatal(e)
	}
	script, e := os.ReadFile("../../scripts/migrate_v1.sql")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(string(script)); e != nil {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(tapePath)
	if !bytes.Equal(before, after) {
		t.Fatal("migration changed original media")
	}
	plan, e := PlanVerification(s, d, "")
	if e != nil {
		t.Fatal(e)
	}
	result, e := VerifyTape(context.Background(), s, d, plan, nil, ReadOptions{Memory: 8 << 20})
	if e != nil || result.Archives != 2 {
		t.Fatal("native v1 records failed verification", result, e)
	}
	image, e := media.OpenImage(filepath.Join(dir, "native.img"), true, oldMedia, "")
	if e != nil {
		t.Fatal(e)
	}
	defer image.Close()
	p, e := PlanSync(s, oldMedia, d, image, "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = ReplicaSync(context.Background(), s, d, image, p, nil, false, ReadOptions{Memory: 8 << 20}); e != nil {
		t.Fatal(e)
	}
	set, e := ResolveReplicaSet(s, oldMedia)
	if e != nil {
		t.Fatal(e)
	}
	other := secondMember(t, s, set)
	p, e = PlanSync(s, oldMedia, image, other, "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = ReplicaSync(context.Background(), s, image, other, p, nil, false, ReadOptions{Memory: 8 << 20}); e != nil {
		t.Fatal(e)
	}
	d.Locate(2)
	other.Locate(2)
	for pos := uint64(2); pos < *info.Position; pos++ {
		a, ea := d.ReadRecord()
		b, eb := other.ReadRecord()
		if ea != eb || !bytes.Equal(a, b) {
			t.Fatal("copied native bytes differ", pos)
		}
	}
}
