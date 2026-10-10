package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/thezzisu/taper/internal/media"
)

type observedTape struct {
	media.Device
	reads, writes, marks, locates, ends, keys, flushes int
	corrupt                                            uint64
	corruptEnabled                                     bool
}

func (d *observedTape) ReadRecord() ([]byte, error) { d.reads++; return d.Device.ReadRecord() }
func (d *observedTape) ReadRecordInto(b []byte) (int, error) {
	d.reads++
	pos, _ := d.Device.Position()
	n, e := d.Device.(media.RecordReader).ReadRecordInto(b)
	if e == nil && d.corruptEnabled && pos == d.corrupt && n > 64 {
		b[n-1] ^= 1
	}
	return n, e
}
func (d *observedTape) WriteRecord(b []byte) error { d.writes++; return d.Device.WriteRecord(b) }
func (d *observedTape) Filemark() error            { d.marks++; return d.Device.Filemark() }
func (d *observedTape) Locate(p uint64) error      { d.locates++; return d.Device.Locate(p) }
func (d *observedTape) End() error                 { d.ends++; return d.Device.End() }
func (d *observedTape) SetKey(k []byte) error      { d.keys++; return d.Device.SetKey(k) }
func (d *observedTape) Flush() error               { d.flushes++; return d.Device.Flush() }
func (d *observedTape) ReadIdentity() ([]byte, error) {
	return d.Device.(media.Identity).ReadIdentity()
}
func (d *observedTape) WriteIdentity(b []byte) error {
	return d.Device.(media.Identity).WriteIdentity(b)
}

func TestMediaRegistrationIsReadOnlyOnOccupiedTape(t *testing.T) {
	s, d, root := setup(t)
	d.WriteRecord([]byte("existing foreign data"))
	d.Filemark()
	pos, _ := d.Position()
	spy := &observedTape{Device: d}
	if _, e := RegisterMedia(s, spy, "existing", nil, true); e != nil {
		t.Fatal(e)
	}
	after, _ := d.Position()
	if after != pos || spy.reads+spy.writes+spy.marks+spy.locates+spy.ends+spy.keys+spy.flushes != 0 {
		t.Fatalf("registration touched tape: %+v", spy)
	}
	os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0600)
	StageAdd(context.Background(), s, root, nil, nil)
	if _, e := StagePlan(context.Background(), s, "archive", 1, true, false, nil); e != nil {
		t.Fatal(e)
	}
	if _, _, e := StageWrite(context.Background(), s, spy, nil, WriteOptions{Memory: 8 << 20, AllowFrozen: true}); e == nil {
		t.Fatal("occupied tape overwritten")
	}
	if spy.writes != 0 || spy.marks != 0 {
		t.Fatal("write reached foreign tape")
	}
}

func TestStagingSelectionPlanAndReplicaLifecycle(t *testing.T) {
	s, d, root := setup(t)
	files := makeSource(t, root)
	if _, e := StageAdd(context.Background(), s, root, []string{"."}, nil); e != nil {
		t.Fatal(e)
	}
	first, e := StagePlan(context.Background(), s, "first", 2, true, false, nil)
	if e != nil {
		t.Fatal(e)
	}
	same, e := StagePlan(context.Background(), s, "", 0, true, false, nil)
	if e != nil || same.ID != first.ID {
		t.Fatal("plan is not stable", e)
	}
	if n, e := StageRemove(s, []string{"tree/small.txt"}); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if got, e := os.ReadFile(filepath.Join(root, "tree/small.txt")); e != nil || !bytes.Equal(got, files["tree/small.txt"]) {
		t.Fatal("rm changed source data", e)
	}
	st, _ := LoadStage(s)
	if st.Plan != "" {
		t.Fatal("stale plan survived selection edit")
	}
	var old int
	s.DB.QueryRow("SELECT COUNT(*) FROM batches WHERE id=?", first.ID.String()).Scan(&old)
	if old != 0 {
		t.Fatal("unused old plan was retained")
	}
	if _, _, e := StageWrite(context.Background(), s, d, nil, WriteOptions{Memory: 8 << 20, AllowFrozen: true}); e == nil {
		t.Fatal("unplanned selection written")
	}
	m, e := StagePlan(context.Background(), s, "final", 2, true, false, nil)
	if e != nil {
		t.Fatal(e)
	}
	if m.Files != len(files)-1 {
		t.Fatal(m.Files)
	}
	if _, e = RegisterMedia(s, d, "one", nil, true); e != nil {
		t.Fatal(e)
	}
	id, _, e := StageWrite(context.Background(), s, d, nil, WriteOptions{Memory: 8 << 20, AllowFrozen: true})
	if e != nil || id != m.ID.String() {
		t.Fatal(id, e)
	}
	st, _ = LoadStage(s)
	if st.Plan != id {
		t.Fatal("stage cleared before required copies existed")
	}
	pos, _ := d.Position()
	if _, _, e = StageWrite(context.Background(), s, d, nil, WriteOptions{Memory: 8 << 20, AllowFrozen: true}); e == nil {
		t.Fatal("duplicate same-tape copy allowed")
	}
	after, _ := d.Position()
	if after != pos {
		t.Fatal("duplicate attempt moved tape")
	}
	name := filepath.Join(t.TempDir(), "second.tape")
	media.CreateSim(name, 8<<30, false)
	other, e := media.OpenSim(name, true)
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	if _, e = RegisterMedia(s, other, "two", nil, true); e != nil {
		t.Fatal(e)
	}
	if _, _, e = StageWrite(context.Background(), s, other, nil, WriteOptions{Memory: 8 << 20, AllowFrozen: true}); e != nil {
		t.Fatal(e)
	}
	st, _ = LoadStage(s)
	if len(st.Files) != 0 || st.Plan != "" {
		t.Fatal("committed staging area was not cleared")
	}
	status, e := BatchStatus(s, m)
	if e != nil || status.Committed != 2 || !status.Covered {
		t.Fatal(status, e)
	}
}

func TestStagingRejectsChangedAliasBeforeTapeIO(t *testing.T) {
	s, d, root := setup(t)
	makeSource(t, root)
	StageAdd(context.Background(), s, root, nil, nil)
	if _, e := StagePlan(context.Background(), s, "test", 1, true, false, nil); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(root, "tree/large.bin"), []byte("changed alias"), 0600)
	spy := &observedTape{Device: d}
	if _, _, e := StageWrite(context.Background(), s, spy, nil, WriteOptions{Memory: 8 << 20, AllowFrozen: true}); e == nil {
		t.Fatal("changed alias accepted")
	}
	if spy.reads+spy.writes+spy.marks+spy.locates+spy.ends+spy.keys != 0 {
		t.Fatal("tape was touched for stale input")
	}
	st, _ := LoadStage(s)
	if st.Plan == "" || len(st.Files) == 0 {
		t.Fatal("failed write lost staging area")
	}
}

func TestStageAddRefreshAndWriterFailure(t *testing.T) {
	s, d, root := setup(t)
	makeSource(t, root)
	if n, e := StageAdd(context.Background(), s, root, []string{"tree", "tree/small.txt"}, nil); e != nil || n != 2 {
		t.Fatal(n, e)
	}
	os.Remove(filepath.Join(root, "tree/small.txt"))
	if _, e := StageAdd(context.Background(), s, root, []string{"tree"}, nil); e != nil {
		t.Fatal(e)
	}
	st, _ := LoadStage(s)
	if len(st.Files) != 1 {
		t.Fatal("deleted path remained staged", st.Files)
	}
	if _, e := StagePlan(context.Background(), s, "test", 1, true, false, nil); e != nil {
		t.Fatal(e)
	}
	RegisterMedia(s, d, "test", nil, true)
	if _, _, e := StageWrite(context.Background(), s, &failDevice{Device: d, failAt: 2}, nil, WriteOptions{Memory: 8 << 20, AllowFrozen: true}); e == nil {
		t.Fatal("injected failure ignored")
	}
	st, _ = LoadStage(s)
	if len(st.Files) != 1 || st.Plan == "" {
		t.Fatal("failure discarded plan")
	}
	if n, e := StageRemove(s, []string{"."}); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	st, _ = LoadStage(s)
	if st.Source != "" {
		t.Fatal("clear did not reset source root")
	}
}
