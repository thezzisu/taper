package fusefs

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/thezzisu/taper/internal/engine"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
)

type cacheBlock struct {
	pos     uint64
	payload []byte
}
type fillRequest struct {
	batch engine.BatchMeta
	pos   uint64
	done  chan error
}
type Reader struct {
	image       *media.Image
	tape        media.Device
	present     map[string]bool
	min, memory uint64
	ctx         context.Context
	cancel      context.CancelFunc
	queue       chan fillRequest
	done        chan struct{}
	mu          sync.Mutex
	cache       map[uint64]*list.Element
	lru         *list.List
	limit       int
	decodeMu    sync.Mutex
	dec         *zstd.Decoder
	scratch     []byte
}

func NewReader(ctx context.Context, img *media.Image, tape media.Device, available []engine.BatchMeta, m, memory uint64) (*Reader, error) {
	if img == nil {
		return nil, errors.New("read-through image is required")
	}
	if memory == 0 {
		memory = 1 << 30
	}
	if memory < 8<<20 || memory > uint64(engine.MaxMemory) {
		return nil, errors.New("memory must be 8 MiB–64 GiB")
	}
	if m == 0 {
		return nil, errors.New("min-blocks must be positive")
	}
	dec, e := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(4<<20), zstd.WithDecoderMaxWindow(format.BlockSize))
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &Reader{image: img, tape: tape, present: map[string]bool{}, min: m, memory: memory, ctx: ctx, cancel: cancel, queue: make(chan fillRequest, 32), done: make(chan struct{}), cache: map[uint64]*list.Element{}, lru: list.New(), limit: int(min(32, memory/(8<<20))), dec: dec, scratch: make([]byte, format.BlockSize)}
	for _, b := range available {
		r.present[b.ID] = true
	}
	go r.schedule()
	return r, nil
}
func (r *Reader) schedule() {
	defer close(r.done)
	var deviceErr error
	for {
		var first fillRequest
		select {
		case <-r.ctx.Done():
			return
		case first = <-r.queue:
		}
		pending := []fillRequest{first}
		for len(pending) < 32 {
			select {
			case q := <-r.queue:
				pending = append(pending, q)
			default:
				goto ready
			}
		}
	ready:
		sort.SliceStable(pending, func(i, j int) bool { return pending[i].pos < pending[j].pos })
		for _, q := range pending {
			e := deviceErr
			if r.image.HasObject(q.pos) {
				e = nil
			} else if e == nil {
				if r.tape == nil || !r.present[q.batch.ID] {
					e = media.ErrMissing
				} else {
					s, t := PlanFillWindow(r.image.HasObject, q.batch.DataPos, q.batch.CommitPos, q.pos, r.min)
					e = engine.FillImage(r.ctx, r.tape, r.image, q.batch, s, t, engine.ReadOptions{Memory: int64(r.memory)})
					if e != nil {
						deviceErr = e
					}
				}
			}
			q.done <- e
		}
	}
}
func (r *Reader) block(ctx context.Context, b engine.BatchMeta, pos uint64) ([]byte, error) {
	r.mu.Lock()
	if e := r.cache[pos]; e != nil {
		r.lru.MoveToFront(e)
		p := e.Value.(cacheBlock).payload
		r.mu.Unlock()
		return p, nil
	}
	r.mu.Unlock()
	if !r.image.HasObject(pos) {
		q := fillRequest{batch: b, pos: pos, done: make(chan error, 1)}
		select {
		case r.queue <- q:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-r.ctx.Done():
			return nil, r.ctx.Err()
		}
		select {
		case e := <-q.done:
			if e != nil {
				return nil, e
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-r.ctx.Done():
			return nil, r.ctx.Err()
		}
	}
	r.decodeMu.Lock()
	defer r.decodeMu.Unlock()
	r.mu.Lock()
	if e := r.cache[pos]; e != nil {
		p := e.Value.(cacheBlock).payload
		r.mu.Unlock()
		return p, nil
	}
	r.mu.Unlock()
	n, e := r.image.ReadAt(pos, r.scratch)
	if e != nil {
		return nil, e
	}
	f, e := format.DecodeFrame(r.scratch[:n], r.dec)
	if e != nil {
		return nil, e
	}
	if f.Kind != format.Data || f.Batch.String() != b.ID || f.Seq != pos-b.DataPos {
		return nil, errors.New("cached DATA identity mismatch")
	}
	expected := int64(format.PayloadSize)
	if pos == b.CommitPos-1 {
		expected = b.Logical - int64(b.Blocks-1)*format.PayloadSize
	}
	if int64(len(f.Payload)) != expected {
		return nil, errors.New("cached DATA size mismatch")
	}
	payload := append([]byte(nil), f.Payload...)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[pos] = r.lru.PushFront(cacheBlock{pos, payload})
	if r.lru.Len() > r.limit {
		last := r.lru.Back()
		delete(r.cache, last.Value.(cacheBlock).pos)
		r.lru.Remove(last)
	}
	return payload, nil
}
func (r *Reader) Read(ctx context.Context, b engine.BatchMeta, data, size, offset int64, dst []byte) (int, error) {
	if offset < 0 || size < 0 || data < 0 || data > b.Logical || size > b.Logical-data {
		return 0, errors.New("invalid file extent")
	}
	if offset >= size || len(dst) == 0 {
		return 0, nil
	}
	want := int(min(int64(len(dst)), size-offset))
	copied := 0
	for copied < want {
		if e := ctx.Err(); e != nil {
			return 0, e
		}
		o := data + offset + int64(copied)
		seq := uint64(o / format.PayloadSize)
		pos := b.DataPos + seq
		p, e := r.block(ctx, b, pos)
		if e != nil {
			return 0, fmt.Errorf("archive %s object %d: %w", b.ID, pos, e)
		}
		inside := int(o % format.PayloadSize)
		if inside >= len(p) {
			return 0, errors.New("file exceeds DATA payload")
		}
		n := copy(dst[copied:want], p[inside:])
		copied += n
	}
	return copied, nil
}
func (r *Reader) Close() error {
	r.cancel()
	<-r.done
	r.decodeMu.Lock()
	defer r.decodeMu.Unlock()
	r.dec.Close()
	return r.image.Flush()
}
