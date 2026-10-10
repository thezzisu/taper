package engine

import (
	"context"
	"github.com/thezzisu/taper/internal/policy"
	"os"
	"path/filepath"
	"testing"
)

func TestSpoolPath(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "data"), make([]byte, 5<<20), 0600)
	m, e := Scan(context.Background(), root, "spool", policy.Rules{})
	if e != nil {
		t.Fatal(e)
	}
	dst := filepath.Join(t.TempDir(), "stream.bin")
	n, b, e := Spool(context.Background(), m, dst, 8<<20)
	if e != nil || n != m.Logical || b == 0 {
		t.Fatalf("spool n=%d logical=%d b=%d e=%v", n, m.Logical, b, e)
	}
	st, _ := os.Stat(dst)
	if st.Size() != n {
		t.Fatal(st.Size(), n)
	}
}
