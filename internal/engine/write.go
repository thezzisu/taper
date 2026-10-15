package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/klauspost/compress/zstd"
	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
)

func encoder(enabled bool) (*zstd.Encoder, error) {
	if !enabled {
		return nil, nil
	}
	return zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedFastest))
}
func decoder() (*zstd.Decoder, error) {
	return zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(4<<20), zstd.WithDecoderMaxWindow(format.BlockSize))
}
func boundaryBytes(m Manifest, commit bool) ([]byte, error) {
	b := Meta(m).Boundary(commit)
	raw, e := json.Marshal(b)
	if e != nil {
		return nil, e
	}
	kind := format.Begin
	if commit {
		kind = format.Commit
	}
	return format.AppendFrame(nil, format.Frame{Kind: kind, Batch: m.ID, First: format.NoEntry, Payload: raw}, nil)
}
func supply(ctx context.Context, m Manifest, o WriteOptions) (*pipeline, error) {
	p, e := newPipeline(ctx, o.Memory, m.Logical, func(ctx context.Context, free chan []byte, ready chan chunk) error {
		return Produce(ctx, m, free, ready)
	})
	if e != nil {
		return nil, e
	}
	if e = p.Fill(); e != nil {
		p.Close()
		return nil, e
	}
	return p, nil
}
func packWrite(p *pipeline, d media.Device, m Manifest, o WriteOptions) (int64, error) {
	begin, e := boundaryBytes(m, false)
	if e != nil {
		return 0, e
	}
	commit, e := boundaryBytes(m, true)
	if e != nil {
		return 0, e
	}
	enc, e := encoder(o.Zstd)
	if e != nil {
		return 0, e
	}
	if enc != nil {
		defer enc.Close()
	}
	if e = d.WriteRecord(begin); e != nil {
		return 0, e
	}
	n := int64(len(begin))
	seq := uint64(0)
	out := make([]byte, 0, format.BlockSize+4096)
	for {
		paused, e := p.Refill(d.Flush)
		if e != nil {
			return n, e
		}
		if paused && o.Pause != nil {
			o.Pause()
		}
		c, ok, wait, e := p.Next(o.MaxWait)
		if e != nil {
			return n, e
		}
		if !ok {
			break
		}
		if wait > 0 && o.Underflow != nil {
			o.Underflow(wait)
		}
		out, e = format.AppendFrame(out, format.Frame{Kind: format.Data, Batch: m.ID, Seq: seq, First: c.first, Payload: c.b}, enc)
		if e != nil {
			return n, e
		}
		e = d.WriteRecord(out)
		p.Release(c)
		if e != nil {
			return n, e
		}
		seq++
		n += int64(len(out))
		if o.Progress != nil {
			o.Progress(seq, n)
		}
	}
	if seq != m.Blocks {
		return n, errors.New("produced DATA count differs from plan")
	}
	if e = d.WriteRecord(commit); e != nil {
		return n, e
	}
	n += int64(len(commit))
	if e = d.Filemark(); e != nil {
		return n, e
	}
	return n, nil
}
func WriteArchive(ctx context.Context, s *catalog.Store, d media.Device, m Manifest, key []byte, o WriteOptions) (string, error) {
	if e := ValidateManifest(m); e != nil {
		return "", e
	}
	var state string
	if e := s.DB.QueryRow("SELECT state FROM batches WHERE id=?", m.ID.String()).Scan(&state); e != nil {
		return "", e
	}
	if state != "planned" {
		return "", errors.New("source writing requires an unstarted plan; copy committed streams with tape sync")
	}
	info, e := d.Info()
	if e != nil {
		return "", e
	}
	member, e := MemberForSerial(s, info.Serial)
	if e != nil {
		return "", e
	}
	if e = recordMediaInfo(s, member.ID, info); e != nil {
		return "", e
	}
	if _, e := CheckWriteTarget(s, d, Meta(m)); e != nil {
		return "", e
	}
	p, e := supply(ctx, m, o)
	if e != nil {
		return "", e
	}
	defer p.Close()
	return commitTapeBatch(s, d, Meta(m), key, func(dst media.Device) (int64, error) { return packWrite(p, dst, m, o) })
}

// commitTapeBatch is shared by source writes and image synchronization. Failed
// attempts retain their counters and block further appends, including crashes.
func commitTapeBatch(s *catalog.Store, d media.Device, b BatchMeta, key []byte, write func(media.Device) (int64, error)) (rep string, err error) {
	member, err := CheckWriteTarget(s, d, b)
	if err != nil {
		return
	}
	if _, err = prepareMedia(s, d, key); err != nil {
		return
	}
	if err = d.End(); err != nil {
		return
	}
	pos, err := d.Position()
	if err != nil {
		return
	}
	all, err := ListBatches(s, b.ReplicaID)
	if err != nil {
		return
	}
	previous := uint64(2)
	if b.Seq > 0 {
		previous = all[b.Seq-1].EndPos
	}
	if pos != previous && pos != b.BeginPos {
		return "", fmt.Errorf("tape EOD %d differs from committed prefix end %d", pos, previous)
	}
	rep = format.NewID().String()
	if _, err = s.DB.Exec("INSERT INTO replicas(id,batch_id,media_id,begin_pos,data_pos,state) VALUES(?,?,?,?,?,'writing')", rep, b.ID, member.ID, b.BeginPos, b.DataPos); err != nil {
		return
	}
	counted := &countedWriter{Device: d}
	committed := false
	defer func() {
		if !committed {
			cause := "write interrupted"
			if err != nil {
				cause = err.Error()
			}
			_, e := s.DB.Exec("UPDATE replicas SET state='failed',error=?,confirmed_bytes=?,confirmed_records=? WHERE id=?", cause, counted.bytes, counted.records, rep)
			err = errors.Join(err, e)
		}
		i, e := d.Info()
		if e != nil {
			err = errors.Join(err, e, recordMediaFailure(s, member.ID, e))
			return
		}
		if e = recordMediaInfo(s, member.ID, i); e != nil {
			err = errors.Join(err, e, recordMediaFailure(s, member.ID, e))
		}
	}()
	// Explicit positions describe a replica stream; any inter-batch objects are
	// native filemarks. New plans always append directly at previous.end_pos.
	for p := pos; p < b.BeginPos; p++ {
		if err = d.Filemark(); err != nil {
			return
		}
	}
	if _, err = write(counted); err != nil {
		return
	}
	end, e := d.Position()
	if e != nil {
		return rep, e
	}
	if end != b.EndPos || counted.records != int64(b.Blocks+2) {
		return rep, errors.New("written replica layout differs from plan")
	}
	if b.EncodedBytes != nil && *b.EncodedBytes != counted.bytes {
		return rep, errors.New("written encoded bytes differ from immutable stream")
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return rep, e
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE replicas SET state='committed',commit_pos=?,blocks=?,confirmed_bytes=?,confirmed_records=?,end_pos=? WHERE id=?", b.CommitPos, b.Blocks, counted.bytes, counted.records, b.EndPos, rep); err != nil {
		return
	}
	if _, err = tx.Exec("UPDATE batches SET state='committed',encoded_bytes=? WHERE id=?", counted.bytes, b.ID); err != nil {
		return
	}
	if _, err = tx.Exec("UPDATE media_health SET unhealthy=1 WHERE media_id=?", member.ID); err != nil {
		return
	}
	err = tx.Commit()
	committed = err == nil
	return
}
