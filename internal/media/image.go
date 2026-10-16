package media

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/thezzisu/taper/internal/format"
	"golang.org/x/crypto/chacha20"
	"golang.org/x/sys/unix"
)

const ImageHeaderSize = 1 << 20
const imageSuperSize = 4096
const ImageMaxObjects = uint64((ImageHeaderSize - imageSuperSize) * 4)
const imageWriteWindow = 4 << 20
const imageCheckpointBytes = 4 << 30

// ErrMissing is a hole inside an image, never a tape EOD event.
var ErrMissing = errors.New("image object is unavailable")

// Image stores one immutable replica stream in fixed sparse slots. Disk bitmap
// publication follows fdatasync of the payload; Flush/Filemark/Close commit any
// pending records. In-process readers may use pending records before publication.
type Image struct {
	mu                   sync.Mutex
	f                    *os.File
	writable             bool
	ReplicaID            string
	KeyID                string
	id                   format.ID
	key                  []byte
	pos, maxPos          uint64
	bitmap               []byte
	dirtyStart, dirtyEnd int
	unpublishedBytes     int64
	failed               error
	writeBuf             []byte
	writeStart           uint64
}

// OpenImage requires an expected identity. It creates new writable files but
// never replaces an occupied file or tolerates a different key/set identity.
func OpenImage(name string, writable bool, replicaID, keyID string) (*Image, error) {
	id, err := format.ParseID(replicaID)
	if err != nil || id.String() != replicaID {
		return nil, errors.New("invalid image replica ID")
	}
	if keyID != "" {
		b, e := hex.DecodeString(keyID)
		if e != nil || len(b) != 16 {
			return nil, errors.New("invalid image key ID")
		}
	}
	flags := os.O_RDONLY
	if writable {
		flags = os.O_RDWR | os.O_CREATE
	}
	f, err := os.OpenFile(name, flags, 0600)
	if err != nil {
		return nil, err
	}
	v := &Image{f: f, writable: writable, ReplicaID: replicaID, KeyID: keyID, id: id, bitmap: make([]byte, ImageHeaderSize-imageSuperSize), dirtyStart: -1}
	fail := func(e error) (*Image, error) { f.Close(); return nil, e }
	lock := unix.LOCK_SH
	if writable {
		lock = unix.LOCK_EX
	}
	if err = unix.Flock(int(f.Fd()), lock|unix.LOCK_NB); err != nil {
		return fail(err)
	}
	st, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if !st.Mode().IsRegular() {
		return fail(errors.New("image must be a regular file"))
	}
	if st.Size() == 0 {
		if !writable {
			return fail(errors.New("empty image"))
		}
		h := make([]byte, ImageHeaderSize)
		copy(h, v.super())
		if err = writeAt(f, h, 0); err != nil {
			return fail(err)
		}
		if err = f.Sync(); err != nil {
			return fail(err)
		}
		dir, e := os.Open(filepath.Dir(name))
		if e != nil {
			return fail(e)
		}
		e = errors.Join(dir.Sync(), dir.Close())
		if e != nil {
			return fail(e)
		}
	} else {
		h := make([]byte, imageSuperSize)
		if _, err = f.ReadAt(h, 0); err != nil {
			return fail(err)
		}
		want := binary.LittleEndian.Uint32(h[12:])
		clear(h[12:16])
		if string(h[:8]) != "TAPERIMG" || binary.LittleEndian.Uint32(h[8:]) != 1 || crc32.Checksum(h, format.CRC) != want {
			return fail(errors.New("invalid image superblock"))
		}
		if !bytes.Equal(h[32:48], id[:]) || string(bytes.TrimRight(h[48:80], "\x00")) != keyID {
			return fail(errors.New("image replica/key identity mismatch"))
		}
		if !allZero(h[24:32]) || !allZero(h[80:]) {
			return fail(errors.New("unsupported image superblock fields"))
		}
		v.maxPos = binary.LittleEndian.Uint64(h[16:])
		if v.maxPos > ImageMaxObjects {
			return fail(errors.New("image exceeds bitmap capacity"))
		}
		if _, err = f.ReadAt(v.bitmap, imageSuperSize); err != nil {
			return fail(err)
		}
		for p := uint64(0); p < ImageMaxObjects; p++ {
			state := v.state(p)
			if state == 3 || p >= v.maxPos && state != 0 {
				return fail(fmt.Errorf("invalid image bitmap at object %d", p))
			}
		}
	}
	return v, nil
}
func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
func writeAt(f *os.File, b []byte, off int64) error {
	n, e := f.WriteAt(b, off)
	if e == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return e
}
func (v *Image) super() []byte {
	b := make([]byte, imageSuperSize)
	copy(b, "TAPERIMG")
	binary.LittleEndian.PutUint32(b[8:], 1)
	binary.LittleEndian.PutUint64(b[16:], v.maxPos)
	copy(b[32:], v.id[:])
	copy(b[48:80], v.KeyID)
	binary.LittleEndian.PutUint32(b[12:], crc32.Checksum(b, format.CRC))
	return b
}
func (v *Image) state(p uint64) byte { return v.bitmap[p/4] >> (p % 4 * 2) & 3 }
func (v *Image) set(p uint64, state byte) {
	j := int(p / 4)
	shift := p % 4 * 2
	v.bitmap[j] = v.bitmap[j]&^(3<<shift) | state<<shift
	if v.dirtyStart < 0 || j < v.dirtyStart {
		v.dirtyStart = j
	}
	v.dirtyEnd = max(v.dirtyEnd, j+1)
	v.maxPos = max(v.maxPos, p+1)
}
func slot(p uint64) int64 { return int64(p+1) << 20 }
func (v *Image) available(write bool) error {
	if v.f == nil {
		return os.ErrClosed
	}
	if v.failed != nil {
		return v.failed
	}
	if write && !v.writable {
		return errors.New("read-only image")
	}
	return nil
}
func (v *Image) transform(b []byte, p uint64) error {
	if v.KeyID == "" {
		return nil
	}
	if len(v.key) != 32 {
		return errors.New("matching image key required")
	}
	var n [24]byte
	copy(n[:16], v.id[:])
	binary.LittleEndian.PutUint64(n[16:], p)
	c, e := chacha20.NewUnauthenticatedCipher(v.key, n[:])
	if e != nil {
		return e
	}
	var stored [4]byte
	copy(stored[:], b[44:48])
	c.XORKeyStream(b, b)
	copy(b[44:48], stored[:])
	return nil
}
func (v *Image) SetKey(k []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if e := v.available(false); e != nil {
		return e
	}
	if len(k) == 0 {
		clear(v.key)
		v.key = nil
		return nil
	}
	if len(k) != 32 || v.KeyID == "" || KeyID(k) != v.KeyID {
		return errors.New("image key does not match key ID")
	}
	clear(v.key)
	v.key = append([]byte(nil), k...)
	return nil
}
func (v *Image) HasObject(p uint64) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.f != nil && v.failed == nil && p < v.maxPos && v.state(p) != 0
}
func (v *Image) HasRange(s, e uint64) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.f == nil || v.failed != nil || s > e || e > v.maxPos {
		return false
	}
	for p := s; p < e; p++ {
		if v.state(p) == 0 {
			return false
		}
	}
	return true
}
func (v *Image) MaxPos() uint64 { v.mu.Lock(); defer v.mu.Unlock(); return v.maxPos }
func (v *Image) Position() (uint64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.pos, v.available(false)
}
func (v *Image) Locate(p uint64) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if e := v.available(false); e != nil {
		return e
	}
	if p > ImageMaxObjects {
		return errors.New("image position out of bounds")
	}
	v.pos = p
	return nil
}
func (v *Image) End() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if e := v.available(false); e != nil {
		return e
	}
	v.pos = v.maxPos
	return nil
}
func (v *Image) readAt(p uint64, b []byte) (int, error) {
	if e := v.available(false); e != nil {
		return 0, e
	}
	if p >= v.maxPos {
		return 0, io.EOF
	}
	switch v.state(p) {
	case 0:
		return 0, ErrMissing
	case 2:
		return 0, ErrFilemark
	case 3:
		return 0, errors.New("invalid image state")
	}
	if len(b) < format.BlockSize {
		return 0, errors.New("record buffer must be at least 1 MiB")
	}
	off := slot(p) - slot(v.writeStart)
	buffered := off >= 0 && off < int64(len(v.writeBuf))
	if buffered {
		copy(b[:64], v.writeBuf[off:off+64])
	} else if _, e := v.f.ReadAt(b[:64], slot(p)); e != nil {
		return 0, fmt.Errorf("truncated image record header: %w", e)
	}
	stored := binary.LittleEndian.Uint32(b[44:48])
	if stored > format.PayloadSize {
		return 0, errors.New("invalid image record length")
	}
	n := int(stored) + 64
	if buffered {
		if off+int64(n) > int64(len(v.writeBuf)) {
			return 0, errors.New("truncated buffered record")
		}
		copy(b[64:n], v.writeBuf[off+64:off+int64(n)])
	} else if _, e := v.f.ReadAt(b[64:n], slot(p)+64); e != nil {
		return 0, fmt.Errorf("truncated image record: %w", e)
	}
	if e := v.transform(b[:n], p); e != nil {
		return 0, e
	}
	if e := format.ValidateHeader(b[:n]); e != nil {
		return 0, e
	}
	return n, nil
}
func (v *Image) ReadAt(p uint64, b []byte) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.readAt(p, b)
}
func (v *Image) ReadRecordInto(b []byte) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	n, e := v.readAt(v.pos, b)
	if e == nil || e == ErrFilemark {
		v.pos++
	}
	return n, e
}
func (v *Image) ReadRecord() ([]byte, error) {
	b := make([]byte, format.BlockSize)
	n, e := v.ReadRecordInto(b)
	return b[:n], e
}

// WriteRecord copies encrypted frames into a reusable sequential buffer. Short
// records keep their sparse slot tails; nonadjacent writes drain the buffer.
// Pending records are visible to this Image before durable bitmap publication.
func (v *Image) WriteRecord(b []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if e := v.available(true); e != nil {
		return e
	}
	if v.pos >= ImageMaxObjects {
		return errors.New("image bitmap is full")
	}
	if e := format.ValidateHeader(b); e != nil {
		return e
	}
	if v.state(v.pos) != 0 {
		return errors.New("image object already present; existing records are immutable")
	}
	if len(v.writeBuf) > 0 && (slot(v.pos) != slot(v.writeStart)+int64(len(v.writeBuf)) || len(v.writeBuf)+len(b) > imageWriteWindow) {
		if e := v.drain(); e != nil {
			return e
		}
	}
	if v.writeBuf == nil {
		v.writeBuf = make([]byte, 0, imageWriteWindow)
	}
	if len(v.writeBuf) == 0 {
		v.writeStart = v.pos
	}
	off := len(v.writeBuf)
	v.writeBuf = append(v.writeBuf, b...)
	if e := v.transform(v.writeBuf[off:], v.pos); e != nil {
		v.writeBuf = v.writeBuf[:off]
		return e
	}
	v.set(v.pos, 1)
	v.pos++
	v.unpublishedBytes += int64(len(b))
	if len(v.writeBuf) == cap(v.writeBuf) {
		if e := v.drain(); e != nil {
			return e
		}
	}
	if v.unpublishedBytes >= imageCheckpointBytes {
		return v.flush()
	}
	return nil
}

// CheckSpace rejects a read-only/full destination before a source tape moves.
// needed is a conservative allocation bound, not the image's sparse file size.
func (v *Image) CheckSpace(needed int64) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.available(true); err != nil {
		return err
	}
	if needed < 0 {
		return errors.New("invalid image allocation bound")
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(v.f.Fd()), &fs); err != nil {
		return err
	}
	available := fs.Bavail * uint64(fs.Bsize)
	if uint64(needed) > available {
		return fmt.Errorf("image needs at most %d additional bytes; filesystem has %d available", needed, available)
	}
	return nil
}

func (v *Image) drain() error {
	if len(v.writeBuf) == 0 {
		return nil
	}
	if e := writeAt(v.f, v.writeBuf, slot(v.writeStart)); e != nil {
		v.failed = e
		return e
	}
	// Start payload writeback while incoming records are processed. This is a
	// scheduling hint, not durability: flush still fsyncs data before coverage.
	if e := unix.SyncFileRange(int(v.f.Fd()), slot(v.writeStart), int64(len(v.writeBuf)), unix.SYNC_FILE_RANGE_WRITE); e != nil {
		v.failed = e
		return e
	}
	v.writeBuf = v.writeBuf[:0]
	return nil
}
func (v *Image) Filemark() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if e := v.available(true); e != nil {
		return e
	}
	if v.pos >= ImageMaxObjects || v.state(v.pos) != 0 {
		return errors.New("filemark position unavailable")
	}
	v.set(v.pos, 2)
	v.pos++
	return v.flush()
}
func (v *Image) flush() (err error) {
	if e := v.available(false); e != nil {
		return e
	}
	if v.dirtyStart < 0 {
		return nil
	}
	defer func() {
		if err != nil {
			v.failed = err
		}
	}()
	if err = v.drain(); err != nil {
		return
	}
	if err = unix.Fdatasync(int(v.f.Fd())); err != nil {
		return
	}
	if err = writeAt(v.f, v.bitmap[v.dirtyStart:v.dirtyEnd], int64(imageSuperSize+v.dirtyStart)); err != nil {
		return
	}
	if err = writeAt(v.f, v.super(), 0); err != nil {
		return
	}
	if err = v.f.Sync(); err != nil {
		return
	}
	v.dirtyStart = -1
	v.dirtyEnd = 0
	v.unpublishedBytes = 0
	return nil
}
func (v *Image) Flush() error { v.mu.Lock(); defer v.mu.Unlock(); return v.flush() }
func (v *Image) PunchHole(s, e uint64) (err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err = v.available(true); err != nil {
		return
	}
	if s > e || e > ImageMaxObjects {
		return errors.New("invalid hole range")
	}
	if s == e {
		return nil
	}
	if err = v.flush(); err != nil {
		return
	}
	// Withdraw coverage durably BEFORE discarding payload, so interruption cannot
	// leave a published object pointing to punched zeroes. MaxPos stays unchanged.
	for p := s; p < min(e, v.maxPos); p++ {
		v.set(p, 0)
	}
	if err = v.flush(); err != nil {
		return
	}
	err = unix.Fallocate(int(v.f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, slot(s), int64(e-s)<<20)
	if err != nil {
		return
	}
	return unix.Fdatasync(int(v.f.Fd()))
}
func (v *Image) Info() (Info, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if e := v.available(false); e != nil {
		return Info{}, e
	}
	p := v.pos
	return Info{Serial: v.ReplicaID, Position: &p, MaxRecord: format.BlockSize, Encryption: v.KeyID != ""}, nil
}
func (v *Image) ReadIdentity() ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if e := v.available(false); e != nil {
		return nil, e
	}
	b, e := json.Marshal([]string{v.ReplicaID, "image", v.KeyID})
	return append([]byte("taper1:"), b...), e
}
func (v *Image) WriteIdentity([]byte) error { return errors.New("image identity is fixed at creation") }
func (v *Image) SetCompression(bool) error  { return nil }
func (v *Image) Eject() error               { return v.Locate(0) }
func (v *Image) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.f == nil {
		return nil
	}
	e := v.flush()
	clear(v.key)
	v.key = nil
	clear(v.writeBuf)
	v.writeBuf = nil
	e = errors.Join(e, v.f.Close())
	v.f = nil
	return e
}
