package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
)

type ReadOptions struct {
	Memory              int64
	Progress            func(uint64, int64)
	HardwareCompression *bool
}
type TapeResult struct {
	Complete        bool          `json:"complete"`
	Start           uint64        `json:"scope_start_position"`
	End             uint64        `json:"scope_end_position"`
	Position        *uint64       `json:"logical_position"`
	Whole           bool          `json:"whole_tape"`
	Bytes           int64         `json:"encoded_bytes_read"`
	EODChecked      bool          `json:"eod_checked"`
	Media           string        `json:"media"`
	Records         uint64        `json:"records_read"`
	Archives        int           `json:"archives_checked"`
	ArchiveIDs      []string      `json:"archive_ids"`
	Imported        int           `json:"replicas_imported"`
	ReachedEOD      bool          `json:"reached_eod"`
	LongestSinkWait time.Duration `json:"longest_sink_wait_ns"`
	Issues          []string      `json:"issues,omitempty"`
}
type scannedArchive struct {
	m                   Manifest
	begin               Boundary
	end                 Boundary
	start, data, commit uint64
	replicaID           string
	bytes               int64
	finish              uint64
}

// ReindexTape reconstructs archives in one BOT-to-EOD pass. SQLite writes follow
// the pass so catalog latency cannot stall the drive. Damaged archives are
// reported while later records are still read.
func ReindexTape(ctx context.Context, s *catalog.Store, d media.Device, provided []byte, o ReadOptions) (out TapeResult, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	if _, err = readPoolCount(o.Memory); err != nil {
		return
	}
	out.Whole = true
	info, err := d.Info()
	if err != nil {
		return
	}
	mam, e := d.ReadIdentity()
	if e != nil {
		return out, e
	}
	candidate, e := decodeLabel(mam, info.Serial)
	if e != nil {
		return out, e
	}
	var registered int
	if e = s.DB.QueryRow("SELECT COUNT(*) FROM replica_sets WHERE id=?", candidate.ID).Scan(&registered); e != nil {
		return out, e
	}
	if registered == 0 && o.HardwareCompression == nil {
		return out, errors.New("reindex of an unknown replica set requires an explicit --hardware-compression preference")
	}

	if err = d.SetKey(nil); err != nil {
		return
	}
	if err = d.Locate(0); err != nil {
		return
	}
	b, err := d.ReadRecord()
	if err != nil {
		return
	}
	out.Records = 1
	f, err := format.DecodeFrame(b, nil)
	if err != nil {
		return
	}
	if f.Kind != format.Media {
		return out, errors.New("not a taper tape")
	}
	var label Label
	if err = json.Unmarshal(f.Payload, &label); err != nil {
		return
	}
	if label.Version != 1 || label.ID != f.Batch.String() || label.Serial != info.Serial {
		return out, errors.New("invalid media identity")
	}
	out.Media = label.ID
	key := provided
	if len(key) == 0 && label.KeyID != "" {
		var saved []byte
		e := s.DB.QueryRow("SELECT rs.key FROM replica_sets rs JOIN media m ON m.replica_id=rs.id WHERE rs.id=? AND m.serial=?", label.ID, info.Serial).Scan(&saved)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return out, e
		}
		key = saved
		defer clear(saved)
	}
	if label.KeyID != "" && media.KeyID(key) != label.KeyID {
		return out, errors.New("matching tape key required")
	}
	if label.KeyID == "" && len(key) > 0 {
		return out, errors.New("tape is not encrypted")
	}
	if err = d.SetKey(key); err != nil {
		return
	}
	r, err := newSequentialReader(ctx, d, 1, o.Memory)
	if err != nil {
		return
	}
	if o.Progress != nil {
		r.progress = func(n uint64, bytes int64) { o.Progress(1+n, int64(len(b))+bytes) }
	}
	defer func() {
		r.Close()
		out.Records = 1 + r.Reads
		out.Bytes = int64(len(b)) + r.Bytes
		out.End = r.position
		out.LongestSinkWait = r.SinkWait
	}()
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(4<<20), zstd.WithDecoderMaxWindow(format.BlockSize))
	if err != nil {
		return
	}
	defer dec.Close()
	var archives []scannedArchive
	unclaimed := false
	issue := func(e error) { out.Issues = append(out.Issues, e.Error()) }
	if b, e := r.ReadRecord(); e != media.ErrFilemark {
		issue(errors.New("missing label filemark"))
		if e == nil {
			r.unread(b)
		}
	}
	for {
		if err = ctx.Err(); err != nil {
			return
		}
		pos := r.position
		byteStart := r.Bytes
		b, e := r.ReadRecord()
		if e == io.EOF {
			out.ReachedEOD, out.EODChecked = true, true
			break
		}
		if e == media.ErrFilemark {
			continue
		}
		if e != nil {
			return out, e
		}
		frame, e := format.DecodeFrame(b, dec)
		if e != nil {
			issue(fmt.Errorf("record %d: %w", pos, e))
			continue
		}
		if frame.Kind != format.Begin {
			if !unclaimed {
				issue(fmt.Errorf("unclaimed records starting at %d", pos))
				unclaimed = true
			}
			continue
		}
		unclaimed = false
		var begin Boundary
		if e = json.Unmarshal(frame.Payload, &begin); e != nil {
			issue(e)
			continue
		}
		if begin.Media != label.ID || begin.Batch != frame.Batch.String() {
			issue(fmt.Errorf("invalid archive identity at %d", pos))
			continue
		}
		if _, e = format.ParseID(begin.Replica); e != nil {
			issue(e)
			continue
		}
		data := r.position
		m, end, commit, e := readBatch(ctx, r, dec, begin)
		if e != nil {
			issue(fmt.Errorf("archive %s: %w", begin.Batch, e))
			continue
		}
		if e = ValidateManifest(m); e != nil {
			issue(e)
			continue
		}
		archives = append(archives, scannedArchive{m: m, begin: begin, end: end, start: pos, data: data, commit: commit, bytes: r.Bytes - byteStart, finish: r.position})
		out.Archives++
		out.ArchiveIDs = append(out.ArchiveIDs, m.ID.String())
		if o.Progress != nil {
			o.Progress(1+r.Reads, int64(len(b))+r.Bytes)
		}
	}
	r.Close()
	actual, e := d.Position()
	if e != nil {
		return out, e
	}
	if actual != 1+r.Reads {
		return out, fmt.Errorf("tape advanced to %d after %d captured objects", actual, 1+r.Reads)
	}
	out.Position = &actual
	// The pre-read sample may be hours old. Persist a separately timed sample
	// of the device at the completed read boundary.
	info, e = d.Info()
	if e != nil {
		return out, e
	}
	if info.Serial != label.Serial {
		return out, errors.New("cartridge changed during scan")
	}
	member, e := MemberForSerial(s, info.Serial)
	if errors.Is(e, sql.ErrNoRows) {
		member = MediaMember{ID: format.NewID().String(), Replica: label.ID, Serial: label.Serial, Label: label.Name, State: "open"}
	} else if e != nil {
		return out, e
	} else if member.Replica != label.ID {
		return out, errors.New("cartridge belongs to another replica set")
	}
	var setCount int
	if e = s.DB.QueryRow("SELECT COUNT(*) FROM replica_sets WHERE id=?", label.ID).Scan(&setCount); e != nil {
		return out, e
	}
	if setCount == 0 {
		if o.HardwareCompression == nil {
			return out, errors.New("unknown replica write preference; provide --hardware-compression explicitly")
		}
		if _, e = s.DB.Exec("INSERT INTO replica_sets(id,name,key_id,key,hardware_compression) VALUES(?,?,?,?,?)", label.ID, label.ID, label.KeyID, key, *o.HardwareCompression); e != nil {
			return out, e
		}
	}
	var compression bool
	if e = s.DB.QueryRow("SELECT hardware_compression FROM replica_sets WHERE id=?", label.ID).Scan(&compression); e != nil {
		return out, e
	}
	if _, e = s.DB.Exec("INSERT INTO media(id,replica_id,label,note,serial,total,remaining,sampled,state,key_id,key,hardware_compression) VALUES(?,?,?,'',?,?,?,?,'open',?,?,?) ON CONFLICT(id) DO UPDATE SET state='open'", member.ID, label.ID, label.Name, label.Serial, info.Total, info.Remaining, info.Sampled, label.KeyID, key, compression); e != nil {
		return out, e
	}
	seen := map[string]bool{}
	for idx, a := range archives {
		m := a.m
		m.ReplicaID = label.ID
		m.StreamID = a.begin.Replica
		m.Seq = idx
		m.BeginPos, m.DataPos, m.CommitPos, m.Blocks, m.EndPos = a.start, a.data, a.commit, a.end.Blocks, a.finish
		m.EncodedBytes = &a.bytes
		var existing, digest string
		e = s.DB.QueryRow("SELECT replica_id,digest FROM batches WHERE id=?", m.ID.String()).Scan(&existing, &digest)
		if errors.Is(e, sql.ErrNoRows) {
			e = Save(s, m)
		} else if e == nil {
			old, e := Load(s, m.ID.String())
			if e != nil {
				return out, e
			}
			if old.ReplicaID != m.ReplicaID || old.Digest != m.Digest || old.StreamID != m.StreamID || old.BeginPos != m.BeginPos || old.EndPos != m.EndPos {
				issue(fmt.Errorf("archive %s differs from catalog", m.ID))
				continue
			}
		}
		if e != nil {
			issue(e)
			continue
		}
		var rid string
		e = s.DB.QueryRow("SELECT id FROM replicas WHERE batch_id=? AND media_id=? AND begin_pos=?", m.ID.String(), member.ID, m.BeginPos).Scan(&rid)
		if errors.Is(e, sql.ErrNoRows) {
			rid = format.NewID().String()
		} else if e != nil {
			return out, e
		}
		_, e = s.DB.Exec(`INSERT INTO replicas(id,batch_id,media_id,begin_pos,data_pos,commit_pos,blocks,confirmed_bytes,confirmed_records,end_pos,state,verified) VALUES(?,?,?,?,?,?,?,?,?,?,'committed',?)
 ON CONFLICT(id) DO UPDATE SET state='committed',error='',data_pos=excluded.data_pos,commit_pos=excluded.commit_pos,blocks=excluded.blocks,confirmed_bytes=excluded.confirmed_bytes,confirmed_records=excluded.confirmed_records,end_pos=excluded.end_pos,verified=excluded.verified`, rid, m.ID.String(), member.ID, m.BeginPos, m.DataPos, m.CommitPos, m.Blocks, a.bytes, m.Blocks+2, m.EndPos, time.Now().UTC().Format(time.RFC3339))
		if e != nil {
			issue(e)
			continue
		}
		if _, e = s.DB.Exec("UPDATE batches SET state='committed',encoded_bytes=? WHERE id=?", a.bytes, m.ID.String()); e != nil {
			return out, e
		}
		seen[m.ID.String()] = true
		out.Imported++
	}
	rows, e := s.DB.Query("SELECT batch_id FROM replicas WHERE media_id=? AND state='committed'", member.ID)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			break
		}
		if !seen[id] {
			issue(fmt.Errorf("catalog archive %s was not found intact on tape", id))
		}
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return out, e
	}
	if e = recordMediaInfo(s, member.ID, info); e != nil {
		return out, e
	}

	if len(out.Issues) > 0 {
		return out, fmt.Errorf("complete tape scan found %d issues", len(out.Issues))
	}
	out.Complete = true
	return
}
