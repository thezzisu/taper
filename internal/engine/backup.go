package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
func LabelMedia(s *catalog.Store, d media.Device, name string, key []byte, saveKey bool) (l Label, err error) {
	if len(name) == 0 || len(name) > 64 {
		return l, errors.New("media label must be 1–64 UTF-8 bytes")
	}
	info, err := d.Info()
	if err != nil {
		return
	}
	if err = checkMediaInfo(info); err != nil {
		return
	}
	if err = d.Locate(0); err != nil {
		return
	}
	_, err = d.ReadRecord()
	if err != io.EOF {
		return l, errors.New("initialization requires empty media; existing data will not be overwritten")
	}
	if err = d.SetKey(nil); err != nil {
		return
	}
	if err = d.Locate(0); err != nil {
		return
	}
	l = Label{ID: format.NewID().String(), Name: name, Serial: info.Serial, Version: 1}
	if len(key) > 0 {
		l.KeyID = media.KeyID(key)
	}
	id, _ := format.ParseID(l.ID)
	if err = control(d, format.Media, id, l); err != nil {
		return
	}
	if err = d.Filemark(); err != nil {
		return
	}
	if err = d.SetKey(key); err != nil {
		return
	}
	info, err = d.Info()
	if err != nil {
		return
	}
	var stored []byte
	if saveKey {
		stored = key
	}
	_, err = s.DB.Exec("INSERT INTO media VALUES(?,?,?,?,?,?,?,?,?)", l.ID, l.Name, l.Serial, info.Total, info.Remaining, time.Now().UTC().Format(time.RFC3339), "open", l.KeyID, stored)
	if err == nil {
		if err = recordMediaInfo(s, l.ID, info); err != nil {
			return l, err
		}
		if identity, ok := d.(media.Identity); ok {
			public, _ := json.Marshal([]string{l.ID, l.Name, l.KeyID})
			err = identity.WriteIdentity(append([]byte("taper1:"), public...))
		}
	}
	return
}
func ReadLabel(d media.Device) (l Label, err error) {
	if identity, ok := d.(media.Identity); ok {
		if b, e := identity.ReadIdentity(); e == nil && len(b) > 7 && string(b[:7]) == "taper1:" {
			var fields []string
			if json.Unmarshal(b[7:], &fields) == nil && len(fields) == 3 {
				if _, e = format.ParseID(fields[0]); e != nil {
					return l, e
				}
				info, e := d.Info()
				if e != nil {
					return l, e
				}
				if e = d.Locate(1); e != nil {
					return l, e
				}
				return Label{ID: fields[0], Name: fields[1], Serial: info.Serial, KeyID: fields[2], Version: 1}, nil
			}
		}
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
	f, err := format.DecodeFrame(b, nil)
	if err != nil {
		return
	}
	if f.Kind != format.Media {
		return l, errors.New("not taper media")
	}
	err = json.Unmarshal(f.Payload, &l)
	if err == nil && (l.Version != 1 || l.ID != f.Batch.String()) {
		err = errors.New("invalid media label")
	}
	return
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
	var keyID, state string
	err = s.DB.QueryRow("SELECT key,key_id,state FROM media WHERE id=?", l.ID).Scan(&key, &keyID, &state)
	if err != nil {
		return
	}
	defer func() { clear(key) }()
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
	n := (m.Logical + format.PayloadSize - 1) / format.PayloadSize
	return m.Logical + n*format.HeaderSize + 2*format.BlockSize + 1024
}
func Reserve(total int64) int64 {
	r := total / 100
	if r < 2<<30 {
		r = 2 << 30
	}
	return r
}

type WriteOptions struct {
	Memory      int64
	Zstd        bool
	AllowFrozen bool
	Progress    func(uint64)
	ProbeMBps   float64
	MaxWait     time.Duration
	Underflow   func(time.Duration)
}
type chunk struct {
	b     []byte
	first uint32
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
func Backup(ctx context.Context, s *catalog.Store, d media.Device, m Manifest, key []byte, o WriteOptions) (rep string, err error) {
	if err = ValidateManifest(m); err != nil {
		return
	}
	if !o.AllowFrozen {
		if err = ReadOnly(m.Source); err != nil {
			return
		}
	}
	if _, err = poolCount(o.Memory, m.Logical); err != nil {
		return
	}
	if o.ProbeMBps > 0 {
		probe, e := ProbeSupply(ctx, m, o.Memory, o.ProbeMBps)
		if e != nil {
			return "", fmt.Errorf("supply probe failed: %w", e)
		}
		if probe.Underflows > 0 {
			return "", fmt.Errorf("supply probe found %d stalls at %.1f MB/s; increase the buffer or use a faster source", probe.Underflows, o.ProbeMBps)
		}
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

	l, err := UnlockMedia(s, d, key)
	if err != nil {
		return
	}
	var state string
	if err = s.DB.QueryRow("SELECT state FROM media WHERE id=?", l.ID).Scan(&state); err != nil {
		return
	}
	if state != "open" {
		return "", errors.New("media is sealed or unavailable")
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
	if Budget(m) > info.Remaining-Reserve(info.Total) {
		return "", fmt.Errorf("batch requires %d native bytes plus reserve; manually split it or choose another tape", Budget(m))
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
	if _, err = s.DB.Exec("INSERT INTO replicas(id,batch_id,media_id,begin_pos,data_pos,state) VALUES(?,?,?,?,?,'writing')", rep, m.ID.String(), l.ID, start, start+1); err != nil {
		return
	}
	defer func() {
		if err != nil {
			_, _ = s.DB.Exec("UPDATE replicas SET state='failed',error=? WHERE id=?", err.Error(), rep)
		}
		if i, e := d.Info(); e == nil {
			_ = recordMediaInfo(s, l.ID, i)
			_, _ = s.DB.Exec("UPDATE media SET remaining=?,sampled=? WHERE id=?", i.Remaining, time.Now().UTC().Format(time.RFC3339), l.ID)
		}
	}()
	if err = control(d, format.Begin, m.ID, boundary); err != nil {
		return
	}
	dataPos, err := d.Position()
	if err != nil {
		return
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
	var seq uint64
	for {
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
			o.Progress(seq)
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
	_, err = s.DB.Exec("UPDATE replicas SET state='committed',commit_pos=?,blocks=? WHERE id=?", commitPos, seq, rep)
	if err == nil {
		_, err = s.DB.Exec("UPDATE batches SET state='committed' WHERE id=?", m.ID.String())
	}
	return
}
