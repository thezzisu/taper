package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/thezzisu/taper/internal/media"
)

func TestReplicaLabelsNotesAndSingleSelection(t *testing.T) {
	s, d, _ := setup(t)
	r, e := ResolveReplicaSet(s, "")
	if e != nil {
		t.Fatal(e)
	}
	info, _ := d.Info()
	info.Barcode = "123456L6"
	spy := &observedTape{Device: &capacityDevice{Device: d, info: info}}
	m, e := ReplicaAdd(s, spy, "", "ignored", "drawer 2", nil)
	if e != nil || m.Label != "123456L6" || m.Note != "drawer 2" {
		t.Fatal(m, e)
	}
	if spy.reads+spy.locates+spy.keys+spy.compressions != 0 {
		t.Fatal("binding moved tape")
	}
	raw, _ := json.Marshal([]string{r.ID, "654321L6", r.KeyID})
	if e = d.WriteIdentity(append([]byte("taper1:"), raw...)); e != nil {
		t.Fatal(e)
	}
	m, e = ReplicaAdd(s, spy, "", "other", "new note", nil)
	if e != nil || m.Label != "654321L6" || m.Note != "new note" {
		t.Fatal(m, e)
	}
	if _, e = ReplicaCreate(s, "another", nil, true, true); e != nil {
		t.Fatal(e)
	}
	if _, e = ResolveReplicaSet(s, ""); e == nil {
		t.Fatal("ambiguous set selected")
	}
	if _, e = ReplicaAdd(s, d, "another", "", "", nil); e == nil {
		t.Fatal("foreign MAM identity accepted")
	}
}
func TestReplicaBudgetBottleneckAndLag(t *testing.T) {
	s, d, root := setup(t)
	makeSource(t, root)
	set, _ := ResolveReplicaSet(s, "")
	other := secondMember(t, s, set)
	a, _ := d.Info()
	b, _ := other.Info()
	a.Remaining = 5 << 30
	b.Remaining = 3 << 30
	ma, _ := MemberForSerial(s, a.Serial)
	mb, _ := MemberForSerial(s, b.Serial)
	recordMediaInfo(s, ma.ID, a)
	recordMediaInfo(s, mb.ID, b)
	budget, e := ReplicaBudget(s, set.ID)
	if e != nil || budget != (3<<30)-Reserve(b.NativeLimit) {
		t.Fatal(budget, e)
	}
	StageAdd(context.Background(), s, root, nil, nil)
	m, e := StagePlan(context.Background(), s, "lag", "", false, nil)
	if e != nil {
		t.Fatal(e)
	}
	img, e := media.OpenImage(filepath.Join(t.TempDir(), "lag.img"), true, set.ID, set.KeyID)
	if e != nil {
		t.Fatal(e)
	}
	defer img.Close()
	if _, _, e = StageWrite(context.Background(), s, nil, nil, WriteOptions{Image: img, Memory: 8 << 20}); e != nil {
		t.Fatal(e)
	}
	m, _ = Load(s, m.ID.String())
	expected := budget - *m.EncodedBytes
	if got, e := ReplicaBudget(s, set.ID); e != nil || got != expected {
		t.Fatal(got, expected, e)
	}
	s.DB.Exec("UPDATE batches SET encoded_bytes=NULL WHERE id=?", m.ID.String())
	if got, e := ReplicaBudget(s, set.ID); e != nil || got != budget-Budget(m) {
		t.Fatal("fallback upper bound", got, e)
	}
	s.DB.Exec("UPDATE media_health SET unhealthy=1 WHERE media_id=?", ma.ID)
	if got, e := ReplicaBudget(s, set.ID); e != nil || got != 0 {
		t.Fatal("unhealthy member ignored", got, e)
	}
}
func TestReplicaRemovalAndKeyInheritance(t *testing.T) {
	s, d, _ := setup(t)
	key := bytes.Repeat([]byte{7}, 32)
	r, _ := ResolveReplicaSet(s, "")
	s.DB.Exec("UPDATE replica_sets SET key_id=?,key=? WHERE id=?", media.KeyID(key), key, r.ID)
	other := secondMember(t, s, r)
	i, _ := other.Info()
	m, _ := MemberForSerial(s, i.Serial)
	var saved []byte
	if e := s.DB.QueryRow("SELECT key FROM media WHERE id=?", m.ID).Scan(&saved); e != nil || !bytes.Equal(saved, key) {
		t.Fatal("member did not inherit key", e)
	}
	p, e := PlanReplicaRemoval(s, "", nil, m.ID)
	if e != nil || p.Last {
		t.Fatal(p, e)
	}
	if e = ReplicaRemove(s, p); e != nil {
		t.Fatal(e)
	}
	p, e = PlanReplicaRemoval(s, "", d, "")
	if e != nil || !p.Last {
		t.Fatal(p, e)
	}
	if e = ReplicaRemove(s, p); e != nil {
		t.Fatal(e)
	}
	if b, e := ReplicaBudget(s, r.ID); e != nil || b != 0 {
		t.Fatal(b, e)
	}
}
func TestArchiveRangePrefixes(t *testing.T) {
	all := []BatchMeta{{ID: "12345600000000000000000000000001"}, {ID: "abcdef00000000000000000000000002"}, {ID: "fedcba00000000000000000000000003"}}
	for spec, want := range map[string]int{"": 3, "123456": 1, "ABCDEF-fedcba": 2, "abcdef-": 2, "-abcdef": 2} {
		v, e := ParseArchiveRange(spec, all)
		if e != nil || len(v) != want {
			t.Fatal(spec, v, e)
		}
	}
	for _, spec := range []string{"-", "abc", "abcdef-fedcba-123456", "fedcba-abcdef", "unknown", "aaaaaa"} {
		if _, e := ParseArchiveRange(spec, all); e == nil {
			t.Fatal("invalid range", spec)
		}
	}
	all = append(all, BatchMeta{ID: "abcdef00000000000000000000000004"})
	if _, e := ParseArchiveRange("abcdef", all); e == nil {
		t.Fatal("ambiguous prefix accepted")
	}
}
