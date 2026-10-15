// Package format defines the portable, record-oriented taper v1 format.
package format

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
)

const (
	BlockSize        = 1 << 20
	HeaderSize       = 64
	PayloadSize      = BlockSize - HeaderSize
	NoEntry          = ^uint32(0)
	Media       byte = 1
	Begin       byte = 2
	Data        byte = 3
	Commit      byte = 4
	MaxMetadata      = 256 << 20
)

var CRC = crc32.MakeTable(crc32.Castagnoli)
var magic = []byte("TAPERBLK")

type ID [16]byte

func NewID() ID {
	var id ID
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	id[6] = id[6]&15 | 64
	id[8] = id[8]&63 | 128
	return id
}
func (id ID) String() string { return hex.EncodeToString(id[:]) }
func ParseID(s string) (id ID, err error) {
	b, e := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if e != nil || len(b) != 16 {
		return id, errors.New("invalid UUID")
	}
	copy(id[:], b)
	return id, nil
}

type Frame struct {
	Kind    byte
	Batch   ID
	Seq     uint64
	First   uint32
	Payload []byte
}

// AppendFrame reuses dst. CRC covers uncompressed payload and the entire header.
func AppendFrame(dst []byte, f Frame, enc *zstd.Encoder) ([]byte, error) {
	if len(f.Payload) > PayloadSize || f.Kind < Media || f.Kind > Commit {
		return nil, errors.New("invalid frame")
	}
	if f.First != NoEntry && int(f.First) >= len(f.Payload) {
		return nil, errors.New("invalid entry offset")
	}
	dst = append(dst[:0], make([]byte, HeaderSize)...)
	codec := byte(0)
	if enc != nil && f.Kind == Data {
		dst = enc.EncodeAll(f.Payload, dst)
		if len(dst)-HeaderSize+16 < len(f.Payload) {
			codec = 1
		} else {
			dst = append(dst[:HeaderSize], f.Payload...)
		}
	} else {
		dst = append(dst, f.Payload...)
	}
	copy(dst, magic)
	binary.LittleEndian.PutUint16(dst[8:], 1)
	dst[10] = f.Kind
	dst[11] = codec
	copy(dst[16:], f.Batch[:])
	binary.LittleEndian.PutUint64(dst[32:], f.Seq)
	binary.LittleEndian.PutUint32(dst[40:], uint32(len(f.Payload)))
	binary.LittleEndian.PutUint32(dst[44:], uint32(len(dst)-HeaderSize))
	binary.LittleEndian.PutUint32(dst[48:], f.First)
	binary.LittleEndian.PutUint32(dst[52:], crc32.Checksum(f.Payload, CRC))
	binary.LittleEndian.PutUint32(dst[12:], crc32.Checksum(dst[:HeaderSize], CRC))
	return dst, nil
}

// ValidateHeader bounds a record before decoding or publishing image coverage.
func ValidateHeader(src []byte) error {
	if len(src) < HeaderSize || len(src) > BlockSize || !bytes.Equal(src[:8], magic) || binary.LittleEndian.Uint16(src[8:]) != 1 {
		return errors.New("invalid frame header")
	}
	var h [HeaderSize]byte
	copy(h[:], src[:HeaderSize])
	want := binary.LittleEndian.Uint32(h[12:])
	clear(h[12:16])
	if crc32.Checksum(h[:], CRC) != want {
		return errors.New("frame header CRC mismatch")
	}
	if binary.LittleEndian.Uint32(src[40:]) > PayloadSize || int(binary.LittleEndian.Uint32(src[44:])) != len(src)-HeaderSize {
		return errors.New("invalid frame lengths")
	}
	if src[10] < Media || src[10] > Commit || src[11] > 1 {
		return errors.New("invalid frame kind or codec")
	}
	return nil
}

func DecodeFrame(src []byte, dec *zstd.Decoder) (f Frame, err error) {
	if err = ValidateHeader(src); err != nil {
		return
	}

	f.Kind = src[10]
	if f.Kind < Media || f.Kind > Commit {
		return f, errors.New("unknown frame kind")
	}
	copy(f.Batch[:], src[16:32])
	f.Seq = binary.LittleEndian.Uint64(src[32:])
	f.First = binary.LittleEndian.Uint32(src[48:])
	raw := binary.LittleEndian.Uint32(src[40:])
	stored := binary.LittleEndian.Uint32(src[44:])
	if raw > PayloadSize || int(stored) != len(src)-HeaderSize {
		return f, errors.New("invalid frame lengths")
	}
	switch src[11] {
	case 0:
		f.Payload = src[HeaderSize:]
	case 1:
		if dec == nil {
			return f, errors.New("zstd decoder required")
		}
		f.Payload, err = dec.DecodeAll(src[HeaderSize:], make([]byte, 0, raw))
		if err != nil {
			return f, err
		}
	default:
		return f, errors.New("unknown codec")
	}
	if len(f.Payload) != int(raw) || crc32.Checksum(f.Payload, CRC) != binary.LittleEndian.Uint32(src[52:]) {
		return f, errors.New("payload CRC mismatch")
	}
	if f.First != NoEntry && int(f.First) >= len(f.Payload) {
		return f, errors.New("invalid first entry")
	}
	return f, nil
}

type Group struct {
	Size  int64
	Paths []string
}

func put(b []byte, n uint64) []byte { return binary.AppendUvarint(b, n) }
func EncodeGroup(g Group) ([]byte, error) {
	if g.Size < 0 || len(g.Paths) == 0 || len(g.Paths) > 1_000_000 {
		return nil, errors.New("invalid group")
	}
	b := put(nil, uint64(g.Size))
	b = put(b, uint64(len(g.Paths)))
	for _, p := range g.Paths {
		if err := ValidPath(p); err != nil {
			return nil, err
		}
		b = put(b, uint64(len(p)))
		b = append(b, p...)
		if len(b) > MaxMetadata {
			return nil, errors.New("group metadata limit exceeded")
		}
	}
	if len(b) > MaxMetadata {
		return nil, errors.New("group metadata limit exceeded")
	}
	return append(put(nil, uint64(len(b))), b...), nil
}
func ReadGroup(r *bytes.Reader) (g Group, n int64, err error) {
	start := r.Len()
	l, e := binary.ReadUvarint(r)
	if e != nil {
		return g, 0, e
	}
	if l > MaxMetadata || l > uint64(r.Len()) {
		return g, 0, errors.New("invalid metadata length")
	}
	body := make([]byte, l)
	io.ReadFull(r, body)
	br := bytes.NewReader(body)
	s, e := binary.ReadUvarint(br)
	if e != nil || s > 1<<63-1 {
		return g, 0, errors.New("invalid content size")
	}
	g.Size = int64(s)
	c, e := binary.ReadUvarint(br)
	if e != nil || c == 0 || c > 1_000_000 || c > uint64(br.Len()) {
		return g, 0, errors.New("invalid path count")
	}
	for i := uint64(0); i < c; i++ {
		l, e := binary.ReadUvarint(br)
		if e != nil || l > 65536 || l > uint64(br.Len()) {
			return g, 0, errors.New("invalid path length")
		}
		b := make([]byte, l)
		io.ReadFull(br, b)
		p := string(b)
		if e = ValidPath(p); e != nil {
			return g, 0, e
		}
		g.Paths = append(g.Paths, p)
	}
	if br.Len() != 0 {
		return g, 0, errors.New("trailing metadata bytes")
	}
	return g, int64(start - r.Len()), nil
}

func ValidPath(p string) error {
	if len(p) == 0 || len(p) > 65536 || !utf8.ValidString(p) || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return fmt.Errorf("invalid archive path %q", p)
	}
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." || s == ".." {
			return fmt.Errorf("unsafe path %q", p)
		}
	}
	return nil
}

// Normalize preserves native Linux names with reversible escaping, not cleaning.
func Normalize(p string) string {
	var b strings.Builder
	for len(p) > 0 {
		r, n := utf8.DecodeRuneInString(p)
		if r < 32 || r == 127 || r == '%' || r == '\\' || (r == utf8.RuneError && n == 1) {
			for _, c := range []byte(p[:n]) {
				fmt.Fprintf(&b, "%%%02X", c)
			}
		} else {
			b.WriteString(p[:n])
		}
		p = p[n:]
	}
	return b.String()
}
