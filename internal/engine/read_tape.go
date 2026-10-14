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
	Memory   int64
	Progress func(uint64, int64)
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
		e := s.DB.QueryRow("SELECT key FROM media WHERE id=? AND serial=?", label.ID, info.Serial).Scan(&saved)
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
	var existing string
	e = s.DB.QueryRow("SELECT id FROM media WHERE serial=?", info.Serial).Scan(&existing)
	if e == nil && existing != label.ID {
		return out, errors.New("cartridge was registered with a different identity")
	}
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	_, e = s.DB.Exec("INSERT INTO media(id,label,serial,total,remaining,sampled,state,key_id,key) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET state='open'", label.ID, label.Name, label.Serial, info.Total, info.Remaining, info.Sampled, "open", label.KeyID, key)
	if e != nil {
		return out, e
	}
	seen := map[string]bool{}
	for _, a := range archives {
		m := a.m
		var digest string
		e = s.DB.QueryRow("SELECT digest FROM batches WHERE id=?", m.ID.String()).Scan(&digest)
		if errors.Is(e, sql.ErrNoRows) {
			e = Save(s, m)
			digest = m.Digest
		}
		if e != nil {
			issue(fmt.Errorf("archive %s: %w", m.ID.String(), e))
			continue
		}
		if digest != m.Digest {
			issue(fmt.Errorf("archive %s differs from catalog", m.ID.String()))
			continue
		}
		var result sql.Result
		result, e = s.DB.Exec(`INSERT INTO replicas(id,batch_id,media_id,begin_pos,data_pos,commit_pos,blocks,confirmed_bytes,confirmed_records,end_pos,state,verified)
VALUES(?,?,?,?,?,?,?,?,?,?,'committed',?) ON CONFLICT(id) DO UPDATE SET state='committed',error='',verified=excluded.verified,confirmed_bytes=excluded.confirmed_bytes,confirmed_records=excluded.confirmed_records,end_pos=excluded.end_pos
WHERE replicas.batch_id=excluded.batch_id AND replicas.media_id=excluded.media_id AND replicas.begin_pos=excluded.begin_pos AND replicas.data_pos=excluded.data_pos AND replicas.commit_pos=excluded.commit_pos AND replicas.blocks=excluded.blocks
AND (replicas.confirmed_bytes IS NULL OR replicas.confirmed_bytes=excluded.confirmed_bytes) AND (replicas.confirmed_records IS NULL OR replicas.confirmed_records=excluded.confirmed_records) AND (replicas.end_pos IS NULL OR replicas.end_pos=excluded.end_pos)`, a.begin.Replica, a.begin.Batch, label.ID, a.start, a.data, a.commit, a.end.Blocks, a.bytes, a.end.Blocks+2, a.finish, time.Now().UTC().Format(time.RFC3339))
		if e == nil {
			n, _ := result.RowsAffected()
			if n != 1 {
				e = errors.New("existing replica identity or layout differs")
			}
		}
		if e == nil {
			_, e = s.DB.Exec("UPDATE batches SET state='committed' WHERE id=?", m.ID.String())
		}
		if e == nil {
			out.Imported++
		}
		if e != nil {
			issue(e)
		} else {
			seen[a.begin.Replica] = true
		}
	}
	rows, e := s.DB.Query("SELECT id FROM replicas WHERE media_id=? AND state='committed'", label.ID)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			break
		}
		if !seen[id] {
			issue(fmt.Errorf("catalog replica %s was not found intact on tape", id))
		}
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return out, e
	}
	var registered int
	if e = s.DB.QueryRow("SELECT COUNT(*) FROM media WHERE id=?", label.ID).Scan(&registered); e != nil {
		return out, e
	}
	if registered > 0 {
		if e = recordMediaInfo(s, label.ID, info); e != nil {
			return out, e
		}
	}
	if len(out.Issues) > 0 {
		return out, fmt.Errorf("complete tape scan found %d issues", len(out.Issues))
	}
	out.Complete = true
	return
}
