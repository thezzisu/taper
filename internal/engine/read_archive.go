package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
)

func readBatch(ctx context.Context, d media.Device, dec *zstd.Decoder, begin Boundary) (m Manifest, end Boundary, commitPos uint64, err error) {
	id, e := format.ParseID(begin.Batch)
	if e != nil {
		return m, end, 0, e
	}
	if begin.Logical <= 0 || begin.Files <= 0 || begin.Blocks != 0 {
		return m, end, 0, errors.New("invalid batch boundary")
	}
	m = Manifest{ID: id, Name: begin.Name, Created: time.Now().UTC().Format(time.RFC3339), Source: "reindexed", Digest: begin.Digest, Logical: begin.Logical, Files: begin.Files}
	dataPos, e := d.Position()
	if e != nil {
		return m, end, 0, e
	}
	stream := &batchStream{d: d, dec: dec, batch: id, ctx: ctx, position: dataPos}
	logical := int64(0)
	count := 0
	for logical < m.Logical {
		g, n, e := readStreamGroup(stream)
		if e != nil {
			return m, end, 0, e
		}
		c := Content{Size: g.Size, Meta: logical, Data: logical + n}
		if g.Size > m.Logical-c.Data {
			return m, end, 0, errors.New("content exceeds batch")
		}
		for _, p := range g.Paths {
			c.Files = append(c.Files, File{Path: p})
		}
		h := sha256.New()
		if _, e = io.CopyN(h, stream, g.Size); e != nil {
			return m, end, 0, e
		}
		c.Hash = hex.EncodeToString(h.Sum(nil))
		m.Contents = append(m.Contents, c)
		logical = c.Data + c.Size
		count += len(g.Paths)
	}
	if len(stream.pending) != 0 || count != m.Files || stream.seq != uint64(1+(m.Logical-1)/format.PayloadSize) {
		return m, end, 0, errors.New("trailing bytes or file-count mismatch")
	}
	commitPos, e = d.Position()
	if e != nil {
		return m, end, 0, e
	}
	b, e := d.ReadRecord()
	if e != nil {
		return m, end, commitPos, e
	}
	f, e := format.DecodeFrame(b, dec)
	if e != nil {
		return m, end, commitPos, e
	}
	if f.Kind == format.Begin {
		if r, ok := d.(*sequentialReader); ok {
			r.unread(b)
		}
	}
	if f.Kind != format.Commit || f.Batch != id {
		return m, end, commitPos, errors.New("missing COMMIT")
	}
	if e = json.Unmarshal(f.Payload, &end); e != nil {
		return m, end, commitPos, e
	}
	expected := begin
	expected.Blocks = stream.seq
	if end != expected || Digest(m.Contents) != m.Digest {
		return m, end, commitPos, errors.New("commit digest mismatch")
	}
	if _, e = d.ReadRecord(); e != media.ErrFilemark {
		return m, end, commitPos, errors.New("missing durable filemark")
	}
	return m, end, commitPos, nil
}
