package media

import (
	"encoding/binary"
	"errors"
	"math"
	"strings"
)

// SSC MAM capacity attributes are MiB. Preserve both the response and the exact
// conversion; budget limits must never rewrite a reported measurement.
func mamInfo(b []byte, info *Info) error {
	if len(b) < 4 || uint64(binary.BigEndian.Uint32(b[:4]))+4 != uint64(len(b)) {
		return errors.New("invalid MAM response length")
	}
	var total, remaining bool
	for off := 4; off < len(b); {
		if off+5 > len(b) {
			return errors.New("truncated MAM attribute")
		}
		id := binary.BigEndian.Uint16(b[off:])
		n := int(binary.BigEndian.Uint16(b[off+3:]))
		if off+5+n > len(b) {
			return errors.New("truncated MAM attribute")
		}
		value := b[off+5 : off+5+n]
		switch id {
		case 0, 1:
			if n != 8 || binary.BigEndian.Uint64(value) > math.MaxInt64>>20 {
				return errors.New("invalid MAM capacity")
			}
			size := int64(binary.BigEndian.Uint64(value)) << 20
			if id == 0 {
				if remaining {
					return errors.New("duplicate MAM remaining capacity")
				}
				info.Remaining, remaining = size, true
			} else {
				if total {
					return errors.New("duplicate MAM total capacity")
				}
				info.Total, total = size, true
			}
		case 6:
			if n != 1 {
				return errors.New("invalid MAM density")
			}
			info.Density = value[0]
		case 0x401:
			info.Serial = strings.TrimSpace(string(value))
		case 0x806:
			info.Barcode = strings.TrimSpace(string(value))
		}
		off += 5 + n
	}
	if !total || !remaining || info.Total <= 0 || info.Serial == "" || info.Remaining > info.Total {
		return errors.New("missing or inconsistent MAM capacity/serial")
	}
	info.MAM = append([]byte(nil), b...)
	// Only the supported LTO-6 density has a documented native limit here.
	// Other densities remain visible but cannot be budgeted by this build.
	if info.Density == 0x5a {
		info.NativeLimit = 2_500_000_000_000
	}
	return nil
}
