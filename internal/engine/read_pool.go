package engine

import (
	"context"
	"errors"
	"time"

	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
)

type readEvent struct {
	b   []byte
	err error
}

// sequentialReader keeps the drive reading ahead while CPU work consumes records.
// Positions are tracked locally; parser lookahead never seeks the physical tape.
type sequentialReader struct {
	media.Device
	ctx      context.Context
	cancel   context.CancelFunc
	free     chan []byte
	ready    chan readEvent
	done     chan struct{}
	current  []byte
	pending  *readEvent
	position uint64
	Reads    uint64
	SinkWait time.Duration
	progress func(uint64)
}

func newSequentialReader(ctx context.Context, d media.Device, start uint64, memory int64) (*sequentialReader, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if memory == 0 {
		memory = 1 << 30
	}
	if memory < 8<<20 || memory > MaxMemory {
		return nil, errors.New("read memory must be 8 MiB–64 GiB")
	}
	count := int((memory - (6 << 20)) / format.BlockSize)
	if e := availableMemory(int64(count) * format.BlockSize); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &sequentialReader{Device: d, ctx: ctx, cancel: cancel, free: make(chan []byte, count), ready: make(chan readEvent, count), done: make(chan struct{}), position: start}
	for i := 0; i < count; i++ {
		r.free <- make([]byte, format.BlockSize)
	}
	go func() {
		defer close(r.done)
		defer close(r.ready)
		for {
			// Hysteresis avoids requesting one record at a time when the
			// parser is persistently slower than the drive.
			if len(r.free) < max(1, cap(r.free)/8) {
				started := time.Now()
				ticker := time.NewTicker(time.Millisecond)
				for len(r.free) < max(1, cap(r.free)/2) {
					select {
					case <-ctx.Done():
						ticker.Stop()
						return
					case <-ticker.C:
					}
				}
				ticker.Stop()
				r.SinkWait = max(r.SinkWait, time.Since(started))
			}
			var b []byte
			select {
			case b = <-r.free:
			default:
				start := time.Now()
				select {
				case b = <-r.free:
				case <-ctx.Done():
					return
				}
				r.SinkWait = max(r.SinkWait, time.Since(start))
			}
			if ctx.Err() != nil {
				return
			}
			var n int
			var e error
			if reader, ok := d.(media.RecordReader); ok {
				n, e = reader.ReadRecordInto(b)
			} else {
				var data []byte
				data, e = d.ReadRecord()
				if len(data) > len(b) {
					e = errors.New("oversize tape record")
				} else {
					n = copy(b, data)
				}
			}
			select {
			case r.ready <- readEvent{b[:n], e}:
			case <-ctx.Done():
				return
			}
			if e != nil && e != media.ErrFilemark {
				return
			}
		}
	}()
	return r, nil
}
func (r *sequentialReader) ReadRecord() ([]byte, error) {
	if r.current != nil {
		r.free <- r.current[:format.BlockSize]
		r.current = nil
	}
	if r.pending != nil {
		v := *r.pending
		r.pending = nil
		r.position++
		return v.b, v.err
	}
	select {
	case v, ok := <-r.ready:
		if !ok {
			return nil, media.ErrEnd
		}
		r.current = v.b
		if v.err == nil || v.err == media.ErrFilemark {
			r.position++
			r.Reads++
			if r.progress != nil && r.Reads%1024 == 0 {
				r.progress(1 + r.Reads)
			}
		}
		return v.b, v.err
	case <-r.ctx.Done():
		return nil, r.ctx.Err()
	}
}
func (r *sequentialReader) unread(b []byte) {
	r.pending = &readEvent{b: append([]byte(nil), b...)}
	r.position--
}
func (r *sequentialReader) Position() (uint64, error) { return r.position, nil }
func (r *sequentialReader) Locate(uint64) error {
	return errors.New("sequential tape scan cannot seek")
}
func (r *sequentialReader) Close() error { r.cancel(); <-r.done; return nil }
