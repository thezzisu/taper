package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
			label, e := RegisterMedia(s, d, "test", key, true, true)
			if e != nil {
				t.Fatal(e)
			}
			m, e := scanSource(context.Background(), root, "first", policy.Rules{})
			if e != nil {
				t.Fatal(e)
			}
			if len(m.Contents) != len(expected)-1 {
				t.Fatal("dedupe failed")
			}
			if e = Save(s, m); e != nil {
				t.Fatal(e)
			}
			rep, e := WriteArchive(context.Background(), s, d, m, nil, WriteOptions{Memory: 8 << 20, Zstd: compressed})
			if e != nil {
				t.Fatal(e)
			}
			rs, e := Replicas(s, m.ID.String())
			if e != nil || len(rs) != 1 || rs[0].ID != rep || rs[0].Verified {
				t.Fatal("replica state", rs, e)
			}
			result, e := verifyAll(context.Background(), s, d, nil, ReadOptions{Memory: 8 << 20})
			if e != nil || !result.ReachedEOD || result.Archives != 1 {
				t.Fatal(result, e)
			}
			fresh, e := catalog.Create(filepath.Join(t.TempDir(), "fresh.db"), []byte("different password"))
			if e != nil {
				t.Fatal(e)
			}
			defer fresh.Close()
			result, e = ReindexTape(context.Background(), fresh, d, key, ReadOptions{Memory: 8 << 20})
			if e != nil || result.Imported != 1 {
				t.Fatal("reindex", result, e)
			}
			recovered, e := Load(fresh, m.ID.String())
			if e != nil || recovered.Digest != m.Digest {
				t.Fatal("recovered manifest", e)
			}
			copies, e := Replicas(fresh, m.ID.String())
			if e != nil || copies[0].Media != label.ID {
				t.Fatal(e)
			}
			for _, c := range recovered.Contents {
				for _, f := range c.Files {
					var original []byte
					found := false
					for path, data := range expected {
						if format.Normalize(path) == f.Path {
							original = data
							found = true
							break
						}
					}
					if !found {
						t.Fatal("unexpected recovered path", f.Path)
					}
					hash := sha256.Sum256(original)
					if hex.EncodeToString(hash[:]) != c.Hash || int64(len(original)) != c.Size {
						t.Fatal("recovered content identity changed", f.Path)
					}
				}
			}
			// Same logical batch, independent second-tape replica, no duplicate file tree.
			otherName := filepath.Join(t.TempDir(), "other.tape")
			media.CreateSim(otherName, 8<<30, false)
			other, e := media.OpenSim(otherName, true)
			if e != nil {
				t.Fatal(e)
			}
			defer other.Close()
			RegisterMedia(s, other, "second", nil, false, true)
			if _, e = WriteArchive(context.Background(), s, other, m, nil, WriteOptions{Memory: 8 << 20}); e != nil {
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
	for _, flush := range []bool{false, true} {
		t.Run(map[bool]string{false: "data-write", true: "filemark"}[flush], func(t *testing.T) {
			s, d, root := setup(t)
			makeSource(t, root)
			RegisterMedia(s, d, "test", nil, false, true)
			m, e := scanSource(context.Background(), root, "batch", policy.Rules{})
			if e != nil {
				t.Fatal(e)
			}
			Save(s, m)
			if _, e = WriteArchive(context.Background(), s, d, m, nil, WriteOptions{Memory: 8 << 20}); e != nil {
				t.Fatal(e)
			}
			fault := &failDevice{Device: d, failAt: 2}
			if flush {
				fault.failAt, fault.flush = 0, true
			}
			if _, e = WriteArchive(context.Background(), s, fault, m, nil, WriteOptions{Memory: 8 << 20}); e == nil {
				t.Fatal("fault ignored")
			}
			rs, _ := Replicas(s, m.ID.String())
			if len(rs) != 1 {
				t.Fatal("incomplete replica committed")
			}
			pending := m
			pending.ID = format.NewID()
			if err := Save(s, pending); err != nil {
				t.Fatal(err)
			}
			recommendations, err := Recommend(s, pending)
			if err != nil || len(recommendations) != 1 || recommendations[0].ID != "" {
				t.Fatal("damaged tail recommended for another archive", recommendations, err)
			}
			spy := &observedTape{Device: d}
			if _, err := WriteArchive(context.Background(), s, spy, pending, nil, WriteOptions{Memory: 8 << 20}); err == nil || !strings.Contains(err.Error(), "incomplete write") {
				t.Fatal("unresolved write was not blocked", err)
			}
			if spy.writes != 0 || spy.marks != 0 || spy.locates != 0 || spy.ends != 0 || spy.reads != 0 || spy.keys != 0 {
				t.Fatal("blocked retry operated on the tape", spy)
			}
			result, e := ReindexTape(context.Background(), s, d, nil, ReadOptions{Memory: 8 << 20})
			if result.Archives != 1 || !result.ReachedEOD {
				t.Fatal("old committed archive not found", result, e)
			}
		})
	}
}
func TestSourceChangeAndPolicies(t *testing.T) {
	s, d, root := setup(t)
	makeSource(t, root)
	RegisterMedia(s, d, "test", nil, false, true)
	os.Mkdir(filepath.Join(root, "Tmp"), 0700)
	os.WriteFile(filepath.Join(root, "Tmp", "garbage"), []byte("junk"), 0600)
	m, e := scanSource(context.Background(), root, "batch", policy.Rules{Exclude: []string{"Tmp/**"}})
	if e != nil {
		t.Fatal(e)
	}
	if len(m.Excluded) != 1 {
		t.Fatal("rules")
	}
	Save(s, m)
	os.WriteFile(filepath.Join(root, "tree", "small.txt"), []byte("HELLO"), 0600)
	if _, e = WriteArchive(context.Background(), s, d, m, nil, WriteOptions{Memory: 8 << 20}); e == nil || !strings.Contains(e.Error(), "source changed") {
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
	RegisterMedia(s, d, "test", nil, false, true)
	if _, e := prepareMedia(s, d, nil); e != nil {
		t.Fatal(e)
	}
	m, e := scanSource(context.Background(), root, "test", policy.Rules{})
	if e != nil {
		t.Fatal(e)
	}
	Save(s, m)
	if _, e = WriteArchive(context.Background(), s, &failDevice{Device: d, failAt: 2}, m, nil, WriteOptions{Memory: 8 << 20}); e == nil {
		t.Fatal("failure ignored")
	}
	// Build a historical interrupted-tail fixture. Resuming a real tape requires
	// explicit operator inspection; normal writes cannot skip this guard.
	if _, e = s.DB.Exec("UPDATE replicas SET state='discarded' WHERE state='failed'"); e != nil {
		t.Fatal(e)
	}
	if _, e = WriteArchive(context.Background(), s, d, m, nil, WriteOptions{Memory: 8 << 20}); e != nil {
		t.Fatal(e)
	}
	fresh, e := catalog.Create(filepath.Join(t.TempDir(), "fresh.db"), []byte("password"))
	if e != nil {
		t.Fatal(e)
	}
	defer fresh.Close()
	result, e := ReindexTape(context.Background(), fresh, d, nil, ReadOptions{Memory: 8 << 20})
	if result.Imported != 1 || !result.ReachedEOD || e == nil {
		t.Fatalf("complete retry not imported, or incomplete tail unreported: %+v %v", result, e)
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
	m, e := scanSource(context.Background(), root, "bench", policy.Rules{})
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
	RegisterMedia(s, d, "bench", nil, false, true)
	b.SetBytes(8 << 20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, e = WriteArchive(context.Background(), s, d, m, nil, WriteOptions{Memory: 16 << 20}); e != nil {
			b.Fatal(e)
		}
	}
}
