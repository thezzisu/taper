package engine

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thezzisu/taper/internal/policy"
)

// Opt-in host stress test: a sparse source uses negligible disk space but exercises
// all of the real hashing/packing and >64 GiB of stream data through a 64 GiB pool.
func TestLargePool(t *testing.T) {
	if os.Getenv("TAPER_LARGE_POOL_TEST") != "1" {
		t.Skip("set TAPER_LARGE_POOL_TEST=1 on a host with at least 70 GiB available")
	}
	root := t.TempDir()
	f, e := os.Create(filepath.Join(root, "sparse.bin"))
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Truncate(70 << 30); e != nil {
		t.Fatal(e)
	}
	f.Close()
	m, e := Scan(context.Background(), root, "64GiB-pool", policy.Rules{})
	if e != nil {
		t.Fatal(e)
	}
	r, e := ProbeSupply(context.Background(), m, 64<<30, 1000)
	if e != nil {
		t.Fatal(e)
	}
	if r.Bytes != m.Logical || r.Memory < 63<<30 || r.Underflows != 0 {
		t.Fatalf("64GiB stress: %+v", r)
	}
	b, _ := json.Marshal(r)
	t.Log(string(b))
}

func TestDiskSpool(t *testing.T) {
	if os.Getenv("TAPER_DISK_SPOOL_TEST") != "1" {
		t.Skip("set TAPER_DISK_SPOOL_TEST=1 and TMPDIR on the target disk")
	}
	root := t.TempDir()
	f, e := os.Create(filepath.Join(root, "sparse.bin"))
	if e != nil {
		t.Fatal(e)
	}
	f.Truncate(2 << 30)
	f.Close()
	m, e := Scan(context.Background(), root, "spool", policy.Rules{})
	if e != nil {
		t.Fatal(e)
	}
	out := filepath.Join(t.TempDir(), "spool.bin")
	start := time.Now()
	n, blocks, e := Spool(context.Background(), m, out, 256<<20)
	if e != nil {
		t.Fatal(e)
	}
	seconds := time.Since(start).Seconds()
	f, e = os.Open(out)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	r := bufio.NewReader(f)
	g, _, e := readStreamGroup(r)
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.New()
	if _, e = io.CopyN(h, r, g.Size); e != nil {
		t.Fatal(e)
	}
	if hex.EncodeToString(h.Sum(nil)) != m.Contents[0].Hash {
		t.Fatal("disk stream contents differ")
	}
	t.Logf("bytes=%d blocks=%d seconds=%.2f throughput=%.1f MB/s", n, blocks, seconds, float64(n)/seconds/1e6)
}
