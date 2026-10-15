package engine

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
)

// FillImage reads one approved block window through the normal bounded pool.
// It changes only the image. Caller owns the sole device scheduler and key.
func FillImage(ctx context.Context, d media.Device, img *media.Image, b BatchMeta, start, end uint64, o ReadOptions) error {
	if start >= end || start < b.DataPos || end > b.CommitPos {
		return errors.New("fill window lies outside DATA")
	}
	if start == b.DataPos && !img.HasObject(b.BeginPos) {
		start = b.BeginPos
	}
	if end == b.CommitPos && !img.HasObject(b.EndPos-1) {
		end = b.EndPos
	}
	if e := d.Locate(start); e != nil {
		return e
	}
	r, e := newSequentialReader(ctx, &limitedRecords{source: d, left: end - start}, start, o.Memory)
	if e != nil {
		return e
	}
	defer r.Close()
	r.progress = o.Progress
	dec, e := decoder()
	if e != nil {
		return e
	}
	defer dec.Close()
	for p := start; p < end; p++ {
		raw, e := r.ReadRecord()
		if p == b.EndPos-1 {
			if e != media.ErrFilemark {
				return errors.New("missing archive filemark")
			}
		} else {
			if e != nil {
				return e
			}
			f, e := format.DecodeFrame(raw, dec)
			if e != nil {
				return e
			}
			if f.Batch.String() != b.ID {
				return errors.New("fill returned a different archive")
			}
			if p == b.BeginPos || p == b.CommitPos {
				kind := format.Begin
				if p == b.CommitPos {
					kind = format.Commit
				}
				var boundary Boundary
				if e = json.Unmarshal(f.Payload, &boundary); e != nil {
					return e
				}
				if f.Kind != kind || boundary != b.Boundary(p == b.CommitPos) {
					return errors.New("fill boundary mismatch")
				}
			} else {
				if f.Kind != format.Data || f.Seq != p-b.DataPos {
					return errors.New("fill DATA sequence mismatch")
				}
				expected := int64(format.PayloadSize)
				if p == b.CommitPos-1 {
					expected = b.Logical - int64(b.Blocks-1)*format.PayloadSize
				}
				if int64(len(f.Payload)) != expected {
					return errors.New("fill DATA length mismatch")
				}
			}
		}
		if img.HasObject(p) {
			continue
		}
		if e = img.Locate(p); e != nil {
			return e
		}
		if p == b.EndPos-1 {
			e = img.Filemark()
		} else {
			e = img.WriteRecord(raw)
		}
		if e != nil {
			return e
		}
	}
	r.Close()
	pos, e := d.Position()
	if e != nil {
		return e
	}
	if pos != end {
		return errors.New("fill crossed planned boundary")
	}
	return img.Flush()
}
