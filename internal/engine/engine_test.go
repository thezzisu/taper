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
	r, e := ReplicaCreate(s, "test", nil, true, true)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = ReplicaAdd(s, d, r.ID, "test", "", nil); e != nil {
		t.Fatal(e)
	}
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
			label, e := registerTestMember(s, d, "test", key, true, true)
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
			if e = saveTestManifest(s, &m); e != nil {
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
			result, e := verifyAll(context.Background(), s, d, nil, ReadOptions{Memory: 8 << 20, HardwareCompression: testCompression()})
			if e != nil || !result.ReachedEOD || result.Archives != 1 {
				t.Fatal(result, e)
			}
			fresh, e := catalog.Create(filepath.Join(t.TempDir(), "fresh.db"), []byte("different password"))
			if e != nil {
				t.Fatal(e)
			}
			defer fresh.Close()
			result, e = ReindexTape(context.Background(), fresh, d, key, ReadOptions{Memory: 8 << 20, HardwareCompression: testCompression()})
			if e != nil || result.Imported != 1 {
				t.Fatal("reindex", result, e)
			}
			recovered, e := Load(fresh, m.ID.String())
			if e != nil || recovered.Digest != m.Digest {
				t.Fatal("recovered manifest", e)
			}
			copies, e := Replicas(fresh, m.ID.String())
			if e != nil || len(copies) != 1 || recovered.ReplicaID != label.Replica {
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
			registerTestMember(s, d, "test", nil, false, true)
			m, e := scanSource(context.Background(), root, "batch", policy.Rules{})
			if e != nil {
				t.Fatal(e)
			}
			saveTestManifest(s, &m)
			if _, e = WriteArchive(context.Background(), s, d, m, nil, WriteOptions{Memory: 8 << 20}); e != nil {
				t.Fatal(e)
			}
			originalID := m.ID
			m.ID = format.NewID()
			m.StreamID = ""
			if e = saveTestManifest(s, &m); e != nil {
				t.Fatal(e)
			}
			fault := &failDevice{Device: d, failAt: 2}
			if flush {
				fault.failAt, fault.flush = 0, true
			}
			if _, e = WriteArchive(context.Background(), s, fault, m, nil, WriteOptions{Memory: 8 << 20}); e == nil {
				t.Fatal("fault ignored")
			}
			rs, _ := Replicas(s, originalID.String())
			if len(rs) != 1 {
				t.Fatal("incomplete replica committed")
			}
			pending := m
			pending.StreamID = ""
			pending.ID = format.NewID()
			if err := saveTestManifest(s, &pending); err == nil {
				t.Fatal("unfinished batch allowed a new plan")
			}
			recommendations, err := Recommend(s, pending)
			if err != nil || len(recommendations) != 1 || recommendations[0].Remaining != 0 {
				t.Fatal("damaged tail recommended for another archive", recommendations, err)
			}
			spy := &observedTape{Device: d}
			if _, err := WriteArchive(context.Background(), s, spy, m, nil, WriteOptions{Memory: 8 << 20}); err == nil || !strings.Contains(err.Error(), "incomplete write") {
				t.Fatal("unresolved write was not blocked", err)
			}
			if spy.writes != 0 || spy.marks != 0 || spy.locates != 0 || spy.ends != 0 || spy.reads != 0 || spy.keys != 0 {
				t.Fatal("blocked retry operated on the tape", spy)
			}
			result, e := ReindexTape(context.Background(), s, d, nil, ReadOptions{Memory: 8 << 20, HardwareCompression: testCompression()})
			if result.Archives != 1 || !result.ReachedEOD {
				t.Fatal("old committed archive not found", result, e)
			}
		})
	}
}
func TestSourceChangeAndPolicies(t *testing.T) {
	s, d, root := setup(t)
	makeSource(t, root)
	registerTestMember(s, d, "test", nil, false, true)
	os.Mkdir(filepath.Join(root, "Tmp"), 0700)
	os.WriteFile(filepath.Join(root, "Tmp", "garbage"), []byte("junk"), 0600)
	m, e := scanSource(context.Background(), root, "batch", policy.Rules{Exclude: []string{"Tmp/**"}})
	if e != nil {
		t.Fatal(e)
	}
	if len(m.Excluded) != 1 {
		t.Fatal("rules")
	}
	saveTestManifest(s, &m)
	os.WriteFile(filepath.Join(root, "tree", "small.txt"), []byte("HELLO"), 0600)
	if _, e = WriteArchive(context.Background(), s, d, m, nil, WriteOptions{Memory: 8 << 20}); e == nil || !strings.Contains(e.Error(), "source changed") {
		t.Fatal("change ignored", e)
	}
	rs, _ := Replicas(s, m.ID.String())
	if len(rs) != 0 {
		t.Fatal("changed-source replica committed")
	}
}

func TestReindexReportsUncommittedTail(t *testing.T) {
	s, d, root := setup(t)
	makeSource(t, root)
	registerTestMember(s, d, "test", nil, false, true)
	if _, e := prepareMedia(s, d, nil); e != nil {
		t.Fatal(e)
	}
	m, e := scanSource(context.Background(), root, "test", policy.Rules{})
	if e != nil {
		t.Fatal(e)
	}
	saveTestManifest(s, &m)
	if _, e = WriteArchive(context.Background(), s, &failDevice{Device: d, failAt: 2}, m, nil, WriteOptions{Memory: 8 << 20}); e == nil {
		t.Fatal("failure ignored")
	}
	fresh, e := catalog.Create(filepath.Join(t.TempDir(), "fresh.db"), []byte("password"))
	if e != nil {
		t.Fatal(e)
	}
	defer fresh.Close()
	result, e := ReindexTape(context.Background(), fresh, d, nil, ReadOptions{Memory: 8 << 20, HardwareCompression: testCompression()})
	if result.Imported != 0 || !result.ReachedEOD || e == nil {
		t.Fatalf("incomplete tail unreported: %+v %v", result, e)
	}
}

func testCompression() *bool { v := true; return &v }
func testRange(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	if len(ids) == 1 {
		return ids[0]
	}
	return ids[0] + "-" + ids[len(ids)-1]
}
func registerTestMember(s *catalog.Store, d media.Device, label string, key []byte, save, compression bool) (MediaMember, error) {
	r, e := ResolveReplicaSet(s, "")
	if e != nil {
		return MediaMember{}, e
	}
	var stored []byte
	if save {
		stored = key
	}
	kid := ""
	if len(key) > 0 {
		kid = media.KeyID(key)
	}
	if _, e = s.DB.Exec("UPDATE replica_sets SET key_id=?,key=?,hardware_compression=? WHERE id=?", kid, stored, compression, r.ID); e != nil {
		return MediaMember{}, e
	}
	m, e := ReplicaAdd(s, d, r.ID, label, "", key)
	if e == nil {
		_, e = s.DB.Exec("UPDATE media SET key_id=?,key=?,hardware_compression=? WHERE id=?", kid, stored, compression, m.ID)
	}
	return m, e
}
func saveTestManifest(s *catalog.Store, m *Manifest) error {
	if m.ReplicaID == "" {
		r, e := ResolveReplicaSet(s, "")
		if e != nil {
			return e
		}
		m.ReplicaID = r.ID
	}
	if m.StreamID == "" {
		if e := assignBatch(s, m); e != nil {
			return e
		}
	}
	return Save(s, *m)
}
