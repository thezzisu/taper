package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/media"
)

func secondMember(t *testing.T, s *catalog.Store, r ReplicaSet) *media.Sim {
	t.Helper()
	file := filepath.Join(t.TempDir(), "member.tape")
	if e := media.CreateSim(file, 8<<30, false); e != nil {
		t.Fatal(e)
	}
	d, e := media.OpenSim(file, true)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	if _, e = ReplicaAdd(s, d, r.ID, "second", "spare", nil); e != nil {
		t.Fatal(e)
	}
	return d
}
func prepareBatch(t *testing.T, s *catalog.Store, img *media.Image, root, name string, zstd bool) Manifest {
	t.Helper()
	if _, e := StageAdd(context.Background(), s, root, nil, nil); e != nil {
		t.Fatal(e)
	}
	m, e := StagePlan(context.Background(), s, name, "", false, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = StageWrite(context.Background(), s, nil, nil, WriteOptions{Image: img, Memory: 8 << 20, Zstd: zstd}); e != nil {
		t.Fatal(e)
	}
	m, e = Load(s, m.ID.String())
	if e != nil {
		t.Fatal(e)
	}
	return m
}
func syncTest(t *testing.T, s *catalog.Store, d media.Device, img *media.Image, spec string, from, prune bool) SyncResult {
	t.Helper()
	p, e := PlanSync(s, d, img, spec, from)
	if e != nil {
		t.Fatal(e)
	}
	r, e := TapeSync(context.Background(), s, d, img, p, nil, prune, ReadOptions{Memory: 8 << 20})
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func TestImageFirstSyncReadOnlyCaptureAndPrune(t *testing.T) {
	s, first, root := setup(t)
	key := bytes.Repeat([]byte{9}, 32)
	if _, e := registerTestMember(s, first, "first", key, true, false); e != nil {
		t.Fatal(e)
	}
	set, e := ResolveReplicaSet(s, "")
	if e != nil {
		t.Fatal(e)
	}
	second := secondMember(t, s, set)
	img, e := media.OpenImage(filepath.Join(t.TempDir(), "stream.img"), true, set.ID, set.KeyID)
	if e != nil {
		t.Fatal(e)
	}
	defer img.Close()
	makeSource(t, root)
	a := prepareBatch(t, s, img, root, "raw", false)
	if e = os.WriteFile(filepath.Join(root, "new"), []byte("second archive"), 0600); e != nil {
		t.Fatal(e)
	}
	b := prepareBatch(t, s, img, root, "compressed", true)
	if st, e := LoadStage(s); e != nil || st.Plan != "" {
		t.Fatal("image commit did not clear stage", st, e)
	}
	if a.Seq != 0 || a.BeginPos != 2 || b.Seq != 1 || b.BeginPos != a.EndPos || a.StreamID == b.StreamID || a.EncodedBytes == nil || b.EncodedBytes == nil {
		t.Fatal("invalid deterministic layout")
	}
	if got := syncTest(t, s, first, img, "", true, true); len(got.Archives) != 2 || got.PrunedEnd != 0 {
		t.Fatal(got)
	}
	if got := syncTest(t, s, second, img, a.ID.String(), true, true); got.PrunedEnd != a.EndPos {
		t.Fatal(got)
	}
	if img.HasObject(a.DataPos) || !img.HasRange(b.BeginPos, b.EndPos) {
		t.Fatal("prune removed data still owed to a member")
	}
	if got := syncTest(t, s, second, img, "", true, true); got.PrunedEnd != b.EndPos {
		t.Fatal(got)
	}
	if img.HasObject(b.DataPos) {
		t.Fatal("fully replicated prefix was not pruned")
	}
	// Physical labels differ; every batch object at LBA >=2 is identical.
	for _, d := range []*media.Sim{first, second} {
		if e = d.SetKey(key); e != nil {
			t.Fatal(e)
		}
		if e = d.Locate(2); e != nil {
			t.Fatal(e)
		}
	}
	for pos := uint64(2); pos < b.EndPos; pos++ {
		one, e1 := first.ReadRecord()
		two, e2 := second.ReadRecord()
		if e1 != e2 || !bytes.Equal(one, two) {
			t.Fatalf("member stream differs at %d: %v %v", pos, e1, e2)
		}
	}
	ro, e := catalog.OpenReadOnly(s.Path, []byte("test passphrase"))
	if e != nil {
		t.Fatal(e)
	}
	defer ro.Close()
	var samplesBefore int
	s.DB.QueryRow("SELECT COUNT(*) FROM media_samples").Scan(&samplesBefore)
	spy := &observedTape{Device: first}
	if got := syncTest(t, ro, spy, img, "", false, false); len(got.Archives) != 2 || spy.locates != 1 || uint64(spy.reads) != b.EndPos-a.BeginPos {
		t.Fatal("capture was not one bounded pass", got, spy)
	}
	var samplesAfter int
	s.DB.QueryRow("SELECT COUNT(*) FROM media_samples").Scan(&samplesAfter)
	if samplesBefore != samplesAfter {
		t.Fatal("capture changed SQLite")
	}
	if !img.HasRange(a.BeginPos, b.EndPos) {
		t.Fatal("capture omitted records")
	}
	spy = &observedTape{Device: first}
	syncTest(t, ro, spy, img, "", false, false)
	if spy.reads+spy.locates+spy.keys != 0 {
		t.Fatal("complete image caused another tape read")
	}
	// A current member is a no-op; sparse missing prefixes do not force a replay.
	spy = &observedTape{Device: first}
	syncTest(t, s, spy, img, "", true, false)
	if spy.reads+spy.locates+spy.writes+spy.marks+spy.ends != 0 {
		t.Fatal("current tape was rewritten")
	}
}
func TestSyncRejectsMissingPrefixAndRecordsFailedTail(t *testing.T) {
	s, d, root := setup(t)
	set, _ := ResolveReplicaSet(s, "")
	img, e := media.OpenImage(filepath.Join(t.TempDir(), "partial.img"), true, set.ID, set.KeyID)
	if e != nil {
		t.Fatal(e)
	}
	defer img.Close()
	makeSource(t, root)
	a := prepareBatch(t, s, img, root, "a", false)
	b := prepareBatch(t, s, img, root, "b", false)
	spy := &observedTape{Device: d}
	if _, e = PlanSync(s, spy, img, b.ID.String(), true); e == nil {
		t.Fatal("skipping a missing batch accepted")
	}
	if spy.reads+spy.locates+spy.writes != 0 {
		t.Fatal("invalid scope touched tape")
	}
	p, e := PlanSync(s, d, img, a.ID.String(), true)
	if e != nil {
		t.Fatal(e)
	}
	fault := &failDevice{Device: d, failAt: 3}
	if _, e = TapeSync(context.Background(), s, fault, img, p, nil, false, ReadOptions{Memory: 8 << 20}); e == nil {
		t.Fatal("write fault ignored")
	}
	if _, e = PlanSync(s, d, img, "", true); e == nil {
		t.Fatal("failed target tail accepted")
	}
}
func TestStageBothDestinationsShareOneStream(t *testing.T) {
	s, d, root := setup(t)
	makeSource(t, root)
	set, _ := ResolveReplicaSet(s, "")
	img, e := media.OpenImage(filepath.Join(t.TempDir(), "both.img"), true, set.ID, set.KeyID)
	if e != nil {
		t.Fatal(e)
	}
	defer img.Close()
	StageAdd(context.Background(), s, root, nil, nil)
	m, e := StagePlan(context.Background(), s, "both", "", false, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = StageWrite(context.Background(), s, d, nil, WriteOptions{Image: img, Memory: 8 << 20, Zstd: true}); e != nil {
		t.Fatal(e)
	}
	if e = d.Locate(m.BeginPos); e != nil {
		t.Fatal(e)
	}
	if e = img.Locate(m.BeginPos); e != nil {
		t.Fatal(e)
	}
	for p := m.BeginPos; p < m.EndPos; p++ {
		raw, one := d.ReadRecord()
		cached, two := img.ReadRecord()
		if one != two || !bytes.Equal(raw, cached) {
			t.Fatal("image/tape bytes differ", p, one, two)
		}
	}
}
