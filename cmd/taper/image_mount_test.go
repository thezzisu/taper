package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type mountArchive struct{ id, name string }

// Opt-in kernel acceptance completes the CLI stage -> encrypted image -> image
// merge -> filesystem read path. Device endpoints in the parent use only Sim.
func checkImageMount(t *testing.T, image string, archives []mountArchive, files map[string][]byte) {
	t.Helper()
	if os.Getenv("TAPER_FUSE_KERNEL") != "1" {
		t.Skip("requires an isolated mount namespace with mount permission")
	}
	for _, single := range []bool{false, true} {
		t.Run(fmt.Sprintf("single=%t", single), func(t *testing.T) {
			mountpoint := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			c := newCommand()
			c.SetOut(io.Discard)
			c.SetErr(io.Discard)
			args := []string{"replica", "mount", mountpoint, "--image", image, "--memory", "8388608"}
			selected := archives
			if single {
				selected = archives[len(archives)-1:]
				args = append(args, "--archive", selected[0].id[:6])
			}
			c.SetArgs(args)
			done := make(chan error, 1)
			go func() { done <- c.ExecuteContext(ctx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case e := <-done:
					if e != nil {
						t.Error(e)
					}
				case <-time.After(5 * time.Second):
					t.Error("mount did not stop")
				}
			})
			deadline := time.Now().Add(10 * time.Second)
			for {
				var st unix.Statfs_t
				if e := unix.Statfs(mountpoint, &st); e == nil && st.Type == unix.FUSE_SUPER_MAGIC {
					if st.Flags&unix.ST_RDONLY == 0 {
						t.Fatal("mount is not read-only")
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("mount never became ready")
				}
				time.Sleep(10 * time.Millisecond)
			}
			for seq, archive := range selected {
				root := mountpoint
				if !single {
					root = filepath.Join(root, fmt.Sprintf("%d_%s_%s", seq, archive.id[:8], archive.name))
				}
				count := 0
				e := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
					if err != nil || entry.IsDir() {
						return err
					}
					rel, err := filepath.Rel(root, path)
					if err != nil {
						return err
					}
					want, ok := files[rel]
					if !ok {
						return fmt.Errorf("unexpected mounted file %s", rel)
					}
					got, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(got, want) {
						return fmt.Errorf("mounted content differs for %s: %v", rel, err)
					}
					count++
					return nil
				})
				if e != nil || count != len(files) {
					t.Fatalf("mounted tree: %d files, want %d: %v", count, len(files), e)
				}
			}
		})
	}
}
