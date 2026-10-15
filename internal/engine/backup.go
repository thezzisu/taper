package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"time"

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
	Image     *media.Image
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
