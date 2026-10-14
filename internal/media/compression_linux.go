package media

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// MODE SENSE/SELECT configure the drive without reading or moving the tape.
// Only DCE changes; decompression, algorithms and buffered mode are preserved.
func (s *SG) SetCompression(enabled bool) error {
	if !s.write {
		return errors.New("read-only device cannot change compression")
	}
	return setCompression(s.command, enabled)
}

func readCompression(command func([]byte, []byte, bool) (int, error)) (header, page []byte, err error) {
	b := make([]byte, 64)
	n, err := command([]byte{0x5a, 8, 0x0f, 0, 0, 0, 0, 0, byte(len(b)), 0}, b, false)
	if err != nil {
		return nil, nil, fmt.Errorf("read compression mode page: %w", err)
	}
	if n < 8 || n > len(b) {
		return nil, nil, errors.New("truncated compression mode header")
	}
	end := int(binary.BigEndian.Uint16(b[:2])) + 2
	off := 8 + int(binary.BigEndian.Uint16(b[6:8]))
	if end > n || off+2 > end || b[off]&0x7f != 0x0f {
		return nil, nil, errors.New("invalid compression mode page")
	}
	length := 2 + int(b[off+1])
	if length < 16 || off+length > end {
		return nil, nil, errors.New("truncated compression mode page")
	}
	return b[:8], b[off : off+length], nil
}

func setCompression(command func([]byte, []byte, bool) (int, error), enabled bool) error {
	header, page, err := readCompression(command)
	if err != nil {
		return err
	}
	if enabled && page[2]&0x40 == 0 {
		return errors.New("drive does not support hardware compression")
	}
	if (page[2]&0x80 != 0) == enabled {
		return nil
	}
	parameters := make([]byte, 8+len(page))
	parameters[3] = header[3] & 0x7f // Preserve buffered mode/speed; omit write-protect.
	copy(parameters[8:], page)
	parameters[8] &= 0x7f // PS is read-only; do not request saving drive defaults.
	parameters[10] &^= 0x80
	if enabled {
		parameters[10] |= 0x80
	}
	cdb := []byte{0x55, 0x10, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(cdb[7:9], uint16(len(parameters)))
	n, err := command(cdb, parameters, true)
	if err != nil {
		return fmt.Errorf("set hardware compression: %w", err)
	}
	if n != len(parameters) {
		return errors.New("short compression mode select; not retried")
	}
	_, page, err = readCompression(command)
	if err != nil {
		return err
	}
	if (page[2]&0x80 != 0) != enabled {
		return errors.New("drive did not apply hardware compression setting")
	}
	return nil
}
