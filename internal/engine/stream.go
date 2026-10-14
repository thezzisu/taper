package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"github.com/klauspost/compress/zstd"
	"github.com/thezzisu/taper/internal/format"
	"io"
)

type batchStream struct {
	d       *sequentialReader
	dec     *zstd.Decoder
	batch   format.ID
	seq     uint64
	pending []byte
	ctx     context.Context
	short   bool
}

func (s *batchStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if e := s.ctx.Err(); e != nil {
		return 0, e
	}
	if len(s.pending) == 0 {
		if s.short {
			return 0, errors.New("short DATA record before stream end")
		}
		b, e := s.d.ReadRecord()
		if e != nil {
			return 0, e
		}
		f, e := format.DecodeFrame(b, s.dec)
		if e != nil {
			return 0, e
		}
		if f.Kind != format.Data || f.Batch != s.batch || f.Seq != s.seq || len(f.Payload) == 0 {
			if f.Kind == format.Begin {
				s.d.unread(b)
			}
			return 0, errors.New("invalid sequential data record")
		}
		s.seq++
		s.pending = f.Payload
		s.short = len(f.Payload) < format.PayloadSize
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

func readStreamGroup(r io.Reader) (g format.Group, n int64, err error) {
	var prefix []byte
	for len(prefix) < 10 {
		var b [1]byte
		if _, err = io.ReadFull(r, b[:]); err != nil {
			return
		}
		prefix = append(prefix, b[0])
		if b[0] < 128 {
			break
		}
	}
	l, k := binary.Uvarint(prefix)
	if k <= 0 || l > format.MaxMetadata {
		return g, 0, errors.New("invalid metadata length")
	}
	b := make([]byte, int(l)+len(prefix))
	copy(b, prefix)
	if _, err = io.ReadFull(r, b[len(prefix):]); err != nil {
		return
	}
	return format.ReadGroup(bytes.NewReader(b))
}
