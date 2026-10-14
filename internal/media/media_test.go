package media

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestSimulatorPositionsKeysAndEOM(t *testing.T) {
	name := filepath.Join(t.TempDir(), "test.tape")
	if e := CreateSim(name, 1024, false); e != nil {
		t.Fatal(e)
	}
	d, e := OpenSim(name, true)
	if e != nil {
		t.Fatal(e)
	}
	key := bytes.Repeat([]byte{1}, 32)
	d.SetKey(key)
	if e = d.WriteRecord([]byte("first")); e != nil {
		t.Fatal(e)
	}
	d.Filemark()
	d.WriteRecord([]byte("second"))
	if p, _ := d.Position(); p != 3 {
		t.Fatal(p)
	}
	if e = d.WriteRecord(make([]byte, 1024)); e == nil {
		t.Fatal("EOM ignored")
	}
	d.Locate(0)
	d.SetKey(nil)
	if _, e = d.ReadRecord(); e == nil {
		t.Fatal("wrong key accepted")
	}
	d.Locate(0)
	d.SetKey(key)
	b, e := d.ReadRecord()
	if e != nil || string(b) != "first" {
		t.Fatal(e)
	}
	if _, e = d.ReadRecord(); e != ErrFilemark {
		t.Fatal(e)
	}
	if e = d.WriteRecord([]byte("overwrite")); e == nil {
		t.Fatal("overwrite accepted")
	}
	d.Close()
	d, e = OpenSim(name, false)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	d.SetKey(key)
	d.Locate(2)
	b, e = d.ReadRecord()
	if e != nil || string(b) != "second" {
		t.Fatal(e)
	}
	if _, e = d.ReadRecord(); e != io.EOF {
		t.Fatal(e)
	}
}

func TestHPEncryptionIndexZero(t *testing.T) {
	b, e := hex.DecodeString("001000280900000000000000000000000000000000000014ba9c0020003c0020e90000000000000000010014")
	if e != nil {
		t.Fatal(e)
	}
	index, ok := selectHardwareAlgorithm(b)
	if !ok || index != 0 {
		t.Fatal("algorithm zero must be accepted", index, ok)
	}
	if _, ok = selectHardwareAlgorithm(b[:25]); ok {
		t.Fatal("truncated descriptor accepted")
	}
}
func TestSenseParsing(t *testing.T) {
	b := make([]byte, 18)
	b[0] = 0xf0
	b[2] = 0x20
	b[5] = 0x10
	b[6] = 0
	s := parseSense(b)
	if !s.ili || s.diff != 4096 || !s.valid {
		t.Fatal(s)
	}
	b[2] = 0x80
	s = parseSense(b)
	if !s.mark {
		t.Fatal(s)
	}
	b = make([]byte, 24)
	b[0] = 0x72
	b[7] = 16
	b[8] = 0
	b[9] = 10
	b[10] = 128
	binary.BigEndian.PutUint64(b[12:20], ^uint64(0))
	b[20] = 4
	b[21] = 2
	b[23] = 32
	s = parseSense(b)
	if !s.valid || !s.ili || s.diff != -1 {
		t.Fatal("descriptor overlength must not be silently truncated", s)
	}
}

func TestWriteEOMDiagnosticsRetainTransferOutcome(t *testing.T) {
	for _, response := range []byte{0x70, 0x71} {
		b := make([]byte, 18)
		b[0], b[2], b[7], b[13] = response|0x80, 0x40, 10, 2
		binary.BigEndian.PutUint32(b[3:7], 4096)
		err := parseSense(b)
		err.opcode, err.requested, err.transferred, err.residual = 0x0a, 1<<20, (1<<20)-4096, 4096
		if !err.eom || !err.valid || err.diff != 4096 || err.deferred != (response == 0x71) {
			t.Fatal("EOM outcome lost", err)
		}
		for _, field := range []string{"opcode=0a", "ASC=00/02", "information_valid=true", "information=4096", "requested=1048576", "transferred=1044480", "residual=4096"} {
			if !strings.Contains(err.Error(), field) {
				t.Fatal("missing diagnostic", field, err)
			}
		}
	}
	b := make([]byte, 8)
	b[0] = 0x73
	if !parseSense(b).deferred {
		t.Fatal("deferred descriptor sense lost")
	}
}
