package format

import (
	"bytes"
	"github.com/klauspost/compress/zstd"
	"testing"
)

func TestFrameCorruption(t *testing.T) {
	b, e := AppendFrame(nil, Frame{Kind: Data, Batch: NewID(), First: 0, Payload: []byte("hello")}, nil)
	if e != nil {
		t.Fatal(e)
	}
	for i := range b {
		c := append([]byte(nil), b...)
		c[i] ^= 1
		if _, e = DecodeFrame(c, nil); e == nil {
			t.Fatalf("corruption at %d accepted", i)
		}
	}
}
func TestPaths(t *testing.T) {
	for _, p := range []string{"../evil", "/abs", "a/../b", "a\\b", "a//b", "a\x00b"} {
		if ValidPath(p) == nil {
			t.Fatal(p)
		}
	}
	for _, p := range []string{"a\\b", "a%b", "bad\xffname"} {
		if e := ValidPath(Normalize(p)); e != nil {
			t.Fatal(e)
		}
	}
}
func FuzzFrame(f *testing.F) {
	b, _ := AppendFrame(nil, Frame{Kind: Data, First: NoEntry, Payload: []byte("abc")}, nil)
	f.Add(b)
	d, _ := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(4<<20), zstd.WithDecoderMaxWindow(BlockSize))
	defer d.Close()
	f.Fuzz(func(t *testing.T, b []byte) { DecodeFrame(b, d) })
}
func FuzzGroup(f *testing.F) {
	b, _ := EncodeGroup(Group{Size: 10, Paths: []string{"a/b"}})
	f.Add(b)
	f.Fuzz(func(t *testing.T, b []byte) { ReadGroup(bytes.NewReader(b)) })
}
