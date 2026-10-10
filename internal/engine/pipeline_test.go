package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thezzisu/taper/internal/format"
)

func TestWritePoolRefillsBeforeStarvation(t *testing.T) {
	ctx := context.Background()
	produced := make(chan struct{})
	resume := make(chan struct{})
	p, e := newPipeline(ctx, 8<<20, 4*format.PayloadSize, func(ctx context.Context, free chan []byte, ready chan chunk) error {
		defer close(ready)
		for i := 0; i < 4; i++ {
			if i == 2 {
				close(produced)
				select {
				case <-resume:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			var b []byte
			select {
			case b = <-free:
			case <-ctx.Done():
				return ctx.Err()
			}
			b = append(b, byte(i))
			select {
			case ready <- chunk{b: b}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	<-produced
	for i := 0; i < 2; i++ {
		c, ok, _, e := p.Next(time.Second)
		if e != nil || !ok {
			t.Fatal(e)
		}
		p.Release(c)
	}
	flushed := 0
	paused, e := p.Refill(func() error { flushed++; close(resume); return nil })
	if e != nil || !paused || flushed != 1 {
		t.Fatal(paused, flushed, e)
	}
	for i := 0; i < 2; i++ {
		c, ok, _, e := p.Next(time.Second)
		if e != nil || !ok {
			t.Fatal(e)
		}
		p.Release(c)
	}
	<-p.done
	if paused, e = p.Refill(func() error { t.Fatal("flushed after source completed"); return nil }); e != nil || paused {
		t.Fatal(paused, e)
	}
}

func TestPipelineTimeoutAndJoin(t *testing.T) {
	finished := make(chan struct{})
	p, err := newPipeline(context.Background(), 8<<20, 8<<20, func(ctx context.Context, free chan []byte, ready chan chunk) error {
		defer close(ready)
		defer close(finished)
		b := <-free
		b = append(b, 1)
		ready <- chunk{b: b, first: format.NoEntry}
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Fill(); err != nil {
		t.Fatal(err)
	}
	c, ok, _, err := p.Next(time.Second)
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	p.Release(c)
	if _, _, wait, err := p.Next(20 * time.Millisecond); err == nil || wait < 15*time.Millisecond {
		t.Fatal("stall not detected", wait, err)
	}
	p.Close()
	select {
	case <-finished:
	default:
		t.Fatal("producer leaked")
	}
}
func TestPipelineEarlyErrorAndCancellation(t *testing.T) {
	want := errors.New("source changed")
	p, e := newPipeline(context.Background(), 8<<20, 1, func(ctx context.Context, free chan []byte, ready chan chunk) error { defer close(ready); return want })
	if e != nil {
		t.Fatal(e)
	}
	if !errors.Is(p.Fill(), want) {
		t.Fatal("lost producer failure")
	}
	p.Close()
	if _, e = poolCount(65<<30, 100); e == nil {
		t.Fatal("unbounded pool")
	}
	if _, e = poolCount(1, 100); e == nil {
		t.Fatal("undersized pool")
	}
}
