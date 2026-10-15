package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
)

type ReplicaSet struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	KeyID               string `json:"key_id"`
	HardwareCompression bool   `json:"hardware_compression"`
	Members             int    `json:"members"`
}
type MediaMember struct {
	ID      string `json:"id"`
	Replica string `json:"replica_id"`
	Label   string `json:"label"`
	Note    string `json:"note"`
	Serial  string `json:"serial"`
	State   string `json:"state"`
}

func ReplicaCreate(s *catalog.Store, name string, key []byte, saveKey, compression bool) (ReplicaSet, error) {
	r := ReplicaSet{ID: format.NewID().String(), Name: name, HardwareCompression: compression}
	if strings.TrimSpace(name) == "" {
		return r, errors.New("replica name is required")
	}
	if len(key) != 0 && len(key) != 32 {
		return r, errors.New("key must be 32 bytes")
	}
	var stored []byte
	if len(key) > 0 {
		r.KeyID = media.KeyID(key)
		if saveKey {
			stored = key
		}
	}
	release, e := lockStage(s)
	if e != nil {
		return r, e
	}
	defer release()
	_, e = s.DB.Exec("INSERT INTO replica_sets(id,name,key_id,key,hardware_compression) VALUES(?,?,?,?,?)", r.ID, r.Name, r.KeyID, stored, compression)
	return r, e
}
func ResolveReplicaSet(s *catalog.Store, name string) (ReplicaSet, error) {
	var r ReplicaSet
	if name == "" {
		var n int
		if e := s.DB.QueryRow("SELECT COUNT(*) FROM replica_sets").Scan(&n); e != nil {
			return r, e
		}
		if n != 1 {
			return r, errors.New("specify a replica name; automatic selection requires exactly one replica set")
		}
	}
	e := s.DB.QueryRow("SELECT id,name,key_id,hardware_compression,(SELECT COUNT(*) FROM media WHERE replica_id=rs.id) FROM replica_sets rs WHERE ?='' OR name=? OR id=?", name, name, name).Scan(&r.ID, &r.Name, &r.KeyID, &r.HardwareCompression, &r.Members)
	return r, e
}
func ReplicaList(s *catalog.Store) ([]ReplicaSet, error) {
	rows, e := s.DB.Query("SELECT id,name,key_id,hardware_compression,(SELECT COUNT(*) FROM media WHERE replica_id=rs.id) FROM replica_sets rs ORDER BY name")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []ReplicaSet{}
	for rows.Next() {
		var r ReplicaSet
		if e = rows.Scan(&r.ID, &r.Name, &r.KeyID, &r.HardwareCompression, &r.Members); e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func ReplicaKey(s *catalog.Store, id string, provided []byte) ([]byte, error) {
	var keyID string
	var saved []byte
	if e := s.DB.QueryRow("SELECT key_id,key FROM replica_sets WHERE id=?", id).Scan(&keyID, &saved); e != nil {
		return nil, e
	}
	if len(provided) > 0 {
		clear(saved)
		saved = append([]byte(nil), provided...)
	}
	if keyID == "" {
		clear(saved)
		if len(provided) > 0 {
			return nil, errors.New("replica set is not encrypted")
		}
		return nil, nil
	}
	if len(saved) != 32 || media.KeyID(saved) != keyID {
		clear(saved)
		return nil, errors.New("matching replica-set key required")
	}
	return saved, nil
}
func MemberForSerial(s *catalog.Store, serial string) (MediaMember, error) {
	var m MediaMember
	e := s.DB.QueryRow("SELECT id,replica_id,label,note,serial,state FROM media WHERE serial=?", serial).Scan(&m.ID, &m.Replica, &m.Label, &m.Note, &m.Serial, &m.State)
	return m, e
}

// ReplicaAdd observes MAM and capacity only. It neither changes a drive mode nor
// reads a tape record. A formatted cartridge must carry this set's identity.
func ReplicaAdd(s *catalog.Store, d media.Device, name, label, note string, key []byte) (MediaMember, error) {
	release, e := lockStage(s)
	if e != nil {
		return MediaMember{}, e
	}
	defer release()
	r, e := ResolveReplicaSet(s, name)
	if e != nil {
		return MediaMember{}, e
	}
	i, e := d.Info()
	if e != nil {
		return MediaMember{}, e
	}
	if i.Serial == "" {
		return MediaMember{}, errors.New("hardware cartridge serial is unavailable")
	}
	identity, e := d.ReadIdentity()
	if e != nil && !errors.Is(e, media.ErrIdentityAbsent) {
		return MediaMember{}, e
	}
	m := MediaMember{ID: format.NewID().String(), Replica: r.ID, Serial: i.Serial, State: "registered", Note: note}
	if strings.HasPrefix(string(identity), "taper1:") {
		l, e := decodeLabel(identity, i.Serial)
		if e != nil {
			return m, e
		}
		if l.ID != r.ID || l.KeyID != r.KeyID {
			return m, errors.New("cartridge belongs to another replica set or key")
		}
		m.Label = l.Name
		m.State = "open"
	}
	if m.Label == "" {
		m.Label = i.Barcode
	}
	if m.Label == "" {
		m.Label = label
	}
	if m.Label == "" {
		m.Label = i.Serial
	}
	if len(m.Label) > 64 {
		return m, errors.New("cartridge label exceeds 64 bytes")
	}
	if len(key) > 0 && (len(key) != 32 || r.KeyID == "" || media.KeyID(key) != r.KeyID) {
		return m, errors.New("key does not match replica set")
	}
	var saved []byte
	if e = s.DB.QueryRow("SELECT key FROM replica_sets WHERE id=?", r.ID).Scan(&saved); e != nil {
		return m, e
	}
	defer clear(saved)
	old, e := MemberForSerial(s, i.Serial)
	if e == nil {
		if old.Replica != r.ID {
			return m, errors.New("cartridge is already bound to another replica set")
		}
		m.ID = old.ID
		m.State = old.State
	} else if !errors.Is(e, sql.ErrNoRows) {
		return m, e
	}
	_, e = s.DB.Exec(`INSERT INTO media(id,replica_id,label,note,serial,total,remaining,sampled,state,key_id,key,hardware_compression) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(id) DO UPDATE SET replica_id=excluded.replica_id,label=excluded.label,note=excluded.note,total=excluded.total,remaining=excluded.remaining,sampled=excluded.sampled,state=excluded.state,key_id=excluded.key_id,key=excluded.key,hardware_compression=excluded.hardware_compression`, m.ID, r.ID, m.Label, m.Note, m.Serial, i.Total, i.Remaining, i.Sampled, m.State, r.KeyID, saved, r.HardwareCompression)
	if e != nil {
		return m, e
	}
	e = recordMediaInfo(s, m.ID, i)
	return m, e
}

type RemovalPlan struct {
	MediaMember
	Last bool `json:"last_member"`
}

func PlanReplicaRemoval(s *catalog.Store, name string, d media.Device, identifier string) (RemovalPlan, error) {
	var p RemovalPlan
	r, e := ResolveReplicaSet(s, name)
	if e != nil {
		return p, e
	}
	if (d == nil) == (identifier == "") {
		return p, errors.New("provide exactly one of --device or --tape")
	}
	if d != nil {
		i, e := d.Info()
		if e != nil {
			return p, e
		}
		m, e := MemberForSerial(s, i.Serial)
		if e != nil {
			return p, e
		}
		p.MediaMember = m
		if m.Replica != r.ID {
			return p, errors.New("loaded cartridge is not a member of this replica set")
		}
	} else {
		rows, e := s.DB.Query("SELECT id,replica_id,label,note,serial,state FROM media WHERE replica_id=? AND (id=? OR label=? OR serial=?)", r.ID, identifier, identifier, identifier)
		if e != nil {
			return p, e
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			if e = rows.Scan(&p.ID, &p.Replica, &p.Label, &p.Note, &p.Serial, &p.State); e != nil {
				return p, e
			}
			n++
		}
		if e = rows.Err(); e != nil {
			return p, e
		}
		if n != 1 {
			return p, fmt.Errorf("tape identifier matches %d members; provide a unique serial or ID", n)
		}
	}
	p.Last = r.Members == 1
	return p, nil
}
func ReplicaRemove(s *catalog.Store, p RemovalPlan) error {
	release, e := lockStage(s)
	if e != nil {
		return e
	}
	defer release()
	var n int
	if e = s.DB.QueryRow("SELECT COUNT(*) FROM media WHERE replica_id=?", p.Replica).Scan(&n); e != nil {
		return e
	}
	if (n == 1) != p.Last {
		return errors.New("replica membership changed; review removal again")
	}
	current, e := MemberForSerial(s, p.Serial)
	if e != nil {
		return e
	}
	if current != p.MediaMember {
		return errors.New("cartridge changed after removal planning")
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for _, table := range []string{"replicas", "media_health", "media_samples"} {
		if _, e = tx.Exec("DELETE FROM "+table+" WHERE media_id=?", p.ID); e != nil {
			return e
		}
	}
	if _, e = tx.Exec("DELETE FROM media WHERE id=?", p.ID); e != nil {
		return e
	}
	return tx.Commit()
}

// ReplicaBudget is the minimum member budget after reserving every missing
// committed archive. Device observations remain unchanged by this calculation.
func ReplicaBudget(s *catalog.Store, id string) (int64, error) {
	rows, e := s.DB.Query(`SELECT m.id,m.total,m.remaining,COALESCE(h.info,''),COALESCE(h.unhealthy,1),m.state,
 (SELECT COUNT(*) FROM replicas WHERE media_id=m.id AND state IN ('writing','failed')) FROM media m LEFT JOIN media_health h ON h.media_id=m.id WHERE m.replica_id=?`, id)
	if e != nil {
		return 0, e
	}
	type sample struct {
		id               string
		total, remaining int64
		info             string
		bad              bool
		state            string
		incomplete       int
	}
	var members []sample
	for rows.Next() {
		var v sample
		if e = rows.Scan(&v.id, &v.total, &v.remaining, &v.info, &v.bad, &v.state, &v.incomplete); e != nil {
			rows.Close()
			return 0, e
		}
		members = append(members, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return 0, e
	}
	if len(members) == 0 {
		return 0, nil
	}
	batches, e := ListBatches(s, id)
	if e != nil {
		return 0, e
	}
	budget := int64(math.MaxInt64)
	for _, v := range members {
		if v.bad || v.incomplete > 0 || (v.state != "open" && v.state != "registered") {
			return 0, nil
		}
		var i media.Info
		if json.Unmarshal([]byte(v.info), &i) != nil {
			return 0, nil
		}
		i.Total, i.Remaining = v.total, v.remaining
		limit, avail, e := i.Budget()
		if e != nil {
			return 0, nil
		}
		remaining := max(int64(0), avail-Reserve(limit))
		prefix, e := MemberPrefix(s, v.id, batches)
		if e != nil {
			return 0, nil
		}
		for _, b := range batches[prefix:] {
			if b.State != "committed" {
				break
			}
			n := Budget(b.Manifest())
			if b.EncodedBytes != nil {
				n = *b.EncodedBytes
			}
			if n <= 0 || n >= remaining {
				remaining = 0
				break
			}
			remaining -= n
		}
		budget = min(budget, remaining)
	}
	return budget, nil
}
