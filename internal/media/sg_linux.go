package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"github.com/thezzisu/taper/internal/format"
	"golang.org/x/sys/unix"
)

type sgHeader struct {
	Interface int32
	Direction int32
	CmdLen    uint8
	SenseMax  uint8
	Iovec     uint16
	Length    uint32
	Data      uintptr
	Cmd       uintptr
	Sense     uintptr
	Timeout   uint32
	Flags     uint32
	Pack      int32
	User      uintptr
	Status    uint8
	Masked    uint8
	Message   uint8
	SenseLen  uint8
	Host      uint16
	Driver    uint16
	Resid     int32
	Duration  uint32
	Info      uint32
}
type SG struct {
	f             *os.File
	write         bool
	locked        bool
	algorithm     byte
	haveAlgorithm bool
	managed       bool
}
type senseError struct {
	key, asc, ascq byte
	mark, eom, ili bool
	diff           int32
	valid          bool
}

func (e *senseError) Error() string {
	return fmt.Sprintf("SCSI sense key=%x ASC=%02x/%02x filemark=%t EOM=%t ILI=%t", e.key, e.asc, e.ascq, e.mark, e.eom, e.ili)
}
func parseSense(b []byte) *senseError {
	e := &senseError{}
	if len(b) < 3 {
		return e
	}
	switch b[0] & 127 {
	case 0x70, 0x71:
		e.key = b[2] & 15
		e.mark = b[2]&128 != 0
		e.eom = b[2]&64 != 0
		e.ili = b[2]&32 != 0
		if len(b) >= 7 {
			e.valid = b[0]&128 != 0
			e.diff = int32(binary.BigEndian.Uint32(b[3:7]))
		}
		if len(b) >= 14 {
			e.asc = b[12]
			e.ascq = b[13]
		}
	case 0x72, 0x73:
		e.key = b[1] & 15
		e.asc = b[2]
		if len(b) > 3 {
			e.ascq = b[3]
		}
		for off := 8; off+2 <= len(b); {
			end := off + 2 + int(b[off+1])
			if end > len(b) {
				break
			}
			if b[off] == 4 && end >= off+4 {
				v := b[off+3]
				e.mark = v&128 != 0
				e.eom = v&64 != 0
				e.ili = v&32 != 0
			}
			off = end
		}
	}
	return e
}
func OpenSG(name string, write bool) (*SG, error) {
	resolved, e := filepath.EvalSymlinks(name)
	if e != nil {
		return nil, e
	}
	name = resolved
	if filepath.Dir(name) != "/dev" || !strings.HasPrefix(filepath.Base(name), "sg") {
		return nil, errors.New("SG backend requires a /dev/sgN device")
	}
	base := filepath.Base(name)
	typ, e := os.ReadFile("/sys/class/scsi_generic/" + base + "/device/type")
	if e != nil || strings.TrimSpace(string(typ)) != "1" {
		return nil, errors.New("SG backend requires a /dev/sgN sequential-access device")
	}
	f, e := os.OpenFile(name, os.O_RDWR, 0)
	if e != nil {
		return nil, e
	}
	st, e := f.Stat()
	if e != nil {
		f.Close()
		return nil, e
	}
	if st.Mode()&os.ModeCharDevice == 0 {
		f.Close()
		return nil, errors.New("SG device is not a character device")
	}
	var stat unix.Stat_t
	if e = unix.Fstat(int(f.Fd()), &stat); e != nil {
		f.Close()
		return nil, e
	}
	device, e := os.ReadFile("/sys/class/scsi_generic/" + base + "/dev")
	if e != nil || strings.TrimSpace(string(device)) != fmt.Sprintf("%d:%d", unix.Major(stat.Rdev), unix.Minor(stat.Rdev)) {
		f.Close()
		return nil, errors.New("device node identity does not match the SCSI tape")
	}
	if e = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		f.Close()
		return nil, e
	}
	return &SG{f: f, write: write, locked: true}, nil
}
func (s *SG) command(cdb, data []byte, out bool) (int, error) {
	if len(cdb) == 0 {
		return 0, errors.New("empty CDB")
	}
	sense := make([]byte, 64)
	h := sgHeader{Interface: 'S', Direction: -1, CmdLen: uint8(len(cdb)), SenseMax: 64, Cmd: uintptr(unsafe.Pointer(&cdb[0])), Sense: uintptr(unsafe.Pointer(&sense[0])), Timeout: 900000}
	if len(data) > 0 {
		h.Length = uint32(len(data))
		h.Data = uintptr(unsafe.Pointer(&data[0]))
		h.Direction = -3
		if out {
			h.Direction = -2
		}
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, s.f.Fd(), 0x2285, uintptr(unsafe.Pointer(&h)))
	runtime.KeepAlive(cdb)
	runtime.KeepAlive(data)
	runtime.KeepAlive(sense)
	if errno != 0 {
		return 0, errno
	}
	if h.Host != 0 {
		return 0, fmt.Errorf("SCSI host status %x", h.Host)
	}
	n := len(data) - int(h.Resid)
	if n < 0 || n > len(data) {
		return 0, errors.New("invalid SCSI residual")
	}
	if h.Status != 0 || h.Driver&15 != 0 {
		return n, parseSense(sense[:h.SenseLen])
	}
	return n, nil
}
func (s *SG) Position() (uint64, error) {
	b := make([]byte, 32)
	_, e := s.command([]byte{0x34, 6, 0, 0, 0, 0, 0, 0, 0, 0}, b, false)
	if e != nil {
		return 0, e
	}
	if b[0]&4 != 0 {
		return 0, errors.New("logical position unknown")
	}
	if binary.BigEndian.Uint32(b[4:8]) != 0 {
		return 0, errors.New("only partition 0 supported")
	}
	return binary.BigEndian.Uint64(b[8:16]), nil
}
func (s *SG) Locate(pos uint64) error {
	c := make([]byte, 16)
	c[0] = 0x92
	binary.BigEndian.PutUint64(c[4:12], pos)
	_, e := s.command(c, nil, false)
	return e
}
func (s *SG) End() error { _, e := s.command([]byte{0x11, 3, 0, 0, 0, 0}, nil, false); return e }
func (s *SG) ReadRecord() ([]byte, error) {
	b := make([]byte, format.BlockSize)
	c := []byte{8, 0, 0, 0, 0, 0}
	c[2] = byte(len(b) >> 16)
	c[3] = byte(len(b) >> 8)
	c[4] = byte(len(b))
	n, e := s.command(c, b, false)
	if se, ok := e.(*senseError); ok {
		if se.mark {
			return nil, ErrFilemark
		}
		if se.key == 8 || (se.asc == 0 && se.ascq == 5) {
			return nil, ErrEnd
		}
		if se.key == 0 && se.ili {
			if se.valid {
				if se.diff < 0 {
					return nil, errors.New("tape record exceeds supported size")
				}
				n = len(b) - int(se.diff)
			}
			if n < 0 || n > len(b) {
				return nil, errors.New("invalid tape record length")
			}
			return b[:n], nil
		}
	}
	return b[:n], e
}
func (s *SG) WriteRecord(b []byte) error {
	if !s.write {
		return errors.New("read-role worker cannot write")
	}
	if len(b) == 0 || len(b) > format.BlockSize {
		return errors.New("invalid record size")
	}
	c := []byte{0x0a, 0, byte(len(b) >> 16), byte(len(b) >> 8), byte(len(b)), 0}
	n, e := s.command(c, b, true)
	if e == nil && n != len(b) {
		e = errors.New("short tape write; not retried")
	}
	return e
}
func (s *SG) Filemark() error {
	if !s.write {
		return errors.New("read-role worker cannot write filemarks")
	}
	_, e := s.command([]byte{0x10, 0, 0, 0, 1, 0}, nil, false)
	return e
}
func (s *SG) security(page uint16) ([]byte, error) {
	c := make([]byte, 12)
	c[0] = 0xa2
	c[1] = 0x20
	binary.BigEndian.PutUint16(c[2:4], page)
	binary.BigEndian.PutUint32(c[6:10], 8192)
	b := make([]byte, 8192)
	n, e := s.command(c, b, false)
	return b[:n], e
}
func (s *SG) SetKey(key []byte) error {
	if len(key) != 0 && len(key) != 32 {
		return errors.New("key must be 32 bytes")
	}
	if len(key) > 0 && !s.haveAlgorithm {
		b, e := s.security(0x10)
		if e != nil {
			return e
		}
		s.algorithm, s.haveAlgorithm = selectHardwareAlgorithm(b)
		if !s.haveAlgorithm {
			return errors.New("AES-256 hardware algorithm not found")
		}
	}
	b := make([]byte, 20+len(key))
	binary.BigEndian.PutUint16(b, 0x10)
	binary.BigEndian.PutUint16(b[2:4], uint16(len(b)-4))
	b[4] = 0x40 // all I_T nexuses, matching SSC application-managed key scope
	b[5] = 0x44 // application-managed mode; clear on medium removal
	if len(key) > 0 {
		if s.write {
			b[6] = 2
		}
		b[7] = 3
		b[8] = s.algorithm
	}
	binary.BigEndian.PutUint16(b[18:20], uint16(len(key)))
	copy(b[20:], key)
	defer clear(b)
	c := make([]byte, 12)
	c[0] = 0xb5
	c[1] = 0x20
	c[3] = 0x10
	binary.BigEndian.PutUint32(c[6:10], uint32(len(b)))
	_, e := s.command(c, b, true)
	if e == nil {
		s.managed = true
	}
	return e
}

func selectHardwareAlgorithm(b []byte) (byte, bool) {
	for off := 20; off+24 <= len(b); {
		length := int(binary.BigEndian.Uint16(b[off+2:off+4])) + 4
		if length < 24 || off+length > len(b) {
			break
		}
		if binary.BigEndian.Uint16(b[off+10:off+12]) == 32 {
			return b[off], true
		}
		off += length
	}
	return 0, false
}
func (s *SG) Info() (info Info, err error) {
	b := make([]byte, 6)
	if _, err = s.command([]byte{5, 0, 0, 0, 0, 0}, b, false); err != nil {
		return
	}
	info.MaxRecord = int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	c := make([]byte, 16)
	c[0] = 0x8c
	binary.BigEndian.PutUint32(c[10:14], 8192)
	b = make([]byte, 8192)
	n, err := s.command(c, b, false)
	if err != nil {
		return info, err
	}
	b = b[:n]
	for off := 4; off+5 <= len(b); {
		id := binary.BigEndian.Uint16(b[off:])
		l := int(binary.BigEndian.Uint16(b[off+3:]))
		if off+5+l > len(b) {
			return info, errors.New("invalid MAM response")
		}
		v := b[off+5 : off+5+l]
		switch id {
		case 0:
			if l == 8 {
				info.Remaining = int64(binary.BigEndian.Uint64(v)) << 20
			}
		case 1:
			if l == 8 {
				info.Total = int64(binary.BigEndian.Uint64(v)) << 20
			}
		case 0x401:
			info.Serial = strings.TrimSpace(string(v))
		case 0x806:
			info.Barcode = strings.TrimSpace(string(v))
		}
		off += 5 + l
	}
	if info.Total <= 0 || info.Remaining < 0 || info.Serial == "" {
		return info, errors.New("missing MAM capacity/serial")
	}
	rawTotal := info.Total
	info.Total = min(rawTotal, 2_500_000_000_000)
	info.Remaining = min(info.Remaining, rawTotal)
	hi, lo := bits.Mul64(uint64(info.Remaining), uint64(info.Total))
	q, _ := bits.Div64(hi, lo, uint64(rawTotal))
	info.Remaining = int64(q)
	b = make([]byte, 64)
	n, e := s.command([]byte{0x5a, 8, 0x0f, 0, 0, 0, 0, 0, 64, 0}, b, false)
	if e == nil && n >= 12 {
		off := 8 + int(binary.BigEndian.Uint16(b[6:8]))
		if off+3 <= n {
			info.Compression = b[off+2]&128 != 0
		}
	}
	caps, e := s.security(0x10)
	info.Encryption = e == nil && len(caps) >= 44
	logs := make([]byte, 1024)
	ln, e := s.command([]byte{0x4d, 0, 0x6e, 0, 0, 0, 0, 4, 0, 0}, logs, false)
	if e != nil {
		return info, fmt.Errorf("TapeAlert query failed: %w", e)
	}
	for off := 4; off+4 <= ln; {
		length := int(logs[off+3])
		if off+4+length > ln {
			return info, errors.New("invalid TapeAlert response")
		}
		if length > 0 && logs[off+4] != 0 {
			info.Alerts = append(info.Alerts, int(binary.BigEndian.Uint16(logs[off:off+2])))
		}
		off += 4 + length
	}
	return info, nil
}
func (s *SG) Close() error {
	if s.f == nil {
		return nil
	}
	var e error
	if s.managed {
		e = s.SetKey(nil)
	}
	ce := s.f.Close()
	s.f = nil
	if e != nil {
		return e
	}
	return ce
}
