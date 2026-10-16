package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
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
	source, target := media.Device(d), media.Device(img)
	if from {
		source, target = img, d
	}
	p, e := PlanSync(s, "", source, target, spec)
	if e != nil {
		t.Fatal(e)
	}
	r, e := ReplicaSync(context.Background(), s, source, target, p, nil, prune, ReadOptions{Memory: 8 << 20})
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
	if _, e = PlanSync(s, "", img, spy, b.ID.String()); e == nil {
		t.Fatal("skipping a missing batch accepted")
	}
	if spy.reads+spy.locates+spy.writes != 0 {
		t.Fatal("invalid scope touched tape")
	}
	p, e := PlanSync(s, "", img, d, a.ID.String())
	if e != nil {
		t.Fatal(e)
	}
	fault := &failDevice{Device: d, failAt: 3}
	if _, e = ReplicaSync(context.Background(), s, img, fault, p, nil, false, ReadOptions{Memory: 8 << 20}); e == nil {
		t.Fatal("write fault ignored")
	}
	if _, e = PlanSync(s, "", img, d, ""); e == nil {
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

func transfer(t *testing.T, s *catalog.Store, from, to media.Device, spec string) SyncResult {
	t.Helper()
	p, e := PlanSync(s, "", from, to, spec)
	if e != nil {
		t.Fatal(e)
	}
	out, e := ReplicaSync(context.Background(), s, from, to, p, nil, false, ReadOptions{Memory: 8 << 20})
	if e != nil {
		t.Fatal(e)
	}
	if out.Objects != p.Objects {
		t.Fatalf("objects: got %d want %d", out.Objects, p.Objects)
	}
	return out
}
func newTestImage(t *testing.T, r ReplicaSet) *media.Image {
	t.Helper()
	img, e := media.OpenImage(filepath.Join(t.TempDir(), "cache.img"), true, r.ID, r.KeyID)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { img.Close() })
	return img
}
func TestSparseImageTransferUsesReadOnlyCatalogAndStoredKey(t *testing.T) {
	s, d, root := setup(t)
	key := bytes.Repeat([]byte{8}, 32)
	if _, e := registerTestMember(s, d, "source", key, true, false); e != nil {
		t.Fatal(e)
	}
	r, e := ResolveReplicaSet(s, "")
	if e != nil {
		t.Fatal(e)
	}
	src, dst := newTestImage(t, r), newTestImage(t, r)
	makeSource(t, root)
	a := prepareBatch(t, s, src, root, "a", false)
	b := prepareBatch(t, s, src, root, "b", true)
	c := prepareBatch(t, s, src, root, "c", true)
	if e = src.PunchHole(b.BeginPos, b.EndPos); e != nil {
		t.Fatal(e)
	}
	ro, e := catalog.OpenReadOnly(s.Path, []byte("test passphrase"))
	if e != nil {
		t.Fatal(e)
	}
	defer ro.Close()
	if _, e = PlanSync(ro, "", src, dst, a.ID.String()+"-"+c.ID.String()); e == nil {
		t.Fatal("explicit incomplete range accepted")
	}
	p, e := PlanSync(ro, "", src, dst, "")
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Archives) != 2 || p.Archives[0].ID != a.ID.String() || p.Archives[1].ID != c.ID.String() {
		t.Fatal(p)
	}
	// A cached middle block is retained while other records are filled.
	buf := make([]byte, format.BlockSize)
	n, e := src.ReadAt(c.DataPos, buf)
	if e != nil {
		t.Fatal(e)
	}
	if e = dst.SetKey(key); e != nil {
		t.Fatal(e)
	}
	dst.Locate(c.DataPos)
	if e = dst.WriteRecord(buf[:n]); e != nil {
		t.Fatal(e)
	}
	out := transfer(t, ro, src, dst, "")
	if len(out.Archives) != 2 || !dst.HasRange(a.BeginPos, a.EndPos) || dst.HasObject(b.DataPos) || !dst.HasRange(c.BeginPos, c.EndPos) {
		t.Fatal(out)
	}
	for _, m := range []Manifest{a, c} {
		if _, e = validateImageBatch(context.Background(), dst, Meta(m), 8<<20); e != nil {
			t.Fatal(e)
		}
	}
	if next := transfer(t, ro, src, dst, ""); len(next.Archives) != 0 {
		t.Fatal("complete image reread", next)
	}
	if _, e = PlanSync(ro, "", src, src, ""); e == nil {
		t.Fatal("same image accepted")
	}
	var copies int
	if e = ro.DB.QueryRow("SELECT COUNT(*) FROM replicas").Scan(&copies); e != nil || copies != 0 {
		t.Fatal("image copy changed physical replicas", copies, e)
	}
}

func TestDeviceToDeviceUsesOneBoundedPass(t *testing.T) {
	s, src, root := setup(t)
	key := bytes.Repeat([]byte{6}, 32)
	if _, e := registerTestMember(s, src, "source", key, true, false); e != nil {
		t.Fatal(e)
	}
	r, e := ResolveReplicaSet(s, "")
	if e != nil {
		t.Fatal(e)
	}
	dst := secondMember(t, s, r)
	image := newTestImage(t, r)
	makeSource(t, root)
	a := prepareBatch(t, s, image, root, "a", false)
	b := prepareBatch(t, s, image, root, "b", true)
	transfer(t, s, image, src, "")
	from, to := &observedTape{Device: src}, &observedTape{Device: dst}
	if _, e = PlanSync(s, "", from, to, b.ID.String()); e == nil {
		t.Fatal("skipped target prefix")
	}
	if from.reads+from.locates+to.writes+to.locates != 0 {
		t.Fatal("planning moved device")
	}
	var progress uint64
	p, e := PlanSync(s, "", from, to, "")
	if e != nil {
		t.Fatal(e)
	}
	out, e := ReplicaSync(context.Background(), s, from, to, p, nil, false, ReadOptions{Memory: 8 << 20, Progress: func(n uint64, _ int64) {
		if n < progress {
			t.Error("progress regressed")
		}
		progress = n
	}})
	if e != nil {
		t.Fatal(e)
	}
	if from.locates != 1 || uint64(from.reads) != b.EndPos-a.BeginPos || from.writes != 0 || from.ends != 0 || to.ends != 1 {
		t.Fatal("extra positioning or reads", from, to)
	}
	if out.Objects != b.EndPos-a.BeginPos || len(out.Archives) != 2 {
		t.Fatal(out)
	}
	dst.Locate(a.BeginPos)
	image.Locate(a.BeginPos)
	for pos := a.BeginPos; pos < b.EndPos; pos++ {
		one, e1 := dst.ReadRecord()
		two, e2 := image.ReadRecord()
		if e1 != e2 || !bytes.Equal(one, two) {
			t.Fatal("copied stream differs", pos, e1, e2)
		}
	}
	from, to = &observedTape{Device: src}, &observedTape{Device: dst}
	if out = transfer(t, s, from, to, ""); len(out.Archives) != 0 || from.reads+from.locates+from.keys+to.writes+to.ends+to.keys != 0 {
		t.Fatal("no-op accessed stream")
	}
}

func TestDeviceSyncRejectsPlanChangesAndCapacityBeforeMovement(t *testing.T) {
	s, src, root := setup(t)
	r, _ := ResolveReplicaSet(s, "")
	dst := secondMember(t, s, r)
	img := newTestImage(t, r)
	makeSource(t, root)
	a := prepareBatch(t, s, img, root, "a", false)
	transfer(t, s, img, src, "")
	from, to := &observedTape{Device: src}, &observedTape{Device: dst}
	p, e := PlanSync(s, "", from, to, "")
	if e != nil {
		t.Fatal(e)
	}
	p.Archives[0].Name = "changed"
	if _, e = ReplicaSync(context.Background(), s, from, to, p, nil, false, ReadOptions{Memory: 8 << 20}); e == nil {
		t.Fatal("changed scope accepted")
	}
	info, _ := dst.Info()
	info.Remaining = Reserve(info.NativeLimit) + *a.EncodedBytes - 1
	if _, e = PlanSync(s, "", from, &capacityDevice{Device: to, info: info}, ""); e == nil {
		t.Fatal("insufficient capacity accepted")
	}
	if _, e = PlanSync(s, "", from, &observedTape{Device: src}, ""); e == nil {
		t.Fatal("same cartridge accepted")
	}
	if from.reads+from.locates+to.reads+to.locates+to.writes+to.keys != 0 {
		t.Fatal("failed preflight moved tape")
	}
}

type stoppedSource struct {
	media.Device
	reads int
}

func (d *stoppedSource) ReadRecordInto(b []byte) (int, error) {
	d.reads++
	if d.reads == 3 {
		return 0, errors.New("injected source read failure")
	}
	return d.Device.ReadRecordInto(b)
}
func TestDeviceCopyStopsOnReadFailure(t *testing.T) {
	s, src, root := setup(t)
	r, _ := ResolveReplicaSet(s, "")
	dst := secondMember(t, s, r)
	img := newTestImage(t, r)
	makeSource(t, root)
	prepareBatch(t, s, img, root, "a", false)
	transfer(t, s, img, src, "")
	failing := &stoppedSource{Device: src}
	from := &observedTape{Device: failing}
	p, e := PlanSync(s, "", from, dst, "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = ReplicaSync(context.Background(), s, from, dst, p, nil, false, ReadOptions{Memory: 8 << 20}); e == nil {
		t.Fatal("read failure ignored")
	}
	if from.locates != 1 || failing.reads != 3 {
		t.Fatal("retried or skipped source reads", from.locates, failing.reads)
	}
	var failed int
	if e = s.DB.QueryRow("SELECT COUNT(*) FROM replicas WHERE state='failed'").Scan(&failed); e != nil || failed != 1 {
		t.Fatal(failed, e)
	}
	if _, e = PlanSync(s, "", img, dst, ""); e == nil {
		t.Fatal("incomplete target allowed append")
	}
}
