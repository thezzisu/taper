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
	ctx      context.Context
	cancel   context.CancelFunc
	free     chan []byte
	ready    chan readEvent
	done     chan struct{}
	current  []byte
	pending  *readEvent
	position uint64
	Reads    uint64
	Bytes    int64
	SinkWait time.Duration
	progress func(uint64, int64)
	sink     func([]byte, error) error
}

func readPoolCount(memory int64) (int, error) {
	if memory == 0 {
		memory = 1 << 30
	}
	if memory < 8<<20 || memory > MaxMemory {
		return 0, errors.New("read memory must be 8 MiB–64 GiB")
	}
	count := int((memory - (6 << 20)) / format.BlockSize)
	if e := availableMemory(int64(count) * format.BlockSize); e != nil {
		return 0, e
	}
	return count, nil
}

func newSequentialReader(ctx context.Context, d interface{ ReadRecordInto([]byte) (int, error) }, start uint64, memory int64) (*sequentialReader, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	count, e := readPoolCount(memory)
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &sequentialReader{ctx: ctx, cancel: cancel, free: make(chan []byte, count), ready: make(chan readEvent, count), done: make(chan struct{}), position: start}
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
			n, e := d.ReadRecordInto(b)
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
		r.Bytes += int64(len(v.b))
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
			r.Bytes += int64(len(v.b))
			if r.progress != nil && r.Reads%1024 == 0 {
				r.progress(r.Reads, r.Bytes)
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
	r.Bytes -= int64(len(b))
}
func (r *sequentialReader) Close() { r.cancel(); <-r.done }

func (r *sequentialReader) accept(b []byte, e error) error {
	if r.sink != nil {
		return r.sink(b, e)
	}
	return nil
}

// Fill starts bounded read-ahead before an image is used to feed a tape writer.
func (r *sequentialReader) Fill() error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	// Stay below the producer's low-free-buffer pause threshold.
	for len(r.ready) < max(1, cap(r.ready)*3/4) {
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case <-r.done:
			return nil
		case <-ticker.C:
		}
	}
	return nil
}
