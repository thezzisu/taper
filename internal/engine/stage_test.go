package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thezzisu/taper/internal/media"
)

type unreadableIdentity struct {
	*observedTape
	identity []byte
	err      error
}

func (d *unreadableIdentity) ReadIdentity() ([]byte, error) { return d.identity, d.err }

func TestAppendRejectsBadMAMWithoutReadingTape(t *testing.T) {
	s, d, root := setup(t)
	makeSource(t, root)
	if _, e := registerTestMember(s, d, "test", nil, true, true); e != nil {
		t.Fatal(e)
	}
	if _, e := prepareMedia(s, d, nil); e != nil {
		t.Fatal(e)
	}
	if _, e := StageAdd(context.Background(), s, root, nil, nil); e != nil {
		t.Fatal(e)
	}
	if _, e := StagePlan(context.Background(), s, "append", "", false, nil); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"absent", "malformed", "invalid-id", "io-error"} {
		t.Run(name, func(t *testing.T) {
			spy := &observedTape{Device: d}
			fault := &unreadableIdentity{observedTape: spy}
			switch name {
			case "malformed":
				fault.identity = []byte("taper1:{")
			case "invalid-id":
				fault.identity = []byte(`taper1:["invalid","test",""]`)
			case "io-error":
				fault.err = errors.New("MAM unavailable")
			}
			if _, _, e := StageWrite(context.Background(), s, fault, nil, WriteOptions{Memory: 8 << 20}); e == nil {
				t.Fatal("append accepted unusable MAM")
			}
			if spy.reads+spy.writes+spy.marks+spy.locates+spy.ends+spy.keys+spy.flushes+spy.compressions != 0 {
				t.Fatalf("failed append touched tape: %+v", spy)
			}
		})
	}
}

func TestAppendBatchesThenOneFullVerification(t *testing.T) {
	s, d, root := setup(t)
	if _, e := registerTestMember(s, d, "test", nil, true, true); e != nil {
		t.Fatal(e)
	}
	if _, e := prepareMedia(s, d, nil); e != nil {
		t.Fatal(e)
	}
	var ids []string
	for i := 0; i < 2; i++ {
		if e := os.WriteFile(filepath.Join(root, "file"), []byte(fmt.Sprint(i)), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := StageAdd(context.Background(), s, root, nil, nil); e != nil {
			t.Fatal(e)
		}
		if _, e := StagePlan(context.Background(), s, "batch", "", true, nil); e != nil {
			t.Fatal(e)
		}
		spy := &observedTape{Device: d}
		id, _, e := StageWrite(context.Background(), s, spy, nil, WriteOptions{Memory: 8 << 20})
		if e != nil {
			t.Fatal(e)
		}
		if spy.reads != 0 || spy.locates != 0 || spy.ends != 1 || spy.writes == 0 {
			t.Fatalf("append did not stay write-only: %+v", spy)
		}
		replicas, e := Replicas(s, id)
		if e != nil || len(replicas) != 1 || replicas[0].Verified {
			t.Fatal("append must remain unverified", replicas, e)
		}
		ids = append(ids, id)
	}
	end, _ := d.Position()
	spy := &observedTape{Device: d}
	result, e := verifyAll(context.Background(), s, spy, nil, ReadOptions{Memory: 8 << 20, HardwareCompression: testCompression()})
	if e != nil || !result.ReachedEOD || result.Archives != 2 || result.Records != end || spy.locates != 1 || spy.ends != 0 || spy.writes != 0 || uint64(spy.reads) != end+1 {
		t.Fatal("final verification was not one forward pass", result, e, spy)
	}
	for _, id := range ids {
		replicas, e := Replicas(s, id)
		if e != nil || len(replicas) != 1 || !replicas[0].Verified {
			t.Fatal("final pass did not verify every batch", replicas, e)
		}
	}
}

type observedTape struct {
	media.Device
	reads, writes, marks, locates, ends, keys, flushes int
	compressions                                       int
	compressionError                                   error
	corrupt                                            uint64
	corruptEnabled                                     bool
}

func (d *observedTape) ReadRecord() ([]byte, error) { d.reads++; return d.Device.ReadRecord() }
func (d *observedTape) ReadRecordInto(b []byte) (int, error) {
	d.reads++
	pos, _ := d.Device.Position()
	n, e := d.Device.ReadRecordInto(b)
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
func (d *observedTape) SetCompression(enabled bool) error {
	d.compressions++
	if d.compressionError != nil {
		return d.compressionError
	}
	return d.Device.SetCompression(enabled)
}

func TestMediaRegistrationIsReadOnlyOnOccupiedTape(t *testing.T) {
	s, d, root := setup(t)
	d.WriteRecord([]byte("existing foreign data"))
	d.Filemark()
	pos, _ := d.Position()
	spy := &observedTape{Device: d}
	if _, e := registerTestMember(s, spy, "existing", nil, true, true); e != nil {
		t.Fatal(e)
	}
	after, _ := d.Position()
	if after != pos || spy.reads+spy.writes+spy.marks+spy.locates+spy.ends+spy.keys+spy.flushes+spy.compressions != 0 {
		t.Fatalf("registration touched tape: %+v", spy)
	}
	os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0600)
	StageAdd(context.Background(), s, root, nil, nil)
	if _, e := StagePlan(context.Background(), s, "archive", "", false, nil); e != nil {
		t.Fatal(e)
	}
	if _, _, e := StageWrite(context.Background(), s, spy, nil, WriteOptions{Memory: 8 << 20}); e == nil {
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
	first, e := StagePlan(context.Background(), s, "first", "", false, nil)
	if e != nil {
		t.Fatal(e)
	}
	same, e := StagePlan(context.Background(), s, "", "", false, nil)
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
	if _, _, e := StageWrite(context.Background(), s, d, nil, WriteOptions{Memory: 8 << 20}); e == nil {
		t.Fatal("unplanned selection written")
	}
	m, e := StagePlan(context.Background(), s, "final", "", false, nil)
	if e != nil {
		t.Fatal(e)
	}
	if m.Files != len(files)-1 {
		t.Fatal(m.Files)
	}
	if _, e = registerTestMember(s, d, "one", nil, true, true); e != nil {
		t.Fatal(e)
	}
	id, _, e := StageWrite(context.Background(), s, d, nil, WriteOptions{Memory: 8 << 20})
	if e != nil || id != m.ID.String() {
		t.Fatal(id, e)
	}
	st, _ = LoadStage(s)
	if len(st.Files) != 0 || st.Plan != "" {
		t.Fatal("stage did not clear after a complete batch")
	}
	pos, _ := d.Position()
	if _, _, e = StageWrite(context.Background(), s, d, nil, WriteOptions{Memory: 8 << 20}); e == nil {
		t.Fatal("empty stage written")
	}
	after, _ := d.Position()
	if pos != after {
		t.Fatal("empty stage moved tape")
	}
	status, e := BatchStatus(s, m)
	if e != nil || status.Committed != 1 || !status.Covered {
		t.Fatal(status, e)
	}
}

func TestStagingRejectsChangedAliasBeforeTapeIO(t *testing.T) {
	s, d, root := setup(t)
	makeSource(t, root)
	StageAdd(context.Background(), s, root, nil, nil)
	if _, e := StagePlan(context.Background(), s, "test", "", false, nil); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(root, "tree/large.bin"), []byte("changed alias"), 0600)
	spy := &observedTape{Device: d}
	if _, _, e := StageWrite(context.Background(), s, spy, nil, WriteOptions{Memory: 8 << 20}); e == nil {
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
	if _, e := StagePlan(context.Background(), s, "test", "", false, nil); e != nil {
		t.Fatal(e)
	}
	registerTestMember(s, d, "test", nil, true, true)
	if _, _, e := StageWrite(context.Background(), s, &failDevice{Device: d, failAt: 2}, nil, WriteOptions{Memory: 8 << 20}); e == nil {
		t.Fatal("injected failure ignored")
	}
	st, _ = LoadStage(s)
	if len(st.Files) != 1 || st.Plan == "" {
		t.Fatal("failure discarded plan")
	}
	if _, e := StageRemove(s, []string{"."}); e == nil {
		t.Fatal("unfinished image/tape plan was discarded")
	}
	st, _ = LoadStage(s)
	if st.Plan == "" {
		t.Fatal("failed plan identity was lost")
	}
}
