package engine

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/thezzisu/taper/internal/media"
)

type pacedImageSource struct {
	media.Device
	started time.Time
	bytes   int64
}

func (p *pacedImageSource) ReadRecordInto(b []byte) (int, error) {
	n, e := p.Device.ReadRecordInto(b)
	if p.started.IsZero() {
		p.started = time.Now()
	}
	p.bytes += int64(n)
	due := p.started.Add(time.Duration(float64(p.bytes) / 220_000_000 * float64(time.Second)))
	if wait := time.Until(due); wait > 0 {
		time.Sleep(wait)
	}
	return n, e
}

// Opt-in qualification exercises actual encryption, image checkpoints, bounded
// read-ahead, decode and content hashes. It never opens a physical tape node.
func TestStreamingImageQualification(t *testing.T) {
	if os.Getenv("TAPER_QUALIFY") != "1" {
		t.Skip("opt-in complete I/O path qualification")
	}
	s, d, root := setup(t)
	key := make([]byte, 32)
	rand.Read(key)
	if _, e := registerTestMember(s, d, "qualification", key, true, false); e != nil {
		t.Fatal(e)
	}
	set, _ := ResolveReplicaSet(s, "")
	file, e := os.Create(filepath.Join(root, "data.bin"))
	if e != nil {
		t.Fatal(e)
	}
	chunk := make([]byte, 4<<20)
	rand.Read(chunk)
	for n := 0; n < 64; n++ {
		if _, e = file.Write(chunk); e != nil {
			t.Fatal(e)
		}
	}
	if e = file.Close(); e != nil {
		t.Fatal(e)
	}
	image, e := media.OpenImage(filepath.Join(t.TempDir(), "source.img"), true, set.ID, set.KeyID)
	if e != nil {
		t.Fatal(e)
	}
	defer image.Close()
	StageAdd(context.Background(), s, root, nil, nil)
	m, e := StagePlan(context.Background(), s, "qualification", "", false, nil)
	if e != nil {
		t.Fatal(e)
	}
	const memory = 64 << 20
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	if _, _, e = StageWrite(context.Background(), s, d, nil, WriteOptions{Image: image, Memory: memory}); e != nil {
		t.Fatal(e)
	}
	writeTime := time.Since(started)
	dest, e := media.OpenImage(filepath.Join(t.TempDir(), "capture.img"), true, set.ID, set.KeyID)
	if e != nil {
		t.Fatal(e)
	}
	defer dest.Close()
	source := &pacedImageSource{Device: d}
	plan, e := PlanSync(s, source, dest, "", false)
	if e != nil {
		t.Fatal(e)
	}
	started = time.Now()
	result, e := TapeSync(context.Background(), s, source, dest, plan, nil, false, ReadOptions{Memory: memory})
	if e != nil {
		t.Fatal(e)
	}
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	if !dest.HasRange(m.BeginPos, m.EndPos) {
		t.Fatal("capture incomplete")
	}
	rate := float64(result.Bytes) / elapsed.Seconds() / 1e6
	if rate < 160 {
		t.Fatalf("complete image capture only %.1f MB/s", rate)
	}
	status, _ := os.ReadFile("/proc/self/status")
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "VmHWM:") || strings.HasPrefix(line, "VmSwap:") {
			t.Log(line)
		}
	}
	t.Logf("%d encoded bytes; image+simulated-tape write %s; paced capture %s (%.2f MB/s); allocations %d; allocated bytes %d; GC cycles %d", result.Bytes, writeTime, elapsed, rate, after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc, after.NumGC-before.NumGC)
}
