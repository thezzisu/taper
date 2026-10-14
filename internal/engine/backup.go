package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
)

type Label struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Serial  string `json:"serial"`
	KeyID   string `json:"key_id"`
	Version int    `json:"version"`
}
type Boundary struct {
	Batch   string `json:"batch"`
	Replica string `json:"replica"`
	Media   string `json:"media"`
	Name    string `json:"name"`
	Logical int64  `json:"logical"`
	Files   int    `json:"files"`
	Blocks  uint64 `json:"blocks"`
	Digest  string `json:"digest"`
}

func control(d media.Device, kind byte, id format.ID, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	r, e := format.AppendFrame(nil, format.Frame{Kind: kind, Batch: id, First: format.NoEntry, Payload: b}, nil)
	if e != nil {
		return e
	}
	return d.WriteRecord(r)
}
func ReadLabel(d media.Device) (l Label, err error) {
	// Appends identify the cartridge through MAM. Never rewind and read tape
	// records as a fallback; full sequential readers handle the on-tape label.
	b, err := d.ReadIdentity()
	if err != nil {
		return l, fmt.Errorf("read MAM identity: %w", err)
	}
	if len(b) <= 7 || string(b[:7]) != "taper1:" {
		return l, errors.New("append requires a valid MAM identity")
	}
	var fields []string
	if json.Unmarshal(b[7:], &fields) != nil || len(fields) != 3 {
		return l, errors.New("invalid MAM identity")
	}
	if _, err = format.ParseID(fields[0]); err != nil {
		return l, err
	}
	info, err := d.Info()
	if err != nil {
		return l, err
	}
	return Label{ID: fields[0], Name: fields[1], Serial: info.Serial, KeyID: fields[2], Version: 1}, nil
}
func UnlockMedia(s *catalog.Store, d media.Device, provided []byte) (l Label, err error) {
	l, err = ReadLabel(d)
	if err != nil {
		return
	}
	info, e := d.Info()
	if e != nil {
		return l, e
	}
	if info.Serial != l.Serial {
		return l, errors.New("media serial mismatch")
	}
	var key []byte
	var keyID, serial string
	err = s.DB.QueryRow("SELECT key,key_id,serial FROM media WHERE id=?", l.ID).Scan(&key, &keyID, &serial)
	if err != nil {
		return
	}
	defer func() { clear(key) }()
	if serial != info.Serial {
		return l, errors.New("catalog cartridge serial mismatch")
	}
	if keyID != l.KeyID {
		return l, errors.New("catalog/media key mismatch")
	}
	if len(provided) > 0 {
		key = append([]byte(nil), provided...)
	}
	if keyID != "" && media.KeyID(key) != keyID {
		return l, errors.New("matching tape key required")
	}
	if keyID == "" && len(key) != 0 {
		return l, errors.New("unencrypted medium")
	}
	err = d.SetKey(key)
	return
}
func Budget(m Manifest) int64 {
	n := uint64(1 + (m.Logical-1)/format.PayloadSize)
	boundary := Boundary{Batch: m.ID.String(), Replica: (format.ID{}).String(), Media: (format.ID{}).String(), Name: m.Name, Logical: m.Logical, Files: m.Files, Digest: m.Digest}
	begin, _ := json.Marshal(boundary)
	boundary.Blocks = n
	commit, _ := json.Marshal(boundary)
	if m.Logical <= 0 || len(begin) > format.PayloadSize || len(commit) > format.PayloadSize {
		return math.MaxInt64
	}
	overhead := int64(n)*format.HeaderSize + int64(len(begin)+len(commit)) + 2*format.HeaderSize
	if m.Logical > math.MaxInt64-overhead {
		return math.MaxInt64
	}
	return m.Logical + overhead
}
func Reserve(total int64) int64 {
	r := total / 100
	if r < 2<<30 {
		r = 2 << 30
	}
	return r
}

type WriteOptions struct {
	Memory    int64
	Zstd      bool
	Progress  func(uint64, int64)
	MaxWait   time.Duration
	Underflow func(time.Duration)
	Pause     func()
}
type chunk struct {
	b     []byte
	first uint32
}

// Count only write commands the device confirmed. A failing command may have
// transferred more; its sense/residual and the observed position are separate.
type countedWriter struct {
	media.Device
	bytes, records int64
}

func (d *countedWriter) WriteRecord(b []byte) error {
	if err := d.Device.WriteRecord(b); err != nil {
		return err
	}
	d.bytes += int64(len(b))
	d.records++
	return nil
}

type packer struct {
	ctx   context.Context
	free  chan []byte
	ready chan chunk
	b     []byte
	first uint32
}

func (p *packer) acquire() error {
	if p.b != nil {
		return nil
	}
	select {
	case p.b = <-p.free:
		p.b = p.b[:0]
		p.first = format.NoEntry
		return nil
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
}
func (p *packer) flush() error {
	if len(p.b) == 0 {
		return nil
	}
	select {
	case p.ready <- chunk{p.b, p.first}:
		p.b = nil
		return nil
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
}
func (p *packer) write(b []byte) error {
	for len(b) > 0 {
		if e := p.acquire(); e != nil {
			return e
		}
		n := min(len(b), format.PayloadSize-len(p.b))
		p.b = append(p.b, b[:n]...)
		b = b[n:]
		if len(p.b) == format.PayloadSize {
			if e := p.flush(); e != nil {
				return e
			}
		}
	}
	return nil
}
func (p *packer) Write(b []byte) (int, error) {
	if e := p.write(b); e != nil {
		return 0, e
	}
	return len(b), nil
}
func Produce(ctx context.Context, m Manifest, free chan []byte, ready chan chunk) error {
	p := packer{ctx: ctx, free: free, ready: ready}
	defer close(ready)
	if err := CheckSource(m); err != nil {
		return err
	}
	root, e := os.OpenRoot(m.Source)
	if e != nil {
		return e
	}
	defer root.Close()
	buffer := make([]byte, 4<<20)
	for _, c := range m.Contents {
		// Complete alias metadata checks before streaming the shared extent.
		for _, alias := range c.Files[1:] {
			st, e := root.Stat(alias.Source)
			if e != nil || !st.Mode().IsRegular() || st.Size() != c.Size || st.ModTime().UnixNano() != alias.MTime {
				return fmt.Errorf("duplicate source changed: %s", alias.Path)
			}
		}
		if e = p.acquire(); e != nil {
			return e
		}
		if p.first == format.NoEntry {
			p.first = uint32(len(p.b))
		}
		paths := []string{}
		for _, f := range c.Files {
			paths = append(paths, f.Path)
		}
		meta, e := format.EncodeGroup(format.Group{Size: c.Size, Paths: paths})
		if e != nil {
			return e
		}
		if e = p.write(meta); e != nil {
			return e
		}
		f, e := root.Open(c.Files[0].Source)
		if e != nil {
			return e
		}
		st, e := f.Stat()
		if e != nil {
			f.Close()
			return e
		}
		if !st.Mode().IsRegular() || st.Size() != c.Size {
			f.Close()
			return fmt.Errorf("source changed: %s", c.Files[0].Path)
		}
		h := sha256.New()
		n, e := io.CopyBuffer(io.MultiWriter(&p, h), io.LimitReader(f, c.Size+1), buffer)
		after, se := f.Stat()
		f.Close()
		if e != nil {
			return e
		}
		if se != nil {
			return se
		}
		if n != c.Size || after.Size() != st.Size() || !after.ModTime().Equal(st.ModTime()) || hex.EncodeToString(h.Sum(nil)) != c.Hash {
			return fmt.Errorf("source changed: %s", c.Files[0].Path)
		}
	}
	if err := CheckSource(m); err != nil {
		return err
	}
	return p.flush()
}
func WriteArchive(ctx context.Context, s *catalog.Store, d media.Device, m Manifest, key []byte, o WriteOptions) (rep string, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	if err = ValidateManifest(m); err != nil {
		return
	}
	if _, err = poolCount(o.Memory, m.Logical); err != nil {
		return
	}
	pipeline, err := newPipeline(ctx, o.Memory, m.Logical, func(ctx context.Context, free chan []byte, ready chan chunk) error {
		return Produce(ctx, m, free, ready)
	})
	if err != nil {
		return "", err
	}
	defer pipeline.Close()
	if err = pipeline.Fill(); err != nil {
		return "", err
	}

	l, err := prepareMedia(s, d, key)
	if err != nil {
		return
	}
	var state string
	if err = s.DB.QueryRow("SELECT state FROM media WHERE id=?", l.ID).Scan(&state); err != nil {
		return
	}
	if state != "open" {
		return "", fmt.Errorf("tape is not writable: state %s", state)
	}
	info, err := d.Info()
	if err != nil {
		return
	}
	if err = recordMediaInfo(s, l.ID, info); err != nil {
		return
	}
	if err = checkMediaInfo(info); err != nil {
		return
	}
	limit, available, err := info.Budget()
	if err != nil {
		return "", err
	}
	if Budget(m) > available-Reserve(limit) {
		return "", fmt.Errorf("archive requires %d encoded bytes plus reserve; manually split it or choose another tape", Budget(m))
	}
	if err = d.End(); err != nil {
		return
	}
	start, err := d.Position()
	if err != nil {
		return
	}
	rep = format.NewID().String()
	boundary := Boundary{Batch: m.ID.String(), Replica: rep, Media: l.ID, Name: m.Name, Logical: m.Logical, Files: m.Files, Digest: m.Digest}
	if _, err = s.DB.Exec("INSERT INTO replicas(id,batch_id,media_id,begin_pos,state) VALUES(?,?,?,?,'writing')", rep, m.ID.String(), l.ID, start); err != nil {
		return
	}
	counted := &countedWriter{Device: d}
	d = counted
	var seq uint64
	defer func() {
		if err != nil {
			_, saveErr := s.DB.Exec("UPDATE replicas SET state='failed',error=?,blocks=?,confirmed_bytes=?,confirmed_records=? WHERE id=?", err.Error(), seq, counted.bytes, counted.records, rep)
			err = errors.Join(err, saveErr)
		}
		i, e := d.Info()
		if e != nil {
			err = errors.Join(err, fmt.Errorf("device state after write: %w", e), recordMediaFailure(s, l.ID, e))
			return
		}
		if e = recordMediaInfo(s, l.ID, i); e != nil {
			err = errors.Join(err, e, recordMediaFailure(s, l.ID, e))
		}
		if i.Position != nil {
			_, e = s.DB.Exec("UPDATE replicas SET end_pos=? WHERE id=?", *i.Position, rep)
			err = errors.Join(err, e)
		}
	}()
	if err = control(d, format.Begin, m.ID, boundary); err != nil {
		return
	}
	dataPos, err := d.Position()
	if err != nil {
		return
	}
	if dataPos != start+1 {
		return rep, errors.New("tape position after BEGIN differs from layout")
	}
	if _, err = s.DB.Exec("UPDATE replicas SET data_pos=? WHERE id=?", dataPos, rep); err != nil {
		return
	}
	var enc *zstd.Encoder
	if o.Zstd {
		enc, err = zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedFastest))
		if err != nil {
			return
		}
		defer enc.Close()
	}
	out := make([]byte, 0, format.BlockSize+4096)
	for {
		paused, e := pipeline.Refill(d.Flush)
		if e != nil {
			return rep, e
		}
		if paused && o.Pause != nil {
			o.Pause()
		}
		c, ok, wait, e := pipeline.Next(o.MaxWait)
		if e != nil {
			return rep, e
		}
		if !ok {
			break
		}
		if wait > 5*time.Millisecond && o.Underflow != nil {
			o.Underflow(wait)
		}
		out, err = format.AppendFrame(out, format.Frame{Kind: format.Data, Batch: m.ID, Seq: seq, First: c.first, Payload: c.b}, enc)
		if err != nil {
			return
		}
		if err = d.WriteRecord(out); err != nil {
			return
		}
		pipeline.Release(c)
		seq++
		if o.Progress != nil {
			o.Progress(seq, counted.bytes)
		}
	}
	boundary.Blocks = seq
	commitPos, err := d.Position()
	if err != nil {
		return
	}
	if commitPos != dataPos+seq {
		return rep, errors.New("tape logical position differs from recorded layout")
	}
	if err = control(d, format.Commit, m.ID, boundary); err != nil {
		return
	}
	if err = d.Filemark(); err != nil {
		return
	}
	endPos, err := d.Position()
	if err != nil {
		return rep, err
	}
	if endPos != commitPos+2 {
		return rep, errors.New("tape position after durable filemark differs from layout")
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return rep, e
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE replicas SET state='committed',commit_pos=?,blocks=?,confirmed_bytes=?,confirmed_records=?,end_pos=? WHERE id=?", commitPos, seq, counted.bytes, counted.records, endPos, rep); err != nil {
		return
	}
	if _, err = tx.Exec("UPDATE batches SET state='committed' WHERE id=?", m.ID.String()); err != nil {
		return
	}
	// The pre-write sample cannot describe a committed append. Keep it blocked
	// atomically with the commit, including a crash before the final sample.
	if _, err = tx.Exec("UPDATE media_health SET unhealthy=1 WHERE media_id=?", l.ID); err != nil {
		return
	}
	err = tx.Commit()
	return
}
