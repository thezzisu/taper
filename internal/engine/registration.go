package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
)

func decodeLabel(raw []byte, serial string) (Label, error) {
	var l Label
	if len(raw) <= 7 || string(raw[:7]) != "taper1:" {
		return l, errors.New("cartridge has no taper MAM identity")
	}
	var f []string
	if json.Unmarshal(raw[7:], &f) != nil || len(f) != 3 {
		return l, errors.New("invalid MAM identity")
	}
	id, e := format.ParseID(f[0])
	if e != nil || id.String() != f[0] {
		return l, errors.New("invalid MAM replica ID")
	}
	return Label{ID: f[0], Name: f[1], Serial: serial, KeyID: f[2], Version: 1}, nil
}
func ReadLabel(d media.Device) (Label, error) {
	b, e := d.ReadIdentity()
	if e != nil {
		return Label{}, e
	}
	i, e := d.Info()
	if e != nil {
		return Label{}, e
	}
	return decodeLabel(b, i.Serial)
}
func UnlockMedia(s *catalog.Store, d media.Device, provided []byte) (Label, error) {
	l, e := ReadLabel(d)
	if e != nil {
		return l, e
	}
	m, e := MemberForSerial(s, l.Serial)
	if e != nil {
		return l, e
	}
	if m.Replica != l.ID {
		return l, errors.New("MAM identity differs from registered replica set")
	}
	r, e := ResolveReplicaSet(s, m.Replica)
	if e != nil {
		return l, e
	}
	if r.KeyID != l.KeyID {
		return l, errors.New("MAM key differs from replica-set key")
	}
	key, e := ReplicaKey(s, r.ID, provided)
	if e != nil {
		return l, e
	}
	defer clear(key)
	return l, d.SetKey(key)
}

// checkTarget is non-moving and never changes hardware settings or the catalog.
func CheckWriteTarget(s *catalog.Store, d media.Device, b BatchMeta) (MediaMember, error) {
	i, e := d.Info()
	if e != nil {
		return MediaMember{}, e
	}
	m, e := MemberForSerial(s, i.Serial)
	if e != nil {
		return m, e
	}
	if m.Replica != b.ReplicaID {
		return m, errors.New("loaded tape belongs to a different replica set")
	}
	if m.State != "open" && m.State != "registered" {
		return m, fmt.Errorf("cartridge state is %s", m.State)
	}
	var incomplete int
	if e = s.DB.QueryRow("SELECT COUNT(*) FROM replicas WHERE media_id=? AND state IN ('writing','failed')", m.ID).Scan(&incomplete); e != nil {
		return m, e
	}
	if incomplete > 0 {
		return m, errors.New("tape has an incomplete write; resolve its tail before appending")
	}
	all, e := ListBatches(s, m.Replica)
	if e != nil {
		return m, e
	}
	if b.Seq < 0 || b.Seq >= len(all) {
		return m, errors.New("archive is outside replica set")
	}
	expected := all[b.Seq]
	if expected.Boundary(true) != b.Boundary(true) || expected.BeginPos != b.BeginPos || expected.EndPos != b.EndPos {
		return m, errors.New("batch identity/layout differs from catalog")
	}
	prefix, e := MemberPrefix(s, m.ID, all)
	if e != nil {
		return m, e
	}
	if prefix != b.Seq {
		return m, fmt.Errorf("target's next batch is %d, requested %d; synchronize prior batches first", prefix, b.Seq)
	}
	if e = checkMediaInfo(i); e != nil {
		return m, e
	}
	limit, avail, e := i.Budget()
	if e != nil {
		return m, e
	}
	n := Budget(b.Manifest())
	if b.EncodedBytes != nil {
		n = *b.EncodedBytes
	}
	if n > max(int64(0), avail-Reserve(limit)) {
		return m, errors.New("batch exceeds loaded tape budget")
	}
	if m.State == "open" {
		l, e := ReadLabel(d)
		if e != nil {
			return m, e
		}
		r, e := ResolveReplicaSet(s, m.Replica)
		if e != nil {
			return m, e
		}
		if l.ID != m.Replica || l.Serial != m.Serial || l.KeyID != r.KeyID {
			return m, errors.New("loaded tape identity differs from catalog")
		}
	}
	return m, nil
}
func prepareMedia(s *catalog.Store, d media.Device, provided []byte) (Label, error) {
	i, e := d.Info()
	if e != nil {
		return Label{}, e
	}
	m, e := MemberForSerial(s, i.Serial)
	if e != nil {
		return Label{}, e
	}
	r, e := ResolveReplicaSet(s, m.Replica)
	if e != nil {
		return Label{}, e
	}
	l := Label{ID: r.ID, Name: m.Label, Serial: m.Serial, KeyID: r.KeyID, Version: 1}
	key, e := ReplicaKey(s, r.ID, provided)
	if e != nil {
		return l, e
	}
	defer clear(key)
	if m.State == "open" {
		actual, e := UnlockMedia(s, d, key)
		if e != nil {
			return l, e
		}
		if e = d.SetCompression(r.HardwareCompression); e != nil {
			return l, e
		}
		return actual, nil
	}
	if m.State != "registered" {
		return l, fmt.Errorf("cartridge state is %s", m.State)
	}
	if e = checkMediaInfo(i); e != nil {
		return l, e
	}
	if e = d.Locate(0); e != nil {
		return l, e
	}
	if _, e = d.ReadRecord(); e != io.EOF {
		return l, errors.New("registered tape is not empty; refusing initialization")
	}
	if e = d.SetCompression(r.HardwareCompression); e != nil {
		return l, e
	}
	if e = d.SetKey(nil); e != nil {
		return l, e
	}
	pos, e := d.Position()
	if e != nil || pos != 0 {
		return l, errors.Join(e, errors.New("empty tape position is not BOT"))
	}
	if _, e = s.DB.Exec("UPDATE media SET state='initializing' WHERE id=?", m.ID); e != nil {
		return l, e
	}
	id, _ := format.ParseID(r.ID)
	if e = control(d, format.Media, id, l); e != nil {
		return l, e
	}
	if e = d.Filemark(); e != nil {
		return l, e
	}
	raw, _ := json.Marshal([]string{l.ID, l.Name, l.KeyID})
	if e = d.WriteIdentity(append([]byte("taper1:"), raw...)); e != nil {
		return l, e
	}
	if e = d.SetKey(key); e != nil {
		return l, e
	}
	_, e = s.DB.Exec("UPDATE media SET state='open' WHERE id=?", m.ID)
	return l, e
}
