package engine

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/thezzisu/taper/internal/format"
)

type SupplyResult struct {
	Bytes        int64         `json:"logical_bytes"`
	WireBytes    int64         `json:"record_bytes"`
	Blocks       uint64        `json:"blocks"`
	Duration     time.Duration `json:"duration_ns"`
	Prefill      time.Duration `json:"prefill_ns"`
	Underflows   int           `json:"underflows"`
	Wait         time.Duration `json:"total_wait_ns"`
	LongestWait  time.Duration `json:"longest_wait_ns"`
	MinReady     int           `json:"min_ready_records"`
	MaxReady     int           `json:"max_ready_records"`
	Memory       int64         `json:"pool_bytes"`
	ConsumerMBps float64       `json:"consumer_mbps"`
}

func ProbeSupply(ctx context.Context, m Manifest, memory int64, consumerMBps float64) (SupplyResult, error) {
	return probeSupply(ctx, m, memory, consumerMBps, false, nil)
}
func probeSupply(ctx context.Context, m Manifest, memory int64, rate float64, compressed bool, notify func(SupplyResult)) (r SupplyResult, err error) {
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 {
		return r, errors.New("consumer speed must be finite and positive")
	}
	p, err := newPipeline(ctx, memory, m.Logical, func(ctx context.Context, free chan []byte, ready chan chunk) error {
		return Produce(ctx, m, free, ready)
	})
	if err != nil {
		return r, err
	}
	defer p.Close()
	started := time.Now()
	if err = p.Fill(); err != nil {
		return r, err
	}
	r.Prefill = time.Since(started)
	r.Memory = p.poolBytes
	r.ConsumerMBps = rate
	r.MinReady = len(p.ready)
	r.MaxReady = len(p.ready)
	var enc *zstd.Encoder
	if compressed {
		enc, err = zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedFastest))
		if err != nil {
			return r, err
		}
		defer enc.Close()
	}
	out := make([]byte, 0, format.BlockSize+4096)
	deadline := time.Now()
	lastNotice := deadline
	for {
		c, ok, wait, e := p.Next(0)
		if e != nil {
			return r, e
		}
		if !ok {
			break
		}
		r.Wait += wait
		r.LongestWait = max(r.LongestWait, wait)
		if wait > 5*time.Millisecond {
			r.Underflows++
			deadline = time.Now()
		}
		out, e = format.AppendFrame(out, format.Frame{Kind: format.Data, Batch: m.ID, Seq: r.Blocks, First: c.first, Payload: c.b}, enc)
		if e != nil {
			return r, e
		}
		deadline = deadline.Add(time.Duration(float64(len(out)) / rate / 1e6 * float64(time.Second)))
		if delay := time.Until(deadline); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return r, ctx.Err()
			case <-timer.C:
			}
		}
		r.Bytes += int64(len(c.b))
		r.WireBytes += int64(len(out))
		r.Blocks++
		p.Release(c)
		// Ignore the inevitable empty queue after the final block.
		if r.Bytes < m.Logical {
			r.MinReady = min(r.MinReady, len(p.ready))
			r.MaxReady = max(r.MaxReady, len(p.ready))
		}
		if notify != nil && time.Since(lastNotice) > 15*time.Second {
			r.Duration = time.Since(started)
			notify(r)
			lastNotice = time.Now()
		}
	}
	r.Duration = time.Since(started)
	if r.Bytes != m.Logical {
		return r, errors.New("producer length differs from manifest")
	}
	return r, nil
}
