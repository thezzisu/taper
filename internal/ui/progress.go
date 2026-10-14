package ui

import (
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/schollz/progressbar/v3"
)

// Progress decouples terminal writes from the I/O callback. A slow terminal can
// delay rendering, never the tape's producer or consumer.
type Progress struct {
	count, bytes atomic.Int64
	measured     atomic.Bool
	rate         rateEstimator
	stop         chan bool
	done         chan struct{}
}

// rateEstimator changes only when the producer reports a new measurement. The
// renderer can refresh often without turning a quiet interval into a fake drop
// in throughput.
type rateEstimator struct {
	mu          sync.RWMutex
	lastAt      time.Time
	lastBytes   int64
	value       float64
	initialized bool
}

func (r *rateEstimator) update(at time.Time, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.initialized {
		delta := bytes - r.lastBytes
		elapsed := at.Sub(r.lastAt).Seconds()
		if delta >= 0 && elapsed > 0 {
			instant := float64(delta) / elapsed / 1e6
			if r.value == 0 {
				r.value = instant
			} else {
				r.value = r.value*0.75 + instant*0.25
			}
		}
	}
	r.lastAt, r.lastBytes, r.initialized = at, bytes, true
}

func (r *rateEstimator) load() float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.value
}

func NewProgress(w io.Writer, action string, total int64) *Progress {
	if total <= 0 {
		total = -1
	}
	p := &Progress{stop: make(chan bool, 1), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		started := time.Now()
		interval := 15 * time.Second
		var bar *progressbar.ProgressBar
		if IsTerminal(w) && os.Getenv("TERM") != "dumb" {
			interval = 250 * time.Millisecond
			bar = progressbar.NewOptions64(total,
				progressbar.OptionSetWriter(w), progressbar.OptionSetWidth(24),
				progressbar.OptionSetDescription(action), progressbar.OptionShowCount(),
				progressbar.OptionSetPredictTime(false), progressbar.OptionSetElapsedTime(false),
				progressbar.OptionUseANSICodes(true), progressbar.OptionSetRenderBlankState(true),
				progressbar.OptionOnCompletion(func() { fmt.Fprintln(w) }))
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		draw := func(finished, success bool) {
			n, bytes := p.count.Load(), p.bytes.Load()
			elapsed := time.Since(started)
			rate := p.rate.load()
			state := action
			if finished {
				if success {
					state += " complete"
				} else {
					state += " stopped"
				}
			} else if total > 0 && n >= total {
				state += " finalizing"
			}
			measurement := fmt.Sprintf("%.2f GB | %.1f MB/s", float64(bytes)/1e9, rate)
			if !p.measured.Load() {
				measurement = "preparing"
				if finished {
					measurement = "no streamed bytes"
				}
			}
			if bar == nil {
				if total > 0 {
					fmt.Fprintf(w, "%s: %d/%d objects, %s, %s\n", state, n, total, measurement, elapsed.Round(time.Second))
				} else {
					fmt.Fprintf(w, "%s: %s, %s\n", state, measurement, elapsed.Round(time.Second))
				}
				return
			}
			bar.Describe(fmt.Sprintf("%s | %s | %s", state, measurement, elapsed.Round(time.Second)))
			if !finished && total > 0 {
				n = min(n, total-1)
			}
			_ = bar.Set64(n)
			if finished {
				if success {
					_ = bar.Finish()
				} else {
					_ = bar.Exit()
				}
			}
		}
		for {
			select {
			case <-ticker.C:
				draw(false, false)
			case success := <-p.stop:
				draw(true, success)
				return
			}
		}
	}()
	return p
}

func (p *Progress) Update(count uint64, bytes int64) {
	p.update(count, bytes, true)
}

// SetFinal updates the displayed count without treating final catalog or
// filemark work as a new transfer-speed sample.
func (p *Progress) SetFinal(count uint64, bytes int64) {
	p.update(count, bytes, false)
}

func (p *Progress) update(count uint64, bytes int64, sample bool) {
	p.bytes.Store(bytes)
	p.count.Store(int64(count))
	p.measured.Store(true)
	if sample {
		p.rate.update(time.Now(), bytes)
	}
}

func (p *Progress) Close(success bool) { p.stop <- success; <-p.done }
