package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
)

// SyncPlan contains only stable scope and identities, never fresh device samples.
type SyncPlan struct {
	Replica    ReplicaSet   `json:"replica"`
	From       string       `json:"from"`
	To         string       `json:"to"`
	FromTape   *MediaMember `json:"from_tape,omitempty"`
	ToTape     *MediaMember `json:"to_tape,omitempty"`
	Start      uint64       `json:"start_position"`
	End        uint64       `json:"end_position"`
	Objects    uint64       `json:"objects"`
	ImageBytes int64        `json:"image_write_bound_bytes,omitempty"`
	Archives   []BatchMeta  `json:"archives"`
	selector   string
}
type SyncResult struct {
	Archives        []string      `json:"archives"`
	Start           uint64        `json:"start_position"`
	End             uint64        `json:"end_position"`
	Bytes           int64         `json:"encoded_bytes"`
	Objects         uint64        `json:"objects"`
	PrunedEnd       uint64        `json:"pruned_end"`
	LongestSinkWait time.Duration `json:"longest_sink_wait_ns"`
}

func syncMember(s *catalog.Store, d media.Device, r ReplicaSet, target bool) (MediaMember, media.Info, error) {
	i, err := d.Info()
	if err != nil {
		return MediaMember{}, i, err
	}
	m, err := MemberForSerial(s, i.Serial)
	if err != nil {
		return m, i, err
	}
	if m.Replica != r.ID {
		return m, i, errors.New("cartridge belongs to another replica set")
	}
	if m.State == "open" {
		raw, e := d.ReadIdentity()
		if e != nil {
			return m, i, e
		}
		l, e := decodeLabel(raw, i.Serial)
		if e != nil {
			return m, i, e
		}
		if l.ID != r.ID || l.KeyID != r.KeyID {
			return m, i, errors.New("MAM identity differs from catalog")
		}
	} else if !target || m.State != "registered" {
		return m, i, errors.New("cartridge is not ready for synchronization")
	}
	if target {
		var incomplete int
		if err = s.DB.QueryRow("SELECT COUNT(*) FROM replicas WHERE media_id=? AND state IN ('writing','failed')", m.ID).Scan(&incomplete); err != nil {
			return m, i, err
		}
		if incomplete != 0 {
			return m, i, errors.New("target tape has an incomplete write")
		}
		if err = checkMediaInfo(i); err != nil {
			return m, i, err
		}
	}
	return m, i, nil
}

// PlanSync uses only catalog, MAM and image bitmap facts. A nil destination
// represents a new image, which the CLI creates only after scope approval.
func PlanSync(s *catalog.Store, name string, source, target media.Device, spec string) (p SyncPlan, err error) {
	p.Archives = []BatchMeta{}
	p.selector = spec
	if source == nil {
		return p, errors.New("source endpoint is required")
	}
	if source == target {
		return p, errors.New("source and destination are the same endpoint")
	}
	p.Replica, err = ResolveReplicaSet(s, name)
	if err != nil {
		return
	}
	fromImage, _ := source.(*media.Image)
	toImage, _ := target.(*media.Image)
	p.From, p.To = "image", "image"
	for _, img := range []*media.Image{fromImage, toImage} {
		if img != nil && (img.ReplicaID != p.Replica.ID || img.KeyID != p.Replica.KeyID) {
			return p, errors.New("image replica/key identity differs from catalog")
		}
	}
	all, err := ListBatches(s, p.Replica.ID)
	if err != nil {
		return
	}
	for len(all) > 0 && all[len(all)-1].State != "committed" {
		all = all[:len(all)-1]
	}
	available := all
	if fromImage == nil {
		p.From = "device"
		m, _, e := syncMember(s, source, p.Replica, false)
		if e != nil {
			return p, e
		}
		p.FromTape = &m
		n, e := MemberPrefix(s, m.ID, all)
		if e != nil {
			return p, e
		}
		available = all[:n]
	}
	prefix := 0
	var targetInfo media.Info
	if target != nil && toImage == nil {
		p.To = "device"
		m, i, e := syncMember(s, target, p.Replica, true)
		if e != nil {
			return p, e
		}
		p.ToTape, targetInfo = &m, i
		if p.FromTape != nil && m.Serial == p.FromTape.Serial {
			return p, errors.New("source and destination contain the same cartridge")
		}
		prefix, err = MemberPrefix(s, m.ID, all)
		if err != nil {
			return
		}
	}
	selected, err := ParseArchiveRange(spec, available)
	if err != nil {
		return
	}
	if p.To == "device" {
		if spec == "" {
			selected = available[min(prefix, len(available)):]
		} else if len(selected) > 0 && selected[0].Seq != prefix {
			return p, fmt.Errorf("archive range must start at destination's next batch %d", prefix)
		}
		limit, avail, e := targetInfo.Budget()
		if e != nil {
			return p, e
		}
		budget := max(int64(0), avail-Reserve(limit))
		for _, b := range selected {
			n := Budget(b.Manifest())
			if b.EncodedBytes != nil {
				n = *b.EncodedBytes
			}
			if n <= 0 || n > budget {
				return p, errors.New("selected archives exceed target tape budget")
			}
			budget -= n
		}
	}
	// A sparse image source can offer complete, disjoint archives. Explicit
	// ranges and physical destinations require every selected archive intact.
	if fromImage != nil {
		ready := make([]BatchMeta, 0, len(selected))
		for _, b := range selected {
			if !fromImage.HasRange(b.BeginPos, b.EndPos) {
				if spec != "" || p.To == "device" {
					return p, fmt.Errorf("source image lacks complete archive %s", b.ID)
				}
				continue
			}
			ready = append(ready, b)
		}
		selected = ready
	}
	if p.To == "image" {
		if p.From == "device" {
			// Keep one forward tape pass, including any cached middle archives.
			first, last := -1, -1
			for i, b := range selected {
				if toImage == nil || !toImage.HasRange(b.BeginPos, b.EndPos) {
					if first < 0 {
						first = i
					}
					last = i
				}
			}
			if first < 0 {
				selected = nil
			} else {
				selected = selected[first : last+1]
			}
		} else {
			missing := make([]BatchMeta, 0, len(selected))
			for _, b := range selected {
				if toImage == nil || !toImage.HasRange(b.BeginPos, b.EndPos) {
					missing = append(missing, b)
				}
			}
			selected = missing
		}
		for _, b := range selected {
			if b.EndPos > media.ImageMaxObjects {
				return p, errors.New("archive exceeds image bitmap capacity")
			}
			for pos := b.BeginPos; pos < b.EndPos; pos++ {
				if toImage == nil || !toImage.HasObject(pos) {
					p.ImageBytes += format.BlockSize
				}
			}
		}
		if len(selected) > 0 {
			p.ImageBytes += media.ImageHeaderSize
		}
	}
	p.Archives = append(p.Archives, selected...)
	if len(selected) > 0 {
		p.Start, p.End = selected[0].BeginPos, selected[len(selected)-1].EndPos
		if p.From == "device" {
			p.Objects = p.End - p.Start
		} else {
			for i, b := range selected {
				p.Objects += b.EndPos - imageReadStart(fromImage, selected, i)
			}
		}
	}
	return
}

// Preserve any native filemarks between consecutive cached archives without
// making uncached prefixes a dependency of an image-to-image transfer.
func imageReadStart(img *media.Image, archives []BatchMeta, i int) uint64 {
	b := archives[i]
	if i > 0 && archives[i-1].Seq+1 == b.Seq && img.HasRange(archives[i-1].EndPos, b.BeginPos) {
		return archives[i-1].EndPos
	}
	return b.BeginPos
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

// ReplicaSync uses the same validating stream for all endpoint combinations.
// Only a physical destination acquires the catalog writer lock and updates it.
func ReplicaSync(ctx context.Context, s *catalog.Store, source, target media.Device, p SyncPlan, key []byte, prune bool, o ReadOptions) (out SyncResult, err error) {
	out.Archives = []string{}
	out.Start, out.End = p.Start, p.Start
	if err = ctx.Err(); err != nil {
		return
	}
	fromImage, _ := source.(*media.Image)
	toImage, _ := target.(*media.Image)
	if target == nil {
		return out, errors.New("destination endpoint is required")
	}
	if prune && (fromImage == nil || toImage != nil) {
		return out, errors.New("--prune requires --from-image and --to-device")
	}
	if toImage == nil {
		release, e := lockStage(s)
		if e != nil {
			return out, e
		}
		defer release()
	}
	current, e := PlanSync(s, p.Replica.ID, source, target, p.selector)
	if e != nil {
		return out, e
	}
	if !reflect.DeepEqual(current, p) {
		return out, errors.New("synchronization scope changed after preview")
	}
	if len(p.Archives) == 0 {
		if prune {
			out.PrunedEnd, err = pruneImage(s, fromImage, p.Replica.ID)
		}
		return
	}
	if _, err = readPoolCount(o.Memory); err != nil {
		return
	}
	k, e := ReplicaKey(s, p.Replica.ID, key)
	if e != nil {
		return out, e
	}
	defer clear(k)
	if fromImage != nil {
		err = fromImage.SetKey(k)
	} else {
		_, err = UnlockMedia(s, source, k)
	}
	if err != nil {
		return
	}
	if toImage != nil {
		if err = toImage.SetKey(k); err != nil {
			return
		}
		if err = toImage.CheckSpace(p.ImageBytes); err != nil {
			return
		}
	} else {
		// Finish target initialization/positioning before starting the source.
		if _, err = CheckWriteTarget(s, target, p.Archives[0]); err != nil {
			return
		}
		if _, err = prepareMedia(s, target, k); err != nil {
			return
		}
		if err = target.End(); err != nil {
			return
		}
		pos, e := target.Position()
		if e != nil {
			return out, e
		}
		all, e := ListBatches(s, p.Replica.ID)
		if e != nil {
			return out, e
		}
		previous := uint64(2)
		if p.Archives[0].Seq > 0 {
			previous = all[p.Archives[0].Seq-1].EndPos
		}
		if pos != previous && pos != p.Start {
			return out, fmt.Errorf("target EOD %d differs from committed prefix %d", pos, previous)
		}
	}
	var r *sequentialReader
	defer func() {
		if r != nil {
			r.Close()
			out.LongestSinkWait = max(out.LongestSinkWait, r.SinkWait)
		}
	}()
	openReader := func(start, end uint64) error {
		if e := source.Locate(start); e != nil {
			return e
		}
		pos, e := source.Position()
		if e != nil {
			return e
		}
		if pos != start {
			return errors.New("source start position mismatch")
		}
		r, e = newSequentialReader(ctx, &limitedRecords{source: source, left: end - start}, start, o.Memory)
		if e != nil {
			return e
		}
		baseObjects, baseBytes := out.Objects, out.Bytes
		r.progress = func(n uint64, bytes int64) {
			if o.Progress != nil {
				o.Progress(baseObjects+n, baseBytes+bytes)
			}
		}
		return nil
	}
	if fromImage == nil {
		if err = openReader(p.Start, p.End); err != nil {
			return
		}
	}
	for i, b := range p.Archives {
		if err = ctx.Err(); err != nil {
			return
		}
		if fromImage != nil {
			if err = openReader(imageReadStart(fromImage, p.Archives, i), b.EndPos); err != nil {
				return
			}
		}
		if toImage == nil {
			if err = r.Fill(); err != nil {
				return
			}
		}
		writePos := r.position
		sink := func(raw []byte, event error) error {
			pos := writePos
			writePos++
			if toImage.HasObject(pos) {
				return nil
			}
			if e := toImage.Locate(pos); e != nil {
				return e
			}
			if event == media.ErrFilemark {
				return toImage.Filemark()
			}
			return toImage.WriteRecord(raw)
		}
		for r.position < b.BeginPos {
			if _, e = r.ReadRecord(); e != media.ErrFilemark {
				return out, errors.New("inter-archive object is not a filemark")
			}
			if toImage != nil {
				if err = sink(nil, media.ErrFilemark); err != nil {
					return
				}
			}
		}
		var n int64
		if toImage != nil {
			n, err = readExpected(ctx, r, b, sink)
		} else {
			member, e := CheckWriteTarget(s, target, b)
			if e != nil {
				return out, e
			}
			_, err = commitPositionedTapeBatch(s, target, b, member, func(dst media.Device) (int64, error) {
				var e error
				n, e = readExpected(ctx, r, b, func(raw []byte, event error) error {
					if event == media.ErrFilemark {
						return dst.Filemark()
					}
					return dst.WriteRecord(raw)
				})
				return n, e
			})
		}
		if err != nil {
			return
		}
		if toImage != nil {
			if err = toImage.Flush(); err != nil {
				return
			}
		}
		out.Archives = append(out.Archives, b.ID)
		out.End = b.EndPos
		out.Bytes += n
		if fromImage != nil {
			r.Close()
			out.LongestSinkWait = max(out.LongestSinkWait, r.SinkWait)
			out.Objects += r.Reads
			r = nil
		} else {
			out.Objects = b.EndPos - p.Start
		}
	}
	if r != nil {
		r.Close()
		out.LongestSinkWait = max(out.LongestSinkWait, r.SinkWait)
		pos, e := source.Position()
		if e != nil {
			return out, e
		}
		if pos != p.End {
			return out, errors.New("source end position differs from planned range")
		}
	}
	if prune {
		out.PrunedEnd, err = pruneImage(s, fromImage, p.Replica.ID)
	}
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
