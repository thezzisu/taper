package engine

import (
	"context"
	"github.com/thezzisu/taper/internal/policy"
	"os"
	"path/filepath"
	"testing"
)

func TestProbeSupply(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "data"), make([]byte, 12<<20), 0600)
	m, e := Scan(context.Background(), root, "probe", policy.Rules{})
	if e != nil {
		t.Fatal(e)
	}
	r, e := ProbeSupply(context.Background(), m, 8<<20, 16)
	if e != nil {
		t.Fatal(e)
	}
	if r.Blocks == 0 || r.Bytes == 0 || r.Memory == 0 {
		t.Fatalf("invalid supply result: %+v", r)
	}
	fast, e := ProbeSupply(context.Background(), m, 8<<20, 1000)
	if e != nil {
		t.Fatal(e)
	}
	if fast.Blocks != r.Blocks || fast.Bytes != r.Bytes {
		t.Fatalf("probe inconsistency: slow=%+v fast=%+v", r, fast)
	}
}
