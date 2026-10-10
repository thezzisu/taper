package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thezzisu/taper/internal/format"
)

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
