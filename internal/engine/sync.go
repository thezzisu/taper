package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
)

type SyncPlan struct {
	Replica  ReplicaSet  `json:"replica"`
	Tape     MediaMember `json:"tape"`
	From     bool        `json:"from_image"`
	Start    uint64      `json:"start_position"`
	End      uint64      `json:"end_position"`
	Archives []BatchMeta `json:"archives"`
}
type SyncResult struct {
	Archives  []string `json:"archives"`
	Start     uint64   `json:"start_position"`
	End       uint64   `json:"end_position"`
	Bytes     int64    `json:"encoded_bytes"`
	Objects   uint64   `json:"objects"`
	PrunedEnd uint64   `json:"pruned_end"`
}

func PlanSync(s *catalog.Store, d media.Device, img *media.Image, spec string, from bool) (p SyncPlan, err error) {
	p.From = from
	p.Archives = []BatchMeta{}
	i, err := d.Info()
	if err != nil {
		return
	}
	p.Tape, err = MemberForSerial(s, i.Serial)
	if err != nil {
		return
	}
	p.Replica, err = ResolveReplicaSet(s, p.Tape.Replica)
	if err != nil {
		return
	}
	if img != nil && (img.ReplicaID != p.Replica.ID || img.KeyID != p.Replica.KeyID) {
		return p, errors.New("image and tape replica/key identities differ")
	}
	if from && img == nil {
		return p, errors.New("source image is required")
	}
	all, err := ListBatches(s, p.Replica.ID)
	if err != nil {
		return
	}
	prefix, err := MemberPrefix(s, p.Tape.ID, all)
	if err != nil {
		return
	}
	if from {
		var incomplete int
		if err = s.DB.QueryRow("SELECT COUNT(*) FROM replicas WHERE media_id=? AND state IN ('writing','failed')", p.Tape.ID).Scan(&incomplete); err != nil {
			return
		}
		if incomplete > 0 {
			return p, errors.New("target tape has an incomplete write")
		}
	}
	if p.Tape.State == "open" {
		l, e := ReadLabel(d)
		if e != nil {
			return p, e
		}
		if l.ID != p.Replica.ID || l.KeyID != p.Replica.KeyID {
			return p, errors.New("MAM identity differs from catalog")
		}
	} else if !from || p.Tape.State != "registered" {
		return p, errors.New("cartridge is not ready for this synchronization")
	}
	if !from {
		all = all[:prefix]
	} else {
		for len(all) > 0 && all[len(all)-1].State != "committed" {
			all = all[:len(all)-1]
		}
	}
	selected, err := ParseArchiveRange(spec, all)
	if err != nil {
		return
	}
	if from {
		if spec == "" {
			selected = all[prefix:]
		} else if len(selected) > 0 && selected[0].Seq != prefix {
			return p, fmt.Errorf("archive range must start at next unsynchronized batch %d", prefix)
		}
		for _, b := range selected {
			if !img.HasRange(b.BeginPos, b.EndPos) {
				return p, fmt.Errorf("source image lacks complete archive %s", b.ID)
			}
		}
		limit, avail, e := i.Budget()
		if e != nil {
			return p, e
		}
		budget := max(int64(0), avail-Reserve(limit))
		for _, b := range selected {
			n := Budget(b.Manifest())
			if b.EncodedBytes != nil {
				n = *b.EncodedBytes
			}
			if n > budget {
				return p, errors.New("selected archives exceed target tape budget")
			}
			budget -= n
		}
	} else {
		// One forward pass from first missing archive to last missing archive. Cached
		// intervening archives are consumed in that same pass, never read again later.
		first, last := -1, -1
		for n, b := range selected {
			if img == nil || !img.HasRange(b.BeginPos, b.EndPos) {
				if first < 0 {
					first = n
				}
				last = n
			}
		}
		if first < 0 {
			selected = nil
		} else {
			selected = selected[first : last+1]
		}
	}
	p.Archives = append(p.Archives, selected...)
	if len(selected) > 0 {
		p.Start = selected[0].BeginPos
		p.End = selected[len(selected)-1].EndPos
	}
	return
}

type limitedRecords struct {
	source interface{ ReadRecordInto([]byte) (int, error) }
	left   uint64
}

func (r *limitedRecords) ReadRecordInto(b []byte) (int, error) {
	if r.left == 0 {
		return 0, errors.New("planned read boundary reached")
	}
	r.left--
	return r.source.ReadRecordInto(b)
}
func readExpected(ctx context.Context, r *sequentialReader, b BatchMeta, sink func([]byte, error) error) (int64, error) {
	if r.position != b.BeginPos {
		return 0, errors.New("archive start position differs")
	}
	start := r.Bytes
	dec, e := decoder()
	if e != nil {
		return 0, e
	}
	defer dec.Close()
	raw, e := r.ReadRecord()
	if e != nil {
		return 0, e
	}
	frame, e := format.DecodeFrame(raw, dec)
	if e != nil {
		return 0, e
	}
	var begin Boundary
	if e = json.Unmarshal(frame.Payload, &begin); e != nil {
		return 0, e
	}
	if frame.Kind != format.Begin || frame.Batch.String() != b.ID || begin != b.Boundary(false) {
		return 0, errors.New("BEGIN differs from replica stream")
	}
	var observed int64
	r.sink = func(raw []byte, event error) error {
		observed += int64(len(raw))
		if len(raw) >= format.HeaderSize && raw[10] == format.Commit && b.EncodedBytes != nil && observed != *b.EncodedBytes {
			return errors.New("encoded stream size differs before COMMIT")
		}
		if sink != nil {
			return sink(raw, event)
		}
		return nil
	}
	defer func() { r.sink = nil }()
	if e = r.accept(raw, nil); e != nil {
		return 0, e
	}
	m, end, commit, e := readBatch(ctx, r, dec, begin)
	if e != nil {
		return 0, e
	}
	if e = ValidateManifest(m); e != nil {
		return 0, e
	}
	n := r.Bytes - start
	if end != b.Boundary(true) || commit != b.CommitPos || r.position != b.EndPos || b.EncodedBytes != nil && n != *b.EncodedBytes {
		return 0, errors.New("archive layout or encoded length differs")
	}
	return n, nil
}
func imageReader(ctx context.Context, img *media.Image, b BatchMeta, memory int64) (*sequentialReader, error) {
	if !img.HasRange(b.BeginPos, b.EndPos) {
		return nil, errors.New("image archive is incomplete")
	}
	if e := img.Locate(b.BeginPos); e != nil {
		return nil, e
	}
	return newSequentialReader(ctx, &limitedRecords{source: img, left: b.EndPos - b.BeginPos}, b.BeginPos, memory)
}
func validateImageBatch(ctx context.Context, img *media.Image, b BatchMeta, memory int64) (int64, error) {
	r, e := imageReader(ctx, img, b, memory)
	if e != nil {
		return 0, e
	}
	defer r.Close()
	return readExpected(ctx, r, b, nil)
}
func writeImageArchive(ctx context.Context, s *catalog.Store, d media.Device, img *media.Image, b BatchMeta, key []byte, o WriteOptions) (string, error) {
	if _, e := CheckWriteTarget(s, d, b); e != nil {
		return "", e
	}
	r, e := imageReader(ctx, img, b, o.Memory)
	if e != nil {
		return "", e
	}
	defer r.Close()
	// Prefill image reads before moving the tape. Producer is bounded at filemark.
	if e = r.Fill(); e != nil {
		return "", e
	}
	r.progress = o.Progress
	return commitTapeBatch(s, d, b, key, func(dst media.Device) (int64, error) {
		return readExpected(ctx, r, b, func(raw []byte, event error) error {
			if event == media.ErrFilemark {
				return dst.Filemark()
			}
			return dst.WriteRecord(raw)
		})
	})
}
func TapeSync(ctx context.Context, s *catalog.Store, d media.Device, img *media.Image, p SyncPlan, key []byte, prune bool, o ReadOptions) (out SyncResult, err error) {
	out.Archives = []string{}
	out.Start, out.End = p.Start, p.Start
	if prune && !p.From {
		return out, errors.New("--prune requires --from")
	}
	if err = ctx.Err(); err != nil {
		return
	}
	var release func()
	if p.From {
		release, err = lockStage(s)
		if err != nil {
			return
		}
		defer release()
	}
	spec := ""
	if len(p.Archives) > 0 {
		spec = p.Archives[0].ID + "-" + p.Archives[len(p.Archives)-1].ID
	}
	current, e := PlanSync(s, d, img, spec, p.From)
	if e != nil {
		return out, e
	}
	if !reflect.DeepEqual(current, p) {
		return out, errors.New("synchronization scope changed after preview")
	}
	if len(p.Archives) == 0 {
		if prune {
			out.PrunedEnd, err = pruneImage(s, img, p.Replica.ID)
		}
		return
	}
	k, e := ReplicaKey(s, p.Replica.ID, key)
	if e != nil {
		return out, e
	}
	defer clear(k)
	if err = img.SetKey(k); err != nil {
		return
	}
	if p.From {
		for _, b := range p.Archives {
			if err = ctx.Err(); err != nil {
				return
			}
			_, err = writeImageArchive(ctx, s, d, img, b, k, WriteOptions{Memory: o.Memory, Progress: func(n uint64, bytes int64) {
				if o.Progress != nil {
					o.Progress(out.Objects+n, out.Bytes+bytes)
				}
			}})
			if err != nil {
				return
			}
			out.Archives = append(out.Archives, b.ID)
			out.End = b.EndPos
			out.Objects += b.EndPos - b.BeginPos
			var n int64
			if err = s.DB.QueryRow("SELECT confirmed_bytes FROM replicas WHERE batch_id=? AND media_id=? AND state='committed'", b.ID, p.Tape.ID).Scan(&n); err != nil {
				return
			}
			out.Bytes += n
		}
		if prune {
			out.PrunedEnd, err = pruneImage(s, img, p.Replica.ID)
		}
		return
	}
	if _, err = UnlockMedia(s, d, k); err != nil {
		return
	}
	if err = d.Locate(p.Start); err != nil {
		return
	}
	pos, e := d.Position()
	if e != nil || pos != p.Start {
		return out, errors.Join(e, errors.New("read start position mismatch"))
	}
	r, e := newSequentialReader(ctx, &limitedRecords{source: d, left: p.End - p.Start}, p.Start, o.Memory)
	if e != nil {
		return out, e
	}
	r.progress = o.Progress
	defer r.Close()
	writePos := p.Start
	sink := func(raw []byte, event error) error {
		pos := writePos
		writePos++
		if img.HasObject(pos) {
			return nil
		}
		if e := img.Locate(pos); e != nil {
			return e
		}
		if event == media.ErrFilemark {
			return img.Filemark()
		}
		return img.WriteRecord(raw)
	}
	for _, b := range p.Archives {
		for r.position < b.BeginPos {
			if _, e = r.ReadRecord(); e != media.ErrFilemark {
				return out, errors.New("inter-archive object is not a filemark")
			}
			if e = sink(nil, media.ErrFilemark); e != nil {
				return out, e
			}
		}
		n, e := readExpected(ctx, r, b, sink)
		if e != nil {
			return out, e
		}
		out.Archives = append(out.Archives, b.ID)
		out.End = b.EndPos
		out.Bytes += n
	}
	r.Close()
	out.Objects = p.End - p.Start
	pos, e = d.Position()
	if e != nil || pos != p.End {
		return out, errors.Join(e, errors.New("read end position mismatch"))
	}
	err = img.Flush()
	return
}
func pruneImage(s *catalog.Store, img *media.Image, id string) (uint64, error) {
	all, e := ListBatches(s, id)
	if e != nil {
		return 0, e
	}
	rows, e := s.DB.Query("SELECT id FROM media WHERE replica_id=?", id)
	if e != nil {
		return 0, e
	}
	var ids []string
	for rows.Next() {
		var member string
		if e = rows.Scan(&member); e != nil {
			rows.Close()
			return 0, e
		}
		ids = append(ids, member)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return 0, e
	}
	if len(ids) == 0 {
		return 0, nil
	}
	count := len(all)
	for _, member := range ids {
		n, e := MemberPrefix(s, member, all)
		if e != nil {
			return 0, e
		}
		count = min(count, n)
	}
	if count == 0 {
		return 0, nil
	}
	end := all[count-1].EndPos
	return end, img.PunchHole(2, end)
}
