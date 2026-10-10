package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
	"github.com/thezzisu/taper/internal/policy"
)

func setup(t *testing.T) (*catalog.Store, *media.Sim, string) {
	t.Helper()
	dir := t.TempDir()
	s, e := catalog.Create(filepath.Join(dir, "catalog.db"), []byte("test passphrase"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	name := filepath.Join(dir, "sim.tape")
	if e = media.CreateSim(name, 8<<30, true); e != nil {
		t.Fatal(e)
	}
	d, e := media.OpenSim(name, true)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	root := filepath.Join(dir, "source")
	os.Mkdir(root, 0700)
	return s, d, root
}
func makeSource(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{"tree/large.bin": bytes.Repeat([]byte("0123456789abcdef"), 180000), "duplicate.bin": bytes.Repeat([]byte("0123456789abcdef"), 180000), "tree/small.txt": []byte("hello"), "empty": {}, "back\\slash": []byte("slash"), "percent%name": []byte("percent")}
	for p, b := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0700)
		if e := os.WriteFile(filepath.Join(root, p), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	return files
}
func TestRoundTripAndReindex(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(map[bool]string{false: "hardware", true: "zstd"}[compressed], func(t *testing.T) {
			s, d, root := setup(t)
			expected := makeSource(t, root)
			key := bytes.Repeat([]byte{7}, 32)
			label, e := LabelMedia(s, d, "test", key, true)
			if e != nil {
				t.Fatal(e)
			}
			m, e := Scan(context.Background(), root, "first", policy.Rules{})
			if e != nil {
				t.Fatal(e)
			}
			if len(m.Contents) != len(expected)-1 {
				t.Fatal("dedupe failed")
			}
			if e = Save(s, m); e != nil {
				t.Fatal(e)
			}
			rep, e := Backup(context.Background(), s, d, m, nil, WriteOptions{Memory: 8 << 20, Zstd: compressed, AllowFrozen: true})
			if e != nil {
				t.Fatal(e)
			}
			rs, e := Replicas(s, m.ID.String())
			if e != nil || len(rs) != 1 || rs[0].ID != rep || rs[0].Verified {
				t.Fatal("replica state", rs, e)
			}
			dest := filepath.Join(t.TempDir(), "restore")
			reads, locates, e := Restore(context.Background(), s, d, m, rs[0], dest, nil, nil, false)
			if e != nil {
				t.Fatal(e)
			}
			if locates != 1 || reads != rs[0].Blocks {
				t.Fatalf("inefficient restore reads=%d blocks=%d seeks=%d", reads, rs[0].Blocks, locates)
			}
			for p, b := range expected {
				got, e := os.ReadFile(filepath.Join(dest, format.Normalize(p)))
				if e != nil || !bytes.Equal(got, b) {
					t.Fatalf("%s differs: %v", p, e)
				}
			}
			if _, _, e = Restore(context.Background(), s, d, m, rs[0], dest, nil, nil, false); e == nil {
				t.Fatal("overwrote existing targets")
			}
			if _, _, e = Restore(context.Background(), s, d, m, rs[0], "", nil, nil, true); e != nil {
				t.Fatal(e)
			}
			fresh, e := catalog.Create(filepath.Join(t.TempDir(), "fresh.db"), []byte("different password"))
			if e != nil {
				t.Fatal(e)
			}
			defer fresh.Close()
			n, e := Reindex(context.Background(), fresh, d, key)
			if e != nil || n != 1 {
				t.Fatal("reindex", n, e)
			}
			recovered, e := Load(fresh, m.ID.String())
			if e != nil || recovered.Digest != m.Digest {
				t.Fatal("recovered manifest", e)
			}
			copies, e := Replicas(fresh, m.ID.String())
			if e != nil || copies[0].Media != label.ID {
				t.Fatal(e)
			}
			if _, _, e = Restore(context.Background(), fresh, d, recovered, copies[0], filepath.Join(t.TempDir(), "recovered"), nil, nil, false); e != nil {
				t.Fatal(e)
			}
			// Same logical batch, independent second-tape replica, no duplicate file tree.
			otherName := filepath.Join(t.TempDir(), "other.tape")
			media.CreateSim(otherName, 8<<30, false)
			other, e := media.OpenSim(otherName, true)
			if e != nil {
				t.Fatal(e)
			}
			defer other.Close()
			LabelMedia(s, other, "second", nil, false)
			if _, e = Backup(context.Background(), s, other, m, nil, WriteOptions{Memory: 8 << 20, AllowFrozen: true}); e != nil {
				t.Fatal(e)
			}
			rs, _ = Replicas(s, m.ID.String())
			if len(rs) != 2 {
				t.Fatal("replica extension failed")
			}
		})
	}
}

type failDevice struct {
	media.Device
	writes int
	failAt int
	flush  bool
}

func (d *failDevice) WriteRecord(b []byte) error {
	d.writes++
	if d.writes == d.failAt {
		return errors.New("injected write ambiguity")
	}
	return d.Device.WriteRecord(b)
}
func (d *failDevice) Filemark() error {
	if d.flush {
		return errors.New("injected flush failure")
	}
	return d.Device.Filemark()
}
func TestFailuresPreserveOldReplica(t *testing.T) {
	s, d, root := setup(t)
	makeSource(t, root)
	LabelMedia(s, d, "test", nil, false)
	m, e := Scan(context.Background(), root, "batch", policy.Rules{})
	if e != nil {
		t.Fatal(e)
	}
	Save(s, m)
	if _, e = Backup(context.Background(), s, d, m, nil, WriteOptions{Memory: 8 << 20, AllowFrozen: true}); e != nil {
		t.Fatal(e)
	}
	for _, fault := range []*failDevice{{Device: d, failAt: 2}, {Device: d, flush: true}} {
		if _, e = Backup(context.Background(), s, fault, m, nil, WriteOptions{Memory: 8 << 20, AllowFrozen: true}); e == nil {
			t.Fatal("fault ignored")
		}
		rs, _ := Replicas(s, m.ID.String())
		if len(rs) != 1 {
			t.Fatal("incomplete replica committed")
		}
		if _, _, e = Restore(context.Background(), s, d, m, rs[0], "", nil, nil, true); e != nil {
			t.Fatal("old replica damaged", e)
		}
	}
}
func TestSourceChangeAndPolicies(t *testing.T) {
	s, d, root := setup(t)
	makeSource(t, root)
	LabelMedia(s, d, "test", nil, false)
	os.Mkdir(filepath.Join(root, "Tmp"), 0700)
	os.WriteFile(filepath.Join(root, "Tmp", "garbage"), []byte("junk"), 0600)
	m, e := Scan(context.Background(), root, "batch", policy.Rules{Exclude: []string{"Tmp/**"}})
	if e != nil {
		t.Fatal(e)
	}
	if len(m.Excluded) != 1 {
		t.Fatal("rules")
	}
	Save(s, m)
	if _, e = Backup(context.Background(), s, d, m, nil, WriteOptions{Memory: 8 << 20}); e == nil {
		t.Fatal("writable source accepted")
	}
	os.WriteFile(filepath.Join(root, "tree", "small.txt"), []byte("HELLO"), 0600)
	if _, e = Backup(context.Background(), s, d, m, nil, WriteOptions{Memory: 8 << 20, AllowFrozen: true}); e == nil || !strings.Contains(e.Error(), "source changed") {
		t.Fatal("change ignored", e)
	}
	rs, _ := Replicas(s, m.ID.String())
	if len(rs) != 0 {
		t.Fatal("changed-source replica committed")
	}
}

func TestReindexSkipsUncommittedTailAndImportsRetry(t *testing.T) {
	s, d, root := setup(t)
	makeSource(t, root)
	LabelMedia(s, d, "test", nil, false)
	m, e := Scan(context.Background(), root, "test", policy.Rules{})
	if e != nil {
		t.Fatal(e)
	}
	Save(s, m)
	if _, e = Backup(context.Background(), s, &failDevice{Device: d, failAt: 2}, m, nil, WriteOptions{Memory: 8 << 20, AllowFrozen: true}); e == nil {
		t.Fatal("failure ignored")
	}
	if _, e = Backup(context.Background(), s, d, m, nil, WriteOptions{Memory: 8 << 20, AllowFrozen: true}); e != nil {
		t.Fatal(e)
	}
	fresh, e := catalog.Create(filepath.Join(t.TempDir(), "fresh.db"), []byte("password"))
	if e != nil {
		t.Fatal(e)
	}
	defer fresh.Close()
	n, e := Reindex(context.Background(), fresh, d, nil)
	if n != 1 || e == nil {
		t.Fatalf("complete retry not imported, or incomplete tail unreported: %d %v", n, e)
	}
}

func BenchmarkBackupPacked(b *testing.B) {
	dir := b.TempDir()
	root := filepath.Join(dir, "source")
	os.Mkdir(root, 0700)
	os.WriteFile(filepath.Join(root, "data"), bytes.Repeat([]byte("a"), 8<<20), 0600)
	s, e := catalog.Create(filepath.Join(dir, "catalog.db"), []byte("benchmark"))
	if e != nil {
		b.Fatal(e)
	}
	defer s.Close()
	m, e := Scan(context.Background(), root, "bench", policy.Rules{})
	if e != nil {
		b.Fatal(e)
	}
	Save(s, m)
	name := filepath.Join(dir, "sim.tape")
	media.CreateSim(name, 1<<40, false)
	d, e := media.OpenSim(name, true)
	if e != nil {
		b.Fatal(e)
	}
	defer d.Close()
	LabelMedia(s, d, "bench", nil, false)
	b.SetBytes(8 << 20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, e = Backup(context.Background(), s, d, m, nil, WriteOptions{Memory: 16 << 20, AllowFrozen: true}); e != nil {
			b.Fatal(e)
		}
	}
}
