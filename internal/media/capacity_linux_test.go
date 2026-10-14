package media

import (
	"encoding/binary"
	"math"
	"testing"
)

func capacityResponse(total, remaining uint64) []byte {
	b := make([]byte, 4)
	for id, value := range []uint64{remaining, total} {
		attribute := make([]byte, 13)
		binary.BigEndian.PutUint16(attribute, uint16(id))
		binary.BigEndian.PutUint16(attribute[3:], 8)
		binary.BigEndian.PutUint64(attribute[5:], value)
		b = append(b, attribute...)
	}
	b = append(b, 4, 1, 1, 0, 4, 't', 'e', 's', 't')
	b = append(b, 0, 6, 0, 0, 1, 0x5a)
	binary.BigEndian.PutUint32(b, uint32(len(b)-4))
	return b
}

func TestCapacityPreservesUsedBytesNearTapeEnd(t *testing.T) {
	// Reproduce the HP LTO-6 boundary discrepancy: fractional scaling reports
	// 268 GB where preserving already consumed bytes leaves only 160 GB.
	for _, test := range []struct {
		remaining uint64
		want      int64
	}{
		{2499053, 2_500_000_000_000},
		{267911, 160_478_046_208},
		{110320, 0},
		{0, 0},
	} {
		var info Info
		if err := mamInfo(capacityResponse(2499053, test.remaining), &info); err != nil {
			t.Fatal(err)
		}
		limit, available, err := info.Budget()
		if err != nil || limit != 2_500_000_000_000 || available != test.want || info.Total != 2499053<<20 || info.Remaining != int64(test.remaining)<<20 {
			t.Fatalf("remaining count %d: %+v", test.remaining, info)
		}
		if test.remaining == 267911 && 241_830_882_964 <= available-25_000_000_000 {
			t.Fatal("over-capacity archive would be admitted")
		}
	}
	var small Info
	if err := mamInfo(capacityResponse(1000, 100), &small); err != nil || small.Total != 1000<<20 || small.Remaining != 100<<20 {
		t.Fatal("capacity below native ceiling changed", small, err)
	}
	small.EarlyWarning = true
	_, available, err := small.Budget()
	if err != nil || available != 0 || small.Remaining != 100<<20 {
		t.Fatal("EOP overrode measurement or not budget", small, available, err)
	}
	small.NativeLimit = 0
	if _, _, err := small.Budget(); err == nil {
		t.Fatal("unknown native limit silently guessed")
	}
}

func TestCapacityRejectsMissingInvalidOrTruncatedAttributes(t *testing.T) {
	valid := capacityResponse(1000, 100)
	missingRemaining := append([]byte{}, valid[:4]...)
	missingRemaining = append(missingRemaining, valid[17:]...)
	binary.BigEndian.PutUint32(missingRemaining, uint32(len(missingRemaining)-4))
	duplicate := append(append([]byte{}, valid...), valid[4:17]...)
	binary.BigEndian.PutUint32(duplicate, uint32(len(duplicate)-4))
	for _, data := range [][]byte{nil, valid[:3], valid[:len(valid)-1], missingRemaining, duplicate, capacityResponse(0, 0), capacityResponse(100, 101), capacityResponse(math.MaxUint64, 0)} {
		if err := mamInfo(data, &Info{}); err == nil {
			t.Fatal("invalid capacity accepted", data)
		}
	}
}
