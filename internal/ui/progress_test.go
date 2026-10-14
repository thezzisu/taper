package ui

import (
	"testing"
	"time"
)

func TestRateEstimatorUsesProducerSamplesOnly(t *testing.T) {
	var r rateEstimator
	start := time.Unix(100, 0)
	r.update(start, 0)
	r.update(start.Add(10*time.Second), 1_000_000_000)
	if got := r.load(); got != 100 {
		t.Fatalf("first measured rate = %.2f, want 100", got)
	}
	// Rendering between callbacks must not alter the estimate.
	if got := r.load(); got != 100 {
		t.Fatalf("render changed rate = %.2f", got)
	}
	r.update(start.Add(20*time.Second), 3_000_000_000)
	if got := r.load(); got != 125 {
		t.Fatalf("smoothed rate = %.2f, want 125", got)
	}
}

func TestRateEstimatorIgnoresInvalidSamples(t *testing.T) {
	var r rateEstimator
	start := time.Unix(100, 0)
	r.update(start, 1_000)
	r.update(start, 2_000)
	r.update(start.Add(time.Second), 900)
	if got := r.load(); got != 0 {
		t.Fatalf("invalid samples changed rate = %.2f", got)
	}
}

func TestRateEstimatorDoesNotDecayBetweenReports(t *testing.T) {
	var r rateEstimator
	start := time.Unix(100, 0)
	r.update(start, 0)
	r.update(start.Add(10*time.Second), 1_000_000_000)
	measured := r.load()
	// A renderer tick has no timestamp/sample and therefore cannot change it.
	if got := r.load(); got != measured {
		t.Fatalf("renderer changed producer rate: %.2f -> %.2f", measured, got)
	}
}

func TestFinalProgressSnapshotDoesNotBecomeRateSample(t *testing.T) {
	p := &Progress{}
	p.rate.update(time.Unix(100, 0), 0)
	p.rate.update(time.Unix(110, 0), 1_000_000_000)
	want := p.rate.load()
	p.SetFinal(2, 2_000_000_000)
	if got := p.rate.load(); got != want {
		t.Fatalf("finalization changed transfer rate: %.2f -> %.2f", want, got)
	}
}
