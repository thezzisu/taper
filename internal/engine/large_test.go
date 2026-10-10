package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
