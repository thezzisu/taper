package engine

import (
	"database/sql"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/thezzisu/taper/internal/catalog"
	"golang.org/x/sys/unix"
	"hash"
	"os"
	"syscall"
)

// Callers must keep sources unchanged through planning and all replica writes.
type HashCache struct {
	s          *catalog.Store
	root, fsid string
}

func NewHashCache(s *catalog.Store, root string) (*HashCache, error) {
	var st unix.Statfs_t
	if e := unix.Statfs(root, &st); e != nil {
		return nil, e
	}
	return &HashCache{s: s, root: root, fsid: fmt.Sprintf("%d:%d", st.Fsid.Val[0], st.Fsid.Val[1])}, nil
}
func (c *HashCache) Get(p string, info os.FileInfo) (string, bool, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false, nil
	}
	var h string
	e := c.s.DB.QueryRow("SELECT hash FROM scan_cache WHERE root=? AND path=? AND fsid=? AND device=? AND inode=? AND size=? AND mtime=? AND ctime=?", c.root, p, c.fsid, int64(st.Dev), int64(st.Ino), info.Size(), info.ModTime().UnixNano(), st.Ctim.Sec*1e9+st.Ctim.Nsec).Scan(&h)
	if errors.Is(e, sql.ErrNoRows) {
		return "", false, nil
	}
	if e != nil {
		return "", false, e
	}
	b, e := hex.DecodeString(h)
	return h, e == nil && len(b) == 32, nil
}
func (c *HashCache) Put(p string, info os.FileInfo, h string) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	_, e := c.s.DB.Exec("INSERT INTO scan_cache VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(root,path) DO UPDATE SET fsid=excluded.fsid,device=excluded.device,inode=excluded.inode,size=excluded.size,mtime=excluded.mtime,ctime=excluded.ctime,hash=excluded.hash", c.root, p, c.fsid, int64(st.Dev), int64(st.Ino), info.Size(), info.ModTime().UnixNano(), st.Ctim.Sec*1e9+st.Ctim.Nsec, h)
	if e == nil {
		_, e = c.s.DB.Exec("DELETE FROM scan_checkpoints WHERE root=? AND path=?", c.root, p)
	}
	return e
}

func (c *HashCache) Resume(p string, info os.FileInfo, h hash.Hash) (offset int64, err error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, nil
	}
	var state []byte
	err = c.s.DB.QueryRow("SELECT offset,state FROM scan_checkpoints WHERE root=? AND path=? AND fsid=? AND device=? AND inode=? AND size=? AND mtime=? AND ctime=?", c.root, p, c.fsid, int64(st.Dev), int64(st.Ino), info.Size(), info.ModTime().UnixNano(), st.Ctim.Sec*1e9+st.Ctim.Nsec).Scan(&offset, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if offset < 0 || offset > info.Size() {
		return 0, fmt.Errorf("invalid scan checkpoint offset")
	}
	if err = h.(encoding.BinaryUnmarshaler).UnmarshalBinary(state); err != nil {
		return 0, err
	}
	return offset, nil
}
func (c *HashCache) Checkpoint(p string, info os.FileInfo, h hash.Hash, offset int64) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	state, e := h.(encoding.BinaryMarshaler).MarshalBinary()
	if e != nil {
		return e
	}
	_, e = c.s.DB.Exec("INSERT INTO scan_checkpoints VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(root,path) DO UPDATE SET fsid=excluded.fsid,device=excluded.device,inode=excluded.inode,size=excluded.size,mtime=excluded.mtime,ctime=excluded.ctime,offset=excluded.offset,state=excluded.state", c.root, p, c.fsid, int64(st.Dev), int64(st.Ino), info.Size(), info.ModTime().UnixNano(), st.Ctim.Sec*1e9+st.Ctim.Nsec, offset, state)
	return e
}
