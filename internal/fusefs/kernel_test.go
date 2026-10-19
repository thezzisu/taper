package fusefs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thezzisu/taper/internal/engine"
	"github.com/thezzisu/taper/internal/format"
	"golang.org/x/sys/unix"
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
	t.Cleanup(func() { r.Close() })
	tree, e := NewTree([]engine.Manifest{m}, r)
	if e != nil {
		t.Fatal(e)
	}
	mountpoint := mountKernelTree(t, tree)
	target := filepath.Join(mountpoint, "dir/data.bin")
	got, e := os.ReadFile(target)
	if e != nil || !bytes.Equal(got, data) {
		t.Fatal("kernel file read", len(got), e)
	}
	list, e := os.ReadDir(filepath.Join(mountpoint, "dir"))
	if e != nil || len(list) != 1 || list[0].Name() != "data.bin" {
		t.Fatal("kernel directory read", list, e)
	}
	f, e := os.Open(target)
	if e != nil {
		t.Fatal(e)
	}
	mapped, e := unix.Mmap(int(f.Fd()), 0, 4096, unix.PROT_READ, unix.MAP_PRIVATE)
	f.Close()
	if e != nil {
		t.Fatal("read-only private mmap", e)
	}
	if !bytes.Equal(mapped, data[:len(mapped)]) {
		t.Error("mapped bytes differ")
	}
	if e = unix.Munmap(mapped); e != nil {
		t.Fatal(e)
	}
	absent := filepath.Join(mountpoint, "new")
	for name, mutate := range map[string]func() error{
		"open for write": func() error {
			f, err := os.OpenFile(target, os.O_WRONLY, 0)
			if err == nil {
				f.Close()
			}
			return err
		},
		"create":   func() error { return os.WriteFile(absent, nil, 0600) },
		"mkdir":    func() error { return os.Mkdir(absent, 0700) },
		"chmod":    func() error { return os.Chmod(target, 0600) },
		"truncate": func() error { return os.Truncate(target, 0) },
		"rename":   func() error { return os.Rename(target, absent) },
		"unlink":   func() error { return os.Remove(target) },
	} {
		if e := mutate(); !errors.Is(e, unix.EROFS) {
			t.Fatalf("%s: want EROFS, got %v", name, e)
		}
	}
}

func mountKernelTree(t *testing.T, tree *Tree) string {
	t.Helper()
	mountpoint := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Mount(ctx, mountpoint, tree) }()
	t.Cleanup(func() {
		cancel()
		select {
		case e := <-done:
			if e != nil {
				t.Error(e)
			}
		case <-time.After(5 * time.Second):
			t.Error("unmount did not finish")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		var st unix.Statfs_t
		if e := unix.Statfs(mountpoint, &st); e == nil && st.Type == unix.FUSE_SUPER_MAGIC {
			if st.Flags&unix.ST_RDONLY == 0 {
				t.Fatal("kernel does not mark mount read-only")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mount never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return mountpoint
}

func TestKernelImageErrors(t *testing.T) {
	if os.Getenv("TAPER_FUSE_KERNEL") != "1" {
		t.Skip("requires an isolated mount namespace with mount permission")
	}
	for _, mode := range []string{"absent", "hole", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			img, tape, m, data := fileFixture(t)
			buf := make([]byte, format.BlockSize)
			for p := m.DataPos; p < m.CommitPos; p++ {
				if mode == "absent" || mode == "hole" && p == m.DataPos+1 {
					continue
				}
				if e := tape.Locate(p); e != nil {
					t.Fatal(e)
				}
				n, e := tape.ReadRecordInto(buf)
				if e != nil {
					t.Fatal(e)
				}
				if mode == "corrupt" && p == m.DataPos+1 {
					buf[64] ^= 1 // Valid header, invalid compressed payload/CRC.
				}
				if e = img.Locate(p); e != nil {
					t.Fatal(e)
				}
				if e = img.WriteRecord(buf[:n]); e != nil {
					t.Fatal(e)
				}
			}
			r, e := NewReader(context.Background(), img, nil, nil, 16, 8<<20)
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { r.Close() })
			tree, e := NewTree([]engine.Manifest{m}, r)
			if e != nil {
				t.Fatal(e)
			}
			root := mountKernelTree(t, tree)
			f, e := os.Open(filepath.Join(root, "dir/data.bin"))
			if e != nil {
				t.Fatal(e)
			}
			defer f.Close()
			st, e := f.Stat()
			if e != nil || st.Size() != int64(len(data)) {
				t.Fatal("offline metadata unavailable", st, e)
			}
			want := unix.ENODATA
			if mode == "corrupt" {
				want = unix.EIO
			}
			n, e := f.ReadAt(make([]byte, 4096), format.PayloadSize+100)
			if n != 0 || !errors.Is(e, want) {
				t.Fatalf("want %v without zero-filled data, got %d bytes: %v", want, n, e)
			}
		})
	}
}
