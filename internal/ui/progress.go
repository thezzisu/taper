package ui

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"

	"github.com/schollz/progressbar/v3"
)

// Progress decouples terminal writes from the I/O callback. A slow terminal can
// delay rendering, never the tape's producer or consumer.
type Progress struct {
	count, bytes atomic.Int64
	measured     atomic.Bool
	stop         chan bool
	done         chan struct{}
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
				progressbar.OptionSetPredictTime(false), progressbar.OptionSetElapsedTime(true),
				progressbar.OptionUseANSICodes(true), progressbar.OptionSetRenderBlankState(true),
				progressbar.OptionOnCompletion(func() { fmt.Fprintln(w) }))
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		draw := func(finished, success bool) {
			n, bytes := p.count.Load(), p.bytes.Load()
			elapsed := time.Since(started)
			rate := float64(bytes) / max(elapsed.Seconds(), 0.001) / 1e6
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
			bar.Describe(state + " | " + measurement)
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
	p.bytes.Store(bytes)
	p.count.Store(int64(count))
	p.measured.Store(true)
}

func (p *Progress) Close(success bool) { p.stop <- success; <-p.done }
