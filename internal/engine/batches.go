package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
)

type BatchMeta struct {
	ID           string `json:"id"`
	ReplicaID    string `json:"replica_id"`
	StreamID     string `json:"stream_id"`
	Seq          int    `json:"seq"`
	Name         string `json:"name"`
	Digest       string `json:"digest"`
	Logical      int64  `json:"logical_bytes"`
	Files        int    `json:"files"`
	BeginPos     uint64 `json:"begin_position"`
	DataPos      uint64 `json:"data_position"`
	CommitPos    uint64 `json:"commit_position"`
	Blocks       uint64 `json:"blocks"`
	EndPos       uint64 `json:"end_position"`
	EncodedBytes *int64 `json:"encoded_bytes"`
	State        string `json:"state"`
}

func (b BatchMeta) Manifest() Manifest {
	id, _ := format.ParseID(b.ID)
	return Manifest{ID: id, ReplicaID: b.ReplicaID, StreamID: b.StreamID, Seq: b.Seq, Name: b.Name, Digest: b.Digest, Logical: b.Logical, Files: b.Files, BeginPos: b.BeginPos, DataPos: b.DataPos, CommitPos: b.CommitPos, Blocks: b.Blocks, EndPos: b.EndPos, EncodedBytes: b.EncodedBytes}
}
func Meta(m Manifest) BatchMeta {
	return BatchMeta{ID: m.ID.String(), ReplicaID: m.ReplicaID, StreamID: m.StreamID, Seq: m.Seq, Name: m.Name, Digest: m.Digest, Logical: m.Logical, Files: m.Files, BeginPos: m.BeginPos, DataPos: m.DataPos, CommitPos: m.CommitPos, Blocks: m.Blocks, EndPos: m.EndPos, EncodedBytes: m.EncodedBytes}
}
func (b BatchMeta) Boundary(commit bool) Boundary {
	v := Boundary{Batch: b.ID, Replica: b.StreamID, Media: b.ReplicaID, Name: b.Name, Logical: b.Logical, Files: b.Files, Digest: b.Digest}
	if commit {
		v.Blocks = b.Blocks
	}
	return v
}
func ListBatches(s *catalog.Store, id string) ([]BatchMeta, error) {
	rows, e := s.DB.Query("SELECT id,replica_id,stream_id,seq,name,digest,logical_bytes,files,begin_pos,data_pos,commit_pos,blocks,end_pos,encoded_bytes,state FROM batches WHERE replica_id=? ORDER BY seq", id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []BatchMeta{}
	end := uint64(2)
	for rows.Next() {
		var b BatchMeta
		if e = rows.Scan(&b.ID, &b.ReplicaID, &b.StreamID, &b.Seq, &b.Name, &b.Digest, &b.Logical, &b.Files, &b.BeginPos, &b.DataPos, &b.CommitPos, &b.Blocks, &b.EndPos, &b.EncodedBytes, &b.State); e != nil {
			return nil, e
		}
		if b.Seq != len(out) || b.BeginPos < end || b.EndPos >= math.MaxInt64 || b.Logical <= 0 || b.Files <= 0 || b.DataPos != b.BeginPos+1 || b.Blocks != uint64(1+(b.Logical-1)/format.PayloadSize) || b.CommitPos != b.DataPos+b.Blocks || b.EndPos != b.CommitPos+2 {
			return nil, fmt.Errorf("archive %s has invalid replica-set layout", b.ID)
		}
		if len(out) > 0 && out[len(out)-1].State != "committed" {
			return nil, errors.New("uncommitted batch precedes another archive")
		}
		if _, e = format.ParseID(b.StreamID); e != nil {
			return nil, e
		}
		out = append(out, b)
		end = b.EndPos
	}
	return out, rows.Err()
}

// LoadArchive resolves the selector used by offline archive browsing. Full IDs,
// unique hexadecimal ID prefixes and exact committed archive names are accepted.
// A name must be unique; IDs take precedence when a selector could mean both.
func LoadArchive(s *catalog.Store, selector string) (Manifest, error) {
	if selector == "" {
		return Manifest{}, errors.New("archive selector is required")
	}
	if id, err := format.ParseID(selector); err == nil {
		var state string
		err = s.DB.QueryRow("SELECT state FROM batches WHERE id=?", id.String()).Scan(&state)
		if err == nil {
			if state != "committed" {
				return Manifest{}, fmt.Errorf("archive %s is not committed", id.String())
			}
			return Load(s, id.String())
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Manifest{}, err
		}
	}
	if isHexPrefix(selector) {
		rows, err := s.DB.Query("SELECT id FROM batches WHERE state='committed' AND lower(id) LIKE ? ORDER BY seq", strings.ToLower(selector)+"%")
		if err != nil {
			return Manifest{}, err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return Manifest{}, err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return Manifest{}, err
		}
		if len(ids) > 1 {
			return Manifest{}, fmt.Errorf("archive ID prefix %q is ambiguous; use a longer prefix", selector)
		}
		if len(ids) == 1 {
			return Load(s, ids[0])
		}
	}
	rows, err := s.DB.Query("SELECT id FROM batches WHERE state='committed' AND name=? ORDER BY seq", selector)
	if err != nil {
		return Manifest{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return Manifest{}, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Manifest{}, err
	}
	switch len(ids) {
	case 0:
		return Manifest{}, fmt.Errorf("no committed archive matches %q", selector)
	case 1:
		return Load(s, ids[0])
	default:
		return Manifest{}, fmt.Errorf("archive name %q is ambiguous; use its ID", selector)
	}
}

func isHexPrefix(s string) bool {
	if len(s) < 6 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// MemberPrefix also checks positions, not just the maximum sequence number.
func MemberPrefix(s *catalog.Store, member string, batches []BatchMeta) (int, error) {
	rows, e := s.DB.Query("SELECT batch_id,begin_pos,data_pos,commit_pos,blocks,end_pos FROM replicas WHERE media_id=? AND state='committed' ORDER BY begin_pos", member)
	if e != nil {
		return 0, e
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id string
		var begin, data, commit, blocks, end uint64
		if e = rows.Scan(&id, &begin, &data, &commit, &blocks, &end); e != nil {
			return 0, e
		}
		if n >= len(batches) {
			return 0, errors.New("member contains an archive outside the set")
		}
		b := batches[n]
		if id != b.ID || begin != b.BeginPos || data != b.DataPos || commit != b.CommitPos || blocks != b.Blocks || end != b.EndPos {
			return 0, errors.New("member does not contain a contiguous matching prefix")
		}
		n++
	}
	return n, rows.Err()
}
func assignBatch(s *catalog.Store, m *Manifest) error {
	if m.ReplicaID == "" {
		return errors.New("batch requires an explicit replica set")
	}
	all, e := ListBatches(s, m.ReplicaID)
	if e != nil {
		return e
	}
	m.Seq = len(all)
	m.BeginPos = 2
	if len(all) > 0 {
		last := all[len(all)-1]
		if last.State != "committed" {
			return errors.New("replica set already has an unfinished batch")
		}
		m.BeginPos = last.EndPos
	}
	m.StreamID = format.NewID().String()
	m.DataPos = m.BeginPos + 1
	m.Blocks = uint64(1 + (m.Logical-1)/format.PayloadSize)
	m.CommitPos = m.DataPos + m.Blocks
	m.EndPos = m.CommitPos + 2
	return nil
}
