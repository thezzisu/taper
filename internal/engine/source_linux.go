package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

type SourceIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	FSID   string `json:"fsid"`
}

func IdentifySource(root string) (SourceIdentity, error) {
	st, e := os.Stat(root)
	if e != nil {
		return SourceIdentity{}, e
	}
	if !st.IsDir() {
		return SourceIdentity{}, fmt.Errorf("source must be a directory")
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return SourceIdentity{}, fmt.Errorf("source identity unavailable")
	}
	var fs unix.Statfs_t
	if e = unix.Statfs(root, &fs); e != nil {
		return SourceIdentity{}, e
	}
	return SourceIdentity{Device: sys.Dev, Inode: sys.Ino, FSID: fmt.Sprintf("%d:%d", fs.Fsid.Val[0], fs.Fsid.Val[1])}, nil
}
func CheckSource(m Manifest) error {
	if m.SourceIdentity == (SourceIdentity{}) {
		return fmt.Errorf("batch has no source identity: rescan a frozen source")
	}
	id, e := IdentifySource(m.Source)
	if e != nil {
		return e
	}
	if id != m.SourceIdentity {
		return fmt.Errorf("source filesystem or root changed; check mount and rescan")
	}
	return nil
}
func checkDestOutsideSource(source, dest string) error {
	src, e := filepath.EvalSymlinks(source)
	if e != nil {
		return e
	}
	dir, e := filepath.EvalSymlinks(filepath.Dir(dest))
	if e != nil {
		return e
	}
	rel, e := filepath.Rel(src, dir)
	if e == nil && (rel == "." || rel != ".." && len(rel) > 0 && rel[:min(3, len(rel))] != "../") {
		return fmt.Errorf("output must be outside the source directory")
	}
	return nil
}
