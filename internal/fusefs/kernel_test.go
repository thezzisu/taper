package fusefs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thezzisu/taper/internal/engine"
	"github.com/thezzisu/taper/internal/format"
)

// Run in an isolated mount namespace:
// unshare -Urnm env TAPER_FUSE_KERNEL=1 go test ./internal/fusefs -run TestKernel
func TestKernelMount(t *testing.T) {
	if os.Getenv("TAPER_FUSE_KERNEL") != "1" {
		t.Skip("requires an isolated mount namespace with mount permission")
	}
	img, tape, m, data := fileFixture(t)
	buf := make([]byte, format.BlockSize)
	for p := m.DataPos; p < m.CommitPos; p++ {
		tape.Locate(p)
		n, e := tape.ReadRecordInto(buf)
		if e != nil {
			t.Fatal(e)
		}
		img.Locate(p)
		if e = img.WriteRecord(buf[:n]); e != nil {
			t.Fatal(e)
		}
	}
	r, e := NewReader(context.Background(), img, nil, nil, 16, 8<<20)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	tree, e := NewTree([]engine.Manifest{m}, r)
	if e != nil {
		t.Fatal(e)
	}
	mountpoint := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Mount(ctx, mountpoint, tree) }()
	target := filepath.Join(mountpoint, "dir/data.bin")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, e = os.Stat(target); e == nil {
			break
		}
		select {
		case e = <-done:
			t.Fatal("mount exited", e)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("mount never became ready", e)
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, e := os.ReadFile(target)
	if e != nil || !bytes.Equal(got, data) {
		t.Fatal("kernel file read", len(got), e)
	}
	list, e := os.ReadDir(filepath.Join(mountpoint, "dir"))
	if e != nil || len(list) != 1 || list[0].Name() != "data.bin" {
		t.Fatal("kernel directory read", list, e)
	}
	if f, e := os.OpenFile(target, os.O_WRONLY, 0); e == nil {
		f.Close()
		t.Fatal("read-only mount accepted write open")
	}
	cancel()
	select {
	case e = <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unmount did not finish")
	}
}
