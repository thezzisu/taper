package media

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

func TestEncryptionClearUsesMountedAlgorithm(t *testing.T) {
	// Recorded HP LTO-6 capabilities: only algorithm 1 is available.
	page, _ := hex.DecodeString("001000280900000000000000000000000000000001000014ba9c0020003c0020e90000000000000000010014")
	algorithm, ok := selectHardwareAlgorithm(page)
	if !ok || algorithm != 1 {
		t.Fatal("wrong mounted algorithm", algorithm, ok)
	}
	want, _ := hex.DecodeString("0010001040440000010000000000000000000000")
	if got := encryptionParameters(nil, algorithm, true); !bytes.Equal(got, want) {
		t.Fatalf("clear-key parameter page: %x", got)
	}
	for _, write := range []bool{false, true} {
		key := bytes.Repeat([]byte{0xa5}, 32)
		got := encryptionParameters(key, algorithm, write)
		mode := byte(0)
		if write {
			mode = 2
		}
		if len(got) != 52 || binary.BigEndian.Uint16(got[2:4]) != 48 || got[6] != mode || got[7] != 3 || got[8] != 1 || binary.BigEndian.Uint16(got[18:20]) != 32 || !bytes.Equal(got[20:], key) {
			t.Fatal("invalid key parameter page")
		}
	}
	if got := encryptionParameters(nil, 0, false); got[8] != 0 {
		t.Fatal("valid algorithm zero was replaced")
	}
}

func TestMAMIdentityFixedWidth(t *testing.T) {
	value := []byte(`taper1:["media","label","key-id"]`)
	b, err := identityParameters(value)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 169 || binary.BigEndian.Uint32(b[:4]) != 165 || binary.BigEndian.Uint16(b[4:6]) != 0x803 || b[6] != 2 || binary.BigEndian.Uint16(b[7:9]) != 160 {
		t.Fatal("invalid SSC user-label attribute")
	}
	if !bytes.Equal(bytes.TrimRight(b[9:], "\x00"), value) || !bytes.Equal(b[9+len(value):], make([]byte, 160-len(value))) {
		t.Fatal("identity was not NUL-padded")
	}
	if _, err = identityParameters(make([]byte, 161)); err == nil {
		t.Fatal("oversize identity accepted")
	}
}
