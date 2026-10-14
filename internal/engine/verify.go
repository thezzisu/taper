package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
)

type VerifyArchive struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Digest  string  `json:"digest"`
	Logical int64   `json:"logical_bytes"`
	Files   int     `json:"files"`
	Replica Replica `json:"replica"`
}

type VerifyPlan struct {
	Media    string          `json:"media"`
	Label    string          `json:"label"`
	Serial   string          `json:"serial"`
	KeyID    string          `json:"key_id"`
	Whole    bool            `json:"whole_tape"`
	Start    uint64          `json:"start_position"`
	End      uint64          `json:"end_position"` // exclusive
	CheckEOD bool            `json:"check_eod"`
	Archives []VerifyArchive `json:"archives"`
}

// PlanVerification uses catalog and MAM queries only. It never reads records,
// locates, changes encryption settings, or writes catalog observations.
func PlanVerification(s *catalog.Store, d media.Device, ids []string) (p VerifyPlan, err error) {
	if len(ids) > 2 {
		return p, errors.New("provide no IDs, one archive ID, or FIRST_ID LAST_ID")
	}
	selected := make([]string, len(ids))
	for i, id := range ids {
		parsed, e := format.ParseID(id)
		if e != nil {
			return p, fmt.Errorf("invalid archive ID %q: %w", id, e)
		}
		selected[i] = parsed.String()
	}
	l, err := ReadLabel(d)
	if err != nil {
		return
	}
	p.Media, p.Serial, p.KeyID = l.ID, l.Serial, l.KeyID
	var serial, keyID string
	if err = s.DB.QueryRow("SELECT label,serial,key_id FROM media WHERE id=?", l.ID).Scan(&p.Label, &serial, &keyID); err != nil {
		return p, fmt.Errorf("loaded tape is not registered in this catalog: %w", err)
	}
	if serial != l.Serial || keyID != l.KeyID {
		return p, errors.New("loaded tape identity differs from catalog")
	}
	rows, err := s.DB.Query(`SELECT b.id,b.name,b.digest,b.logical_bytes,b.files,r.id,r.begin_pos,r.data_pos,r.commit_pos,r.blocks,r.confirmed_bytes,r.confirmed_records,r.end_pos,r.verified IS NOT NULL
FROM replicas r JOIN batches b ON b.id=r.batch_id WHERE r.media_id=? AND r.state='committed' AND b.state='committed' ORDER BY r.begin_pos`, p.Media)
	if err != nil {
		return
	}
	var all []VerifyArchive
	seen := map[string]bool{}
	end := uint64(2) // public label and its filemark
	for rows.Next() {
		var a VerifyArchive
		r := &a.Replica
		if err = rows.Scan(&a.ID, &a.Name, &a.Digest, &a.Logical, &a.Files, &r.ID, &r.Begin, &r.Data, &r.Commit, &r.Blocks, &r.ConfirmedBytes, &r.ConfirmedRecords, &r.End, &r.Verified); err != nil {
			break
		}
		r.Media, r.Label = p.Media, p.Label
		if seen[a.ID] {
			err = fmt.Errorf("archive %s has multiple committed replicas on this tape", a.ID)
			break
		}
		seen[a.ID] = true
		if a.Logical <= 0 || a.Files <= 0 || r.Begin < end || r.End == nil || *r.End >= math.MaxInt64 || r.Data != r.Begin+1 || r.Blocks != uint64(1+(a.Logical-1)/format.PayloadSize) || r.Commit != r.Data+r.Blocks || *r.End != r.Commit+2 {
			err = fmt.Errorf("archive %s has missing, overlapping or inconsistent object positions", a.ID)
			break
		}
		if r.ConfirmedRecords != nil && *r.ConfirmedRecords != int64(r.Blocks)+2 || r.ConfirmedBytes != nil && *r.ConfirmedBytes <= 0 {
			err = fmt.Errorf("archive %s has inconsistent record counters", a.ID)
			break
		}
		end = *r.End
		all = append(all, a)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return
	}
	p.Whole, p.CheckEOD = len(selected) == 0, true
	p.Archives, p.End = all, end
	if !p.Whole {
		indexes := make([]int, len(selected))
		for i, id := range selected {
			indexes[i] = -1
			for j, a := range all {
				if a.ID == id {
					indexes[i] = j
					break
				}
			}
			if indexes[i] < 0 {
				return p, fmt.Errorf("archive %s has no committed replica on tape %s", id, p.Label)
			}
		}
		first, last := indexes[0], indexes[len(indexes)-1]
		if first > last {
			return p, errors.New("FIRST_ID follows LAST_ID on this tape")
		}
		p.Archives = all[first : last+1]
		p.Start, p.End = all[first].Replica.Begin, *all[last].Replica.End
		p.CheckEOD = last == len(all)-1
	}
	upper := p.End
	if p.CheckEOD {
		upper++
	}
	var incomplete int
	if err = s.DB.QueryRow(`SELECT COUNT(*) FROM replicas WHERE media_id=? AND state IN ('writing','failed') AND begin_pos<? AND (end_pos IS NULL OR end_pos>=?)`, p.Media, upper, p.Start).Scan(&incomplete); err != nil {
		return
	}
	if incomplete > 0 {
		return p, errors.New("verification range intersects an unresolved write; resolve its tail first")
	}
	return
}

// boundedRead limits the producer, not just the parser: read-ahead must never
// cross the approved range even when a boundary or EOD is missing.
type boundedRead struct {
	media.Device
	remaining uint64
	records   uint64
	bytes     int64
}

func (d *boundedRead) ReadRecordInto(b []byte) (int, error) {
	if d.remaining == 0 {
		return 0, errors.New("verification read limit reached")
	}
	d.remaining--
	n, err := d.Device.ReadRecordInto(b)
	if err == nil || err == media.ErrFilemark {
		d.records++
		d.bytes += int64(n)
	}
	return n, err
}

// VerifyTape checks exactly the approved plan in one forward pass. All catalog
// updates follow successful content, boundary and (when selected) EOD checks.
func VerifyTape(ctx context.Context, s *catalog.Store, d media.Device, plan VerifyPlan, provided []byte, o ReadOptions) (out TapeResult, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	release, err := lockStage(s)
	if err != nil {
		return
	}
	defer release()
	if _, err = readPoolCount(o.Memory); err != nil {
		return
	}
	var ids []string
	if !plan.Whole {
		if len(plan.Archives) == 0 {
			return out, errors.New("empty verification range")
		}
		ids = []string{plan.Archives[0].ID, plan.Archives[len(plan.Archives)-1].ID}
	}
	current, err := PlanVerification(s, d, ids)
	if err != nil {
		return
	}
	if !reflect.DeepEqual(current, plan) {
		return out, errors.New("tape or verification scope changed after planning")
	}
	label, err := UnlockMedia(s, d, provided)
	if err != nil {
		return
	}
	if label.ID != plan.Media || label.Serial != plan.Serial || label.KeyID != plan.KeyID {
		return out, errors.New("tape identity changed before verification")
	}
	out.Media, out.Start, out.End, out.Whole = plan.Media, plan.Start, plan.End, plan.Whole
	out.ArchiveIDs = make([]string, 0, len(plan.Archives))
	for _, archive := range plan.Archives {
		out.ArchiveIDs = append(out.ArchiveIDs, archive.ID)
	}
	if err = d.Locate(plan.Start); err != nil {
		return
	}
	pos, err := d.Position()
	if err != nil || pos != plan.Start {
		return out, errors.Join(err, errors.New("verification start position mismatch"))
	}
	limited := &boundedRead{Device: d, remaining: plan.End - plan.Start}
	if plan.CheckEOD {
		limited.remaining++
	}
	r, err := newSequentialReader(ctx, limited, plan.Start, o.Memory)
	if err != nil {
		return
	}
	r.progress = o.Progress
	defer func() {
		r.Close()
		out.Records, out.Bytes, out.LongestSinkWait = limited.records, limited.bytes, r.SinkWait
	}()
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(4<<20), zstd.WithDecoderMaxWindow(format.BlockSize))
	if err != nil {
		return
	}
	defer dec.Close()
	if plan.Whole {
		b, e := r.ReadRecord()
		if e != nil {
			return out, e
		}
		f, e := format.DecodeFrame(b, dec)
		if e != nil {
			return out, e
		}
		var diskLabel Label
		if e = json.Unmarshal(f.Payload, &diskLabel); e != nil {
			return out, e
		}
		if f.Kind != format.Media || f.Batch.String() != plan.Media || diskLabel.ID != plan.Media || diskLabel.Serial != plan.Serial || diskLabel.KeyID != plan.KeyID || diskLabel.Version != 1 {
			return out, errors.New("tape label differs from planned identity")
		}
		if _, e = r.ReadRecord(); e != media.ErrFilemark {
			return out, errors.New("missing label filemark")
		}
	}
	var checked []scannedArchive
	for _, expected := range plan.Archives {
		for r.position < expected.Replica.Begin {
			pos := r.position
			if _, e := r.ReadRecord(); e != media.ErrFilemark {
				return out, fmt.Errorf("expected inter-archive filemark at object %d: %v", pos, e)
			}
		}
		if r.position != expected.Replica.Begin {
			return out, fmt.Errorf("archive %s start position mismatch", expected.ID)
		}
		byteStart := r.Bytes
		b, e := r.ReadRecord()
		if e != nil {
			return out, e
		}
		f, e := format.DecodeFrame(b, dec)
		if e != nil {
			return out, e
		}
		var begin Boundary
		if e = json.Unmarshal(f.Payload, &begin); e != nil {
			return out, e
		}
		want := Boundary{Batch: expected.ID, Replica: expected.Replica.ID, Media: plan.Media, Name: expected.Name, Logical: expected.Logical, Files: expected.Files, Digest: expected.Digest}
		if f.Kind != format.Begin || f.Batch.String() != expected.ID || begin != want {
			return out, fmt.Errorf("archive %s BEGIN differs from catalog", expected.ID)
		}
		m, end, commit, e := readBatch(ctx, r, dec, begin)
		if e != nil {
			return out, fmt.Errorf("archive %s: %w", expected.ID, e)
		}
		if e = ValidateManifest(m); e != nil {
			return out, e
		}
		n := r.Bytes - byteStart
		rep := expected.Replica
		if commit != rep.Commit || end.Blocks != rep.Blocks || r.position != *rep.End || rep.ConfirmedBytes != nil && n != *rep.ConfirmedBytes {
			return out, fmt.Errorf("archive %s recorded layout or byte count differs", expected.ID)
		}
		checked = append(checked, scannedArchive{begin: begin, end: end, start: rep.Begin, data: rep.Data, commit: commit, bytes: n, finish: r.position})
		out.Archives++
	}
	if r.position != plan.End {
		return out, errors.New("verification end position mismatch")
	}
	if plan.CheckEOD {
		out.EODChecked = true
		if _, e := r.ReadRecord(); e != io.EOF {
			return out, fmt.Errorf("expected EOD at object %d: %v", plan.End, e)
		}
		out.ReachedEOD = true
	}
	r.Close()
	pos, err = d.Position()
	if err == nil {
		out.Position = &pos
	}
	if err != nil || pos != plan.End || limited.records != plan.End-plan.Start {
		return out, errors.Join(err, errors.New("physical position differs from verified range"))
	}
	info, err := d.Info()
	if err != nil {
		return
	}
	if info.Serial != plan.Serial {
		return out, errors.New("tape identity changed during verification")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	for _, a := range checked {
		result, e := tx.Exec(`UPDATE replicas SET verified=?,confirmed_bytes=?,confirmed_records=?,end_pos=? WHERE id=? AND batch_id=? AND media_id=? AND begin_pos=? AND data_pos=? AND commit_pos=? AND blocks=? AND state='committed'
AND (confirmed_bytes IS NULL OR confirmed_bytes=?) AND (confirmed_records IS NULL OR confirmed_records=?) AND (end_pos IS NULL OR end_pos=?)`, time.Now().UTC().Format(time.RFC3339), a.bytes, a.end.Blocks+2, a.finish, a.begin.Replica, a.begin.Batch, plan.Media, a.start, a.data, a.commit, a.end.Blocks, a.bytes, a.end.Blocks+2, a.finish)
		if e != nil {
			return out, e
		}
		n, e := result.RowsAffected()
		if e != nil || n != 1 {
			return out, errors.Join(e, errors.New("catalog replica changed during verification"))
		}
	}
	if err = tx.Commit(); err != nil {
		return
	}
	err = recordMediaInfo(s, plan.Media, info)
	out.Complete = err == nil
	return
}
