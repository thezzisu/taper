package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
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
	algorithm     byte
	haveAlgorithm bool
	managed       bool
}
type senseError struct {
	key, asc, ascq         byte
	mark, eom, ili         bool
	diff                   int64
	valid                  bool
	deferred               bool
	opcode                 byte
	requested, transferred int
	residual               int32
	raw                    []byte
}

func (e *senseError) Error() string {
	return fmt.Sprintf("SCSI opcode=%02x sense key=%x ASC=%02x/%02x filemark=%t EOM=%t ILI=%t deferred=%t information_valid=%t information=%d requested=%d transferred=%d residual=%d raw_sense=%x", e.opcode, e.key, e.asc, e.ascq, e.mark, e.eom, e.ili, e.deferred, e.valid, e.diff, e.requested, e.transferred, e.residual, e.raw)
}
func parseSense(b []byte) *senseError {
	e := &senseError{raw: append([]byte(nil), b...)}
	if len(b) < 3 {
		return e
	}
	e.deferred = b[0]&127 == 0x71 || b[0]&127 == 0x73
	switch b[0] & 127 {
	case 0x70, 0x71:
		e.key = b[2] & 15
		e.mark = b[2]&128 != 0
		e.eom = b[2]&64 != 0
		e.ili = b[2]&32 != 0
		if len(b) >= 7 {
			e.valid = b[0]&128 != 0
			e.diff = int64(int32(binary.BigEndian.Uint32(b[3:7])))
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
			if b[off] == 0 && end >= off+12 && b[off+2]&128 != 0 {
				e.valid = true
				e.diff = int64(binary.BigEndian.Uint64(b[off+4 : off+12]))
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
	return &SG{f: f, write: write}, nil
}
func (s *SG) command(cdb, data []byte, out bool) (int, error) {
	if len(cdb) == 0 {
		return 0, errors.New("empty CDB")
	}
	release, e := protectTapeIO()
	if e != nil {
		return 0, e
	}
	defer release()
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
		return 0, fmt.Errorf("SCSI opcode=%02x host_status=%x driver_status=%x scsi_status=%x requested=%d residual=%d raw_sense=%x", cdb[0], h.Host, h.Driver, h.Status, len(data), h.Resid, sense[:h.SenseLen])
	}
	n := len(data) - int(h.Resid)
	if n < 0 || n > len(data) {
		return 0, errors.New("invalid SCSI residual")
	}
	if h.Status != 0 || h.Driver&15 != 0 {
		err := parseSense(sense[:h.SenseLen])
		err.opcode, err.requested, err.transferred, err.residual = cdb[0], len(data), n, h.Resid
		return n, err
	}
	return n, nil
}
func (s *SG) Position() (uint64, error) {
	b, e := s.positionData()
	if e != nil {
		return 0, e
	}
	if b[0]&4 != 0 {
		return 0, errors.New("logical position unknown")
	}
	return binary.BigEndian.Uint64(b[8:16]), nil
}
func (s *SG) positionData() ([]byte, error) {
	b := make([]byte, 32)
	n, e := s.command([]byte{0x34, 6, 0, 0, 0, 0, 0, 0, 0, 0}, b, false)
	if e != nil {
		return nil, e
	}
	if n != len(b) {
		return nil, errors.New("truncated READ POSITION response")
	}
	if binary.BigEndian.Uint32(b[4:8]) != 0 {
		return nil, errors.New("only partition 0 supported")
	}
	return b, nil
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
	n, e := s.ReadRecordInto(b)
	return b[:n], e
}
func (s *SG) ReadRecordInto(b []byte) (int, error) {
	if len(b) != format.BlockSize {
		return 0, errors.New("record buffer must be 1 MiB")
	}
	c := []byte{8, 0, 0, 0, 0, 0}
	c[2] = byte(len(b) >> 16)
	c[3] = byte(len(b) >> 8)
	c[4] = byte(len(b))
	n, e := s.command(c, b, false)
	return readResult(len(b), n, e)
}

func readResult(size, n int, e error) (int, error) {
	if se, ok := e.(*senseError); ok {
		if se.deferred {
			return n, e
		}
		if se.mark {
			return 0, ErrFilemark
		}
		if !se.mark && !se.ili && (se.key == 8 || se.key == 0) && se.asc == 0 && se.ascq == 5 {
			return 0, ErrEnd
		}
		if se.key == 0 && se.ili {
			if !se.valid {
				return 0, errors.New("tape record length unavailable in ILI sense")
			}
			if se.diff < 0 {
				return 0, errors.New("tape record exceeds supported size")
			}
			if se.diff > int64(size) {
				return 0, errors.New("invalid tape record length")
			}
			n = size - int(se.diff)
			if n < 0 || n > size {
				return 0, errors.New("invalid tape record length")
			}
			return n, nil
		}
	}
	return n, e
}
func (s *SG) WriteRecord(b []byte) error {
	if !s.write {
		return errors.New("read-only device cannot write")
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
		return errors.New("read-only device cannot write filemarks")
	}
	_, e := s.command([]byte{0x10, 0, 0, 0, 1, 0}, nil, false)
	return e
}

// WRITE FILEMARKS with count zero synchronously flushes buffered data without
// adding a tape object. It establishes a deliberate pause between write bursts.
func (s *SG) Flush() error {
	if !s.write {
		return errors.New("read-only device cannot flush writes")
	}
	_, e := s.command([]byte{0x10, 0, 0, 0, 0, 0}, nil, false)
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
	// SSC requires a supported algorithm index even when clearing the key.
	if !s.haveAlgorithm {
		b, e := s.security(0x10)
		if e != nil {
			return e
		}
		s.algorithm, s.haveAlgorithm = selectHardwareAlgorithm(b)
		if !s.haveAlgorithm {
			return errors.New("AES-256 hardware algorithm not found")
		}
	}
	b := encryptionParameters(key, s.algorithm, s.write)
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

func encryptionParameters(key []byte, algorithm byte, write bool) []byte {
	b := make([]byte, 20+len(key))
	binary.BigEndian.PutUint16(b, 0x10)
	binary.BigEndian.PutUint16(b[2:4], uint16(len(b)-4))
	b[4] = 0x40 // all I_T nexuses, matching SSC application-managed key scope
	b[5] = 0x44 // no external encryption mode check; clear on medium removal
	b[8] = algorithm
	if len(key) > 0 {
		if write {
			b[6] = 2
		}
		b[7] = 3
	}
	binary.BigEndian.PutUint16(b[18:20], uint16(len(key)))
	copy(b[20:], key)
	return b
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
	n, err := s.command([]byte{5, 0, 0, 0, 0, 0}, b, false)
	if err != nil {
		return
	}
	if n != len(b) {
		return info, errors.New("truncated READ BLOCK LIMITS response")
	}
	info.MaxRecord = int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	c := make([]byte, 16)
	c[0] = 0x8c
	binary.BigEndian.PutUint32(c[10:14], 8192)
	b = make([]byte, 8192)
	n, err = s.command(c, b, false)
	if err != nil {
		return info, err
	}
	if err = mamInfo(b[:n], &info); err != nil {
		return info, err
	}
	position, err := s.positionData()
	if err != nil {
		return info, err
	}
	info.EarlyWarning = position[0]&0x40 != 0
	info.PositionData = position
	if position[0]&4 == 0 {
		p := binary.BigEndian.Uint64(position[8:16])
		info.Position = &p
	}
	_, page, e := readCompression(s.command)
	if e != nil {
		return info, fmt.Errorf("compression query: %w", e)
	}
	info.Compression = page[2]&0x80 != 0
	caps, e := s.security(0x10)
	if e != nil {
		return info, fmt.Errorf("encryption capability query: %w", e)
	}
	if len(caps) < 20 || int(binary.BigEndian.Uint16(caps[2:4]))+4 != len(caps) {
		return info, errors.New("truncated encryption capability response")
	}
	_, info.Encryption = selectHardwareAlgorithm(caps)
	logs := make([]byte, 1024)
	ln, e := s.command([]byte{0x4d, 0, 0x6e, 0, 0, 0, 0, 4, 0, 0}, logs, false)
	if e != nil {
		return info, fmt.Errorf("TapeAlert query failed: %w", e)
	}
	if ln < 4 || logs[0]&0x3f != 0x2e {
		return info, errors.New("invalid TapeAlert log page")
	}
	end := 4 + int(binary.BigEndian.Uint16(logs[2:4]))
	if end > ln {
		return info, errors.New("truncated TapeAlert log page")
	}
	for off := 4; off < end; {
		if off+4 > end {
			return info, errors.New("truncated TapeAlert parameter")
		}
		length := int(logs[off+3])
		if off+4+length > end {
			return info, errors.New("invalid TapeAlert response")
		}
		if length > 0 && logs[off+4] != 0 {
			info.Alerts = append(info.Alerts, int(binary.BigEndian.Uint16(logs[off:off+2])))
		}
		off += 4 + length
	}
	info.Sampled = time.Now().UTC().Format(time.RFC3339Nano)
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

// Eject rewinds and unloads once. It deliberately does not issue a filemark or
// a second positioning command; the drive performs the physical unload.
func (s *SG) Eject() error {
	if s.f == nil {
		return errors.New("device is closed")
	}
	if s.managed {
		if e := s.SetKey(nil); e != nil {
			return e
		}
		s.managed = false
	}
	if _, e := s.command([]byte{0x01, 0, 0, 0, 0, 0}, nil, false); e != nil {
		return e
	}
	// LOAD/UNLOAD byte 4: LoEj=1, Load=0 requests an unload.
	_, e := s.command([]byte{0x1b, 0, 0, 0, 0x02, 0}, nil, false)
	return e
}
