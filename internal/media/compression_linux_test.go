package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func compressionResponse(enabled bool) []byte {
	// MODE SENSE(10), including an eight-byte block descriptor. DCC, DDE,
	// algorithms, buffered mode and speed must survive a change to DCE.
	b := []byte{0, 30, 0, 0x92, 0, 0, 0, 8,
		0, 0, 0, 0, 0, 0, 0, 0,
		0x8f, 14, 0x40, 0x80, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 0}
	if enabled {
		b[18] |= 0x80
	}
	return b
}

func TestCompressionPreservesDriveSettingsAndChecksResult(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		response := compressionResponse(!enabled)
		reads, writes := 0, 0
		command := func(cdb, data []byte, out bool) (int, error) {
			switch cdb[0] {
			case 0x5a:
				reads++
				if out || cdb[2] != 0x0f {
					t.Fatal("not a compression-mode query")
				}
				return copy(data, response), nil
			case 0x55:
				writes++
				if !out || cdb[1] != 0x10 || int(binary.BigEndian.Uint16(cdb[7:9])) != len(data) {
					t.Fatal("invalid mode select or request to save drive defaults")
				}
				if !bytes.Equal(data[:8], []byte{0, 0, 0, 0x12, 0, 0, 0, 0}) || data[8] != 0x0f {
					t.Fatal("mode header changes buffered mode/speed or retains read-only fields")
				}
				if (data[10]&0x80 != 0) != enabled || data[10]&0x40 == 0 || !bytes.Equal(data[11:], response[19:]) {
					t.Fatal("compression change altered decompression or algorithm settings")
				}
				copy(response[16:], data[8:])
				return len(data), nil
			default:
				t.Fatalf("unexpected SCSI command: %x", cdb)
				return 0, nil
			}
		}
		if e := setCompression(command, enabled); e != nil {
			t.Fatal(e)
		}
		if reads != 2 || writes != 1 {
			t.Fatal("setting was not checked once", reads, writes)
		}
		if e := setCompression(command, enabled); e != nil || writes != 1 || reads != 3 {
			t.Fatal("matching setting was unnecessarily changed", reads, writes, e)
		}
	}
}

func TestCompressionRejectsMalformedOrUnappliedSettings(t *testing.T) {
	for _, fault := range []string{"short-header", "short-page", "wrong-page", "unsupported", "query-error", "select-error", "short-select", "ignored-setting"} {
		t.Run(fault, func(t *testing.T) {
			response := compressionResponse(false)
			switch fault {
			case "short-header":
				response = response[:7]
			case "short-page":
				response = response[:25]
			case "wrong-page":
				response[16] = 0x10
			case "unsupported":
				response[18] = 0
			}
			writes := 0
			e := setCompression(func(cdb, data []byte, out bool) (int, error) {
				if cdb[0] == 0x5a {
					if fault == "query-error" {
						return 0, errors.New("query failed")
					}
					return copy(data, response), nil
				}
				writes++
				if fault == "select-error" {
					return 0, errors.New("select failed")
				}
				if fault == "short-select" {
					return len(data) - 1, nil
				}
				return len(data), nil // Drive deliberately leaves DCE unchanged.
			}, true)
			if e == nil || writes > 1 {
				t.Fatal("invalid setting accepted or retried", writes, e)
			}
		})
	}
}
