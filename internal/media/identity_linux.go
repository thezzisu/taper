package media

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
)

func (s *SG) ReadIdentity() ([]byte, error) {
	c := make([]byte, 16)
	c[0] = 0x8c
	binary.BigEndian.PutUint16(c[8:10], 0x803)
	binary.BigEndian.PutUint32(c[10:14], 512)
	b := make([]byte, 512)
	n, e := s.command(c, b, false)
	if e != nil {
		return nil, e
	}
	if n < 9 || binary.BigEndian.Uint16(b[4:6]) != 0x803 {
		return nil, ErrIdentityAbsent
	}
	l := int(binary.BigEndian.Uint16(b[7:9]))
	if l > n-9 {
		return nil, errors.New("invalid MAM identity")
	}
	return bytes.TrimRight(b[9:9+l], "\x00"), nil
}
func (s *SG) WriteIdentity(value []byte) error {
	if !s.write {
		return errors.New("read-only device cannot update MAM")
	}
	b, e := identityParameters(value)
	if e != nil {
		return e
	}
	c := make([]byte, 16)
	c[0] = 0x8d
	binary.BigEndian.PutUint32(c[10:14], uint32(len(b)))
	_, e = s.command(c, b, true)
	return e
}

func identityParameters(value []byte) ([]byte, error) {
	if len(value) > 160 {
		return nil, errors.New("MAM identity limit exceeded")
	}
	// SSC attribute 0803h is a fixed 160-byte text field, padded with NULs.
	b := make([]byte, 4+5+160)
	binary.BigEndian.PutUint32(b, uint32(len(b)-4))
	binary.BigEndian.PutUint16(b[4:6], 0x803)
	b[6] = 2
	binary.BigEndian.PutUint16(b[7:9], 160)
	copy(b[9:], value)
	return b, nil
}
func (s *Sim) ReadIdentity() ([]byte, error) { return append([]byte(nil), s.header.Identity...), nil }
func (s *Sim) WriteIdentity(b []byte) error {
	if !s.writable {
		return errors.New("read-only simulator")
	}
	s.header.Identity = append([]byte(nil), b...)
	h, e := json.Marshal(s.header)
	if e != nil {
		return e
	}
	if len(h) > 4084 {
		return errors.New("identity too large")
	}
	out := make([]byte, 4096)
	copy(out, "TAPERSIM")
	binary.LittleEndian.PutUint32(out[8:], uint32(len(h)))
	copy(out[12:], h)
	if _, e = s.f.WriteAt(out, 0); e != nil {
		return e
	}
	return s.f.Sync()
}
