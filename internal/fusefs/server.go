//go:build linux

package fusefs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/thezzisu/taper/internal/engine"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
	"golang.org/x/sys/unix"
)

const maxRequest = 128 << 10

var native = binary.NativeEndian

type node struct {
	ino, parent       uint64
	name              string
	children          map[string]uint64
	order             []string
	batch             engine.BatchMeta
	data, size, mtime int64
}
type Tree struct {
	nodes    map[uint64]*node
	Reader   *Reader
	uid, gid uint32
}

func NewTree(manifests []engine.Manifest, reader *Reader) (*Tree, error) {
	t := &Tree{nodes: map[uint64]*node{1: {ino: 1, parent: 1, children: map[string]uint64{}}}, Reader: reader, uid: uint32(os.Getuid()), gid: uint32(os.Getgid())}
	for _, m := range manifests {
		root := uint64(1)
		if len(manifests) > 1 {
			name := fmt.Sprintf("%d_%s_%s", m.Seq, m.ID.String()[:8], m.Name)
			name = strings.Map(func(r rune) rune {
				if r == '/' || r == '\\' || r == 0 || r < 32 {
					return '_'
				}
				return r
			}, name)
			if len(name) > 200 {
				name = name[:200]
			}
			root = t.directory(1, name)
		}
		for _, c := range m.Contents {
			for _, f := range c.Files {
				if e := format.ValidPath(f.Path); e != nil {
					return nil, e
				}
				parts := strings.Split(f.Path, "/")
				parent := root
				for _, part := range parts[:len(parts)-1] {
					child, ok := t.nodes[parent].children[part]
					if ok && t.nodes[child].children == nil {
						return nil, errors.New("file/directory path collision")
					}
					parent = t.directory(parent, part)
				}
				name := parts[len(parts)-1]
				if len(name) > 255 {
					return nil, errors.New("stored filename exceeds Linux NAME_MAX")
				}
				if _, ok := t.nodes[parent].children[name]; ok {
					return nil, errors.New("duplicate file/directory path")
				}
				ino := uint64(len(t.nodes) + 1)
				t.nodes[ino] = &node{ino: ino, parent: parent, name: name, batch: engine.Meta(m), data: c.Data, size: c.Size, mtime: f.MTime}
				t.nodes[parent].children[name] = ino
			}
		}
	}
	for _, n := range t.nodes {
		for name := range n.children {
			n.order = append(n.order, name)
		}
		sort.Strings(n.order)
	}
	return t, nil
}
func (t *Tree) directory(parent uint64, name string) uint64 {
	if n, ok := t.nodes[parent].children[name]; ok {
		return n
	}
	id := uint64(len(t.nodes) + 1)
	t.nodes[id] = &node{ino: id, parent: parent, name: name, children: map[string]uint64{}}
	t.nodes[parent].children[name] = id
	return id
}
func (t *Tree) attr(n *node) []byte {
	b := make([]byte, 88)
	native.PutUint64(b, n.ino)
	size := n.size
	mode := uint32(unix.S_IFREG | 0444)
	links := uint32(1)
	if n.children != nil {
		mode = unix.S_IFDIR | 0555
		size = 4096
		links = 2
	}
	native.PutUint64(b[8:], uint64(size))
	native.PutUint64(b[16:], uint64((size+511)/512))
	if n.mtime > 0 {
		native.PutUint64(b[32:], uint64(n.mtime/1e9))
		native.PutUint64(b[40:], uint64(n.mtime/1e9))
		native.PutUint32(b[52:], uint32(n.mtime%1e9))
		native.PutUint32(b[56:], uint32(n.mtime%1e9))
	}
	native.PutUint32(b[60:], mode)
	native.PutUint32(b[64:], links)
	native.PutUint32(b[68:], t.uid)
	native.PutUint32(b[72:], t.gid)
	native.PutUint32(b[80:], 4096)
	return b
}
func fuseError(e error) unix.Errno {
	if e == nil {
		return 0
	}
	var errno unix.Errno
	if errors.As(e, &errno) {
		return errno
	}
	if errors.Is(e, media.ErrMissing) {
		return unix.ENODATA
	}
	if errors.Is(e, context.Canceled) {
		return unix.EINTR
	}
	return unix.EIO
}

// dispatch uses the native Linux FUSE 7.31 wire format. It implements only
// regular-file and directory reads; unsupported operations return an errno.
func (t *Tree) dispatch(ctx context.Context, opcode uint32, id uint64, in []byte) (out []byte, errno unix.Errno, noReply bool) {
	n := t.nodes[id]
	switch opcode {
	case 2, 38, 42:
		return nil, 0, true // FORGET, DESTROY, BATCH_FORGET
	case 26: // INIT
		if len(in) < 16 {
			return nil, unix.EINVAL, false
		}
		major := native.Uint32(in)
		if major > 7 {
			b := make([]byte, 8)
			native.PutUint32(b, 7)
			native.PutUint32(b[4:], 31)
			return b, 0, false
		}
		if major != 7 || native.Uint32(in[4:]) < 23 {
			return nil, unix.EPROTO, false
		}
		b := make([]byte, 64)
		native.PutUint32(b, 7)
		native.PutUint32(b[4:], min(31, native.Uint32(in[4:])))
		native.PutUint32(b[8:], min(maxRequest, native.Uint32(in[8:])))
		native.PutUint32(b[12:], native.Uint32(in[12:])&1)
		native.PutUint16(b[16:], 8)
		native.PutUint16(b[18:], 6)
		native.PutUint32(b[20:], maxRequest)
		native.PutUint32(b[24:], 1)
		return b, 0, false
	case 36:
		return nil, unix.EAGAIN, false // INTERRUPT: request may have completed.
	}
	if n == nil {
		return nil, unix.ENOENT, false
	}
	switch opcode {
	case 1: // LOOKUP
		if n.children == nil {
			return nil, unix.ENOTDIR, false
		}
		end := bytes.IndexByte(in, 0)
		if end < 0 {
			return nil, unix.EINVAL, false
		}
		name := string(in[:end])
		child, ok := n.children[name]
		if name == "." {
			child, ok = n.ino, true
		}
		if name == ".." {
			child, ok = n.parent, true
		}
		if !ok {
			return nil, unix.ENOENT, false
		}
		b := make([]byte, 40)
		native.PutUint64(b, child)
		native.PutUint64(b[8:], 1)
		native.PutUint64(b[16:], 60)
		native.PutUint64(b[24:], 60)
		return append(b, t.attr(t.nodes[child])...), 0, false
	case 3: // GETATTR
		b := make([]byte, 16)
		native.PutUint64(b, 60)
		return append(b, t.attr(n)...), 0, false
	case 14, 27: // OPEN / OPENDIR
		if len(in) < 8 {
			return nil, unix.EINVAL, false
		}
		flags := native.Uint32(in)
		if flags&unix.O_ACCMODE != unix.O_RDONLY || flags&(unix.O_TRUNC|unix.O_APPEND) != 0 {
			return nil, unix.EROFS, false
		}
		if opcode == 27 && n.children == nil {
			return nil, unix.ENOTDIR, false
		}
		if opcode == 14 && n.children != nil {
			return nil, unix.EISDIR, false
		}
		b := make([]byte, 16)
		native.PutUint64(b, id)
		native.PutUint32(b[8:], 2)
		return b, 0, false
	case 15: // READ
		if n.children != nil {
			return nil, unix.EISDIR, false
		}
		if len(in) < 24 {
			return nil, unix.EINVAL, false
		}
		off := native.Uint64(in[8:])
		size := native.Uint32(in[16:])
		if size > maxRequest || off > 1<<63-1 {
			return nil, unix.EINVAL, false
		}
		if t.Reader == nil {
			return nil, unix.EIO, false
		}
		b := make([]byte, size)
		count, e := t.Reader.Read(ctx, n.batch, n.data, n.size, int64(off), b)
		if e != nil {
			return nil, fuseError(e), false
		}
		return b[:count], 0, false
	case 28: // READDIR; offsets are stable ordinals, not byte offsets.
		if n.children == nil {
			return nil, unix.ENOTDIR, false
		}
		if len(in) < 24 {
			return nil, unix.EINVAL, false
		}
		off, size := native.Uint64(in[8:]), native.Uint32(in[16:])
		if size > maxRequest {
			return nil, unix.EINVAL, false
		}
		names := append([]string{".", ".."}, n.order...)
		b := make([]byte, 0, size)
		for index := off; index < uint64(len(names)); index++ {
			name := names[index]
			child := n.ino
			if index == 1 {
				child = n.parent
			} else if index > 1 {
				child = n.children[name]
			}
			length := (24 + len(name) + 7) &^ 7
			if len(b)+length > int(size) {
				break
			}
			entry := make([]byte, length)
			native.PutUint64(entry, child)
			native.PutUint64(entry[8:], index+1)
			native.PutUint32(entry[16:], uint32(len(name)))
			kind := uint32(unix.DT_REG)
			if t.nodes[child].children != nil {
				kind = unix.DT_DIR
			}
			native.PutUint32(entry[20:], kind)
			copy(entry[24:], name)
			b = append(b, entry...)
		}
		return b, 0, false
	case 17:
		b := make([]byte, 80)
		native.PutUint64(b[24:], uint64(len(t.nodes)))
		native.PutUint32(b[40:], 4096)
		native.PutUint32(b[44:], 255)
		native.PutUint32(b[48:], 4096)
		return b, 0, false
	case 18, 20, 25, 29, 30:
		return nil, 0, false
	case 34:
		if len(in) < 8 {
			return nil, unix.EINVAL, false
		}
		if native.Uint32(in)&unix.W_OK != 0 {
			return nil, unix.EROFS, false
		}
		return nil, 0, false
	case 22:
		return nil, unix.ENODATA, false
	case 23:
		return nil, 0, false
	case 4, 6, 8, 9, 10, 11, 12, 13, 16, 21, 24, 35, 43, 45, 47, 51:
		return nil, unix.EROFS, false
	default:
		return nil, unix.ENOSYS, false
	}
}
func (t *Tree) serve(ctx context.Context, f *os.File) error {
	fd := int(f.Fd())
	var writes sync.Mutex
	var workers sync.WaitGroup
	defer workers.Wait()
	slots := make(chan struct{}, 8)
	reply := func(unique uint64, payload []byte, errno unix.Errno) {
		b := make([]byte, 16+len(payload))
		native.PutUint32(b, uint32(len(b)))
		native.PutUint32(b[4:], uint32(-int32(errno)))
		native.PutUint64(b[8:], unique)
		copy(b[16:], payload)
		writes.Lock()
		_, _ = unix.Write(fd, b)
		writes.Unlock()
	}
	for {
		b := make([]byte, maxRequest+4096)
		count, e := unix.Read(fd, b)
		if e == unix.EINTR {
			continue
		}
		if e != nil {
			if errors.Is(e, unix.ENODEV) || errors.Is(e, os.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			return e
		}
		if count < 40 {
			return io.ErrUnexpectedEOF
		}
		b = b[:count]
		length := native.Uint32(b)
		if int(length) != count {
			return errors.New("invalid FUSE message length")
		}
		opcode, id, unique := native.Uint32(b[4:]), native.Uint64(b[16:]), native.Uint64(b[8:])
		if opcode == 38 {
			return nil
		}
		if native.Uint16(b[36:]) != 0 {
			reply(unique, nil, unix.EINVAL)
			continue
		}
		if opcode != 15 {
			out, errno, noReply := t.dispatch(ctx, opcode, id, b[40:])
			if !noReply {
				reply(unique, out, errno)
			}
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		request := append([]byte(nil), b[40:]...)
		requestOpcode, requestID, requestUnique := opcode, id, unique
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-slots }()
			out, errno, _ := t.dispatch(ctx, requestOpcode, requestID, request)
			reply(requestUnique, out, errno)
		}()
	}
}

// Mount uses the kernel interface directly. Mount permission must be supplied
// by the operator; taper never invokes fusermount, sudo or a worker process.
func Mount(ctx context.Context, mountpoint string, tree *Tree) error {
	mountpoint, e := filepath.Abs(mountpoint)
	if e != nil {
		return e
	}
	st, e := os.Stat(mountpoint)
	if e != nil {
		return e
	}
	if !st.IsDir() {
		return errors.New("mountpoint is not a directory")
	}
	entries, e := os.ReadDir(mountpoint)
	if e != nil {
		return e
	}
	if len(entries) != 0 {
		return errors.New("mountpoint must be empty")
	}
	f, e := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	options := fmt.Sprintf("fd=%d,rootmode=40000,user_id=%d,group_id=%d,max_read=%d,default_permissions", f.Fd(), os.Getuid(), os.Getgid(), maxRequest)
	if e = unix.Mount("taper", mountpoint, "fuse", unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, options); e != nil {
		return fmt.Errorf("mount /dev/fuse: %w; direct FUSE mounting requires mount permission supplied outside taper", e)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer unix.Unmount(mountpoint, unix.MNT_DETACH)
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = unix.Unmount(mountpoint, unix.MNT_DETACH)
			_ = f.Close()
		case <-done:
		}
	}()
	defer close(done)
	return tree.serve(ctx, f)
}
