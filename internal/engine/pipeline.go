package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/thezzisu/taper/internal/format"
)

const MaxMemory int64 = 64 << 30

type producer func(context.Context, chan []byte, chan chunk) error
type pipeline struct {
	ctx       context.Context
	cancel    context.CancelFunc
	free      chan []byte
	ready     chan chunk
	done      chan struct{}
	err       error
	poolBytes int64
}

func poolCount(memory, logical int64) (int, error) {
	if memory == 0 {
		memory = 1 << 30
	}
	if memory < 8<<20 || memory > MaxMemory {
		return 0, errors.New("memory must be between 8 MiB and 64 GiB")
	}
	if logical <= 0 {
		return 0, errors.New("empty batch cannot be streamed")
	}
	count := int((memory - 6<<20) / format.BlockSize)
	needed := int(1 + (logical-1)/format.PayloadSize)
	return min(count, max(2, needed)), nil
}

func newPipeline(ctx context.Context, memory, logical int64, produce producer) (*pipeline, error) {
	count, err := poolCount(memory, logical)
	if err != nil {
		return nil, err
	}
	if err = availableMemory(int64(count) * format.BlockSize); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	p := &pipeline{ctx: ctx, cancel: cancel, free: make(chan []byte, count), ready: make(chan chunk, count), done: make(chan struct{}), poolBytes: int64(count) * format.PayloadSize}
	for i := 0; i < count; i++ {
		p.free <- make([]byte, 0, format.PayloadSize)
	}
	go func() { p.err = produce(ctx, p.free, p.ready); close(p.done) }()
	return p, nil
}

// Fill happens before the first tape write. One buffer remains available to the producer.
func (p *pipeline) Fill() error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for len(p.ready) < max(1, cap(p.ready)-1) {
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case <-p.done:
			return p.err
		case <-ticker.C:
		}
	}
	return nil
}

// Next returns only actual consumer wait time, excluding time spent in device writes.
func (p *pipeline) Next(maxWait time.Duration) (chunk, bool, time.Duration, error) {
	select {
	case c, ok := <-p.ready:
		if !ok {
			<-p.done
			return c, false, 0, p.err
		}
		return c, true, 0, nil
	default:
	}
	started := time.Now()
	var timeout <-chan time.Time
	var timer *time.Timer
	if maxWait > 0 {
		timer = time.NewTimer(maxWait)
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case c, ok := <-p.ready:
		wait := time.Since(started)
		if !ok {
			<-p.done
			return c, false, 0, p.err
		}
		return c, true, wait, nil
	case <-p.ctx.Done():
		return chunk{}, false, time.Since(started), p.ctx.Err()
	case <-timeout:
		return chunk{}, false, time.Since(started), fmt.Errorf("source supply stalled for %s; stopping the replica", maxWait)
	}
}
func (p *pipeline) Release(c chunk) {
	select {
	case p.free <- c.b[:0]:
	case <-p.ctx.Done():
	}
}
func (p *pipeline) Close() { p.cancel(); <-p.done }

func availableMemory(wanted int64) error {
	if wanted >= 1<<30 {
		debug.FreeOSMemory()
	}
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			kib, e := strconv.ParseInt(fields[1], 10, 64)
			if e != nil {
				return e
			}
			if wanted+(2<<30) > kib*1024 {
				return fmt.Errorf("pool needs %d bytes plus 2 GiB headroom; available %d", wanted, kib*1024)
			}
			return nil
		}
	}
	return errors.New("cannot determine available memory")
}
