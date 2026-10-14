package media

import "testing"

func TestReadEndRequiresExplicitCurrentEODSense(t *testing.T) {
	for _, key := range []byte{0, 8} {
		n, e := readResult(1<<20, 0, &senseError{key: key, ascq: 5})
		if e != ErrEnd || n != 0 {
			t.Fatal("EOD not recognized", n, e)
		}
	}
	for _, sense := range []*senseError{{key: 8}, {key: 8, ascq: 2, eom: true}, {key: 8, ascq: 5, deferred: true}, {key: 3, ascq: 5}, {key: 8, ascq: 5, ili: true}} {
		if _, e := readResult(1<<20, 0, sense); e == ErrEnd || e == nil {
			t.Fatal("non-EOD accepted", sense, e)
		}
	}
}
