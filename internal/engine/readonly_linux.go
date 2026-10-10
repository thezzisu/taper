package engine

import (
	"fmt"
	"golang.org/x/sys/unix"
)

func ReadOnly(p string) error {
	var s unix.Statfs_t
	if e := unix.Statfs(p, &s); e != nil {
		return e
	}
	if s.Flags&unix.ST_RDONLY == 0 {
		return fmt.Errorf("source is writable: freeze it and explicitly pass --frozen, or mount read-only")
	}
	return nil
}
