package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"time"

	"github.com/thezzisu/taper/internal/format"
	"golang.org/x/sys/unix"
)

type simHeader struct {
	Serial   string
	Capacity int64
	Compress bool
	Identity []byte
}
type simRecord struct {
	offset int64
	size   uint32
	kind   uint32
	key    [16]byte
}
type Sim struct {
	f        *os.File
	header   simHeader
	records  []simRecord
	pos      uint64
	used     int64
	writable bool
	key      [16]byte
	torn     bool
}

func CreateSim(name string, capacity int64, compress bool) error {
	if capacity <= 0 {
		return errors.New("capacity must be positive")
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	h, _ := json.Marshal(simHeader{Serial: format.NewID().String(), Capacity: capacity, Compress: compress})
	b := make([]byte, 4096)
	copy(b, "TAPERSIM")
	binary.LittleEndian.PutUint32(b[8:], uint32(len(h)))
	copy(b[12:], h)
	if _, err = f.Write(b); err != nil {
		return err
	}
	return f.Sync()
}
func OpenSim(name string, writable bool) (*Sim, error) {
	flags := os.O_RDONLY
	if writable {
		flags = os.O_RDWR
	}
	f, e := os.OpenFile(name, flags, 0600)
	if e != nil {
		return nil, e
	}
	s := &Sim{f: f, writable: writable}
	fail := func(e error) (*Sim, error) { f.Close(); return nil, e }
	lock := unix.LOCK_SH
	if writable {
		lock = unix.LOCK_EX
	}
	if e = unix.Flock(int(f.Fd()), lock|unix.LOCK_NB); e != nil {
		return fail(e)
	}
	b := make([]byte, 4096)
	if _, e = io.ReadFull(f, b); e != nil {
		return fail(e)
	}
	l := binary.LittleEndian.Uint32(b[8:])
	if !bytes.Equal(b[:8], []byte("TAPERSIM")) || l > 4084 {
		return fail(errors.New("invalid simulator header"))
	}
	if e = json.Unmarshal(b[12:12+l], &s.header); e != nil {
		return fail(e)
	}
	for {
		off, _ := f.Seek(0, io.SeekCurrent)
		h := make([]byte, 32)
		_, e = io.ReadFull(f, h)
		if e == io.EOF {
			break
		}
		if e != nil {
			s.torn = true
			break
		}
		size := binary.LittleEndian.Uint32(h)
		if size > format.BlockSize {
			return fail(errors.New("invalid simulator record size"))
		}
		r := simRecord{offset: off + 32, size: size, kind: binary.LittleEndian.Uint32(h[4:])}
		copy(r.key[:], h[16:])
		data := make([]byte, size)
		if _, e = io.ReadFull(f, data); e != nil {
			s.torn = true
			break
		}
		if crc32.Checksum(data, format.CRC) != binary.LittleEndian.Uint32(h[8:]) {
			return fail(fmt.Errorf("simulator envelope corrupt at %d", off))
		}
		s.used += int64(binary.LittleEndian.Uint32(h[12:]))
		s.records = append(s.records, r)
	}
	return s, nil
}
func (s *Sim) Info() (Info, error) {
	p := s.pos
	return Info{Serial: s.header.Serial, Total: s.header.Capacity, Remaining: s.header.Capacity - s.used, NativeLimit: s.header.Capacity, Position: &p, Sampled: time.Now().UTC().Format(time.RFC3339Nano), MaxRecord: format.BlockSize, Compression: s.header.Compress, Encryption: true}, nil
}
func (s *Sim) Position() (uint64, error) { return s.pos, nil }
func (s *Sim) Locate(p uint64) error {
	if p > uint64(len(s.records)) {
		return errors.New("locate beyond EOD")
	}
	s.pos = p
	return nil
}
func (s *Sim) End() error { s.pos = uint64(len(s.records)); return nil }
func fingerprint(k []byte) (x [16]byte) {
	if len(k) == 0 {
		return
	}
	h := sha256.Sum256(k)
	copy(x[:], h[:16])
	return
}
func KeyID(k []byte) string { x := fingerprint(k); return hex.EncodeToString(x[:]) }
func (s *Sim) SetKey(k []byte) error {
	if len(k) != 0 && len(k) != 32 {
		return errors.New("key must be 32 bytes")
	}
	s.key = fingerprint(k)
	return nil
}
func (s *Sim) SetCompression(enabled bool) error {
	if !s.writable {
		return errors.New("read-only simulator cannot change compression")
	}
	s.header.Compress = enabled
	return nil
}
func (s *Sim) ReadRecord() ([]byte, error) {
	b := make([]byte, format.BlockSize)
	n, e := s.ReadRecordInto(b)
	return b[:n], e
}
func (s *Sim) ReadRecordInto(b []byte) (int, error) {
	if s.pos == uint64(len(s.records)) {
		return 0, io.EOF
	}
	r := s.records[s.pos]
	s.pos++
	if r.kind == 1 {
		return 0, ErrFilemark
	}
	if r.key != ([16]byte{}) && r.key != s.key {
		return 0, errors.New("wrong simulator key")
	}
	if int(r.size) > len(b) {
		return 0, errors.New("record buffer is too small")
	}
	return s.f.ReadAt(b[:r.size], r.offset)
}
func (s *Sim) append(b []byte, kind uint32) error {
	if !s.writable {
		return errors.New("read-only simulator")
	}
	if s.torn {
		return errors.New("torn simulator tail: append refused")
	}
	if s.pos != uint64(len(s.records)) {
		return errors.New("append-only: not at EOD")
	}
	if len(b) > format.BlockSize {
		return errors.New("record too large")
	}
	cost := len(b) + 32
	if s.header.Compress && kind == 0 {
		var z bytes.Buffer // Simulated compression affects capacity, not address or record bytes.
		// RLE estimate is intentionally conservative; the backend models capacity only.
		for i := 0; i < len(b); {
			j := i + 1
			for j < len(b) && b[j] == b[i] && j-i < 255 {
				j++
			}
			if j-i > 3 {
				z.Write([]byte{0, byte(j - i), b[i]})
			} else {
				z.Write(b[i:j])
			}
			i = j
		}
		if z.Len() < len(b) {
			cost = z.Len() + 32
		}
	}
	if int64(cost) > s.header.Capacity-s.used {
		return errors.New("native EOM")
	}
	off, e := s.f.Seek(0, io.SeekEnd)
	if e != nil {
		return e
	}
	h := make([]byte, 32)
	binary.LittleEndian.PutUint32(h, uint32(len(b)))
	binary.LittleEndian.PutUint32(h[4:], kind)
	binary.LittleEndian.PutUint32(h[8:], crc32.Checksum(b, format.CRC))
	binary.LittleEndian.PutUint32(h[12:], uint32(cost))
	copy(h[16:], s.key[:])
	if _, e = s.f.Write(h); e != nil {
		s.torn = true
		return e
	}
	if _, e = s.f.Write(b); e != nil {
		s.torn = true
		return e
	}
	s.records = append(s.records, simRecord{off + 32, uint32(len(b)), kind, s.key})
	s.pos++
	s.used += int64(cost)
	return nil
}
func (s *Sim) WriteRecord(b []byte) error { return s.append(b, 0) }
func (s *Sim) Filemark() error {
	if e := s.append(nil, 1); e != nil {
		return e
	}
	return s.f.Sync()
}
func (s *Sim) Flush() error {
	if !s.writable {
		return errors.New("read-only simulator")
	}
	return s.f.Sync()
}
func (s *Sim) Eject() error {
	if s.f == nil {
		return errors.New("simulator is closed")
	}
	s.pos = 0
	return nil
}
func (s *Sim) Close() error { return s.f.Close() }
