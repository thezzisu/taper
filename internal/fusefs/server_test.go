package fusefs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/thezzisu/taper/internal/engine"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
	"golang.org/x/sys/unix"
)

func fileFixture(t *testing.T) (*media.Image, *media.Sim, engine.Manifest, []byte) {
	t.Helper()
	dir := t.TempDir()
	rid, bid := format.NewID(), format.NewID()
	data := bytes.Repeat([]byte("0123456789abcdef"), 200000)
	meta, e := format.EncodeGroup(format.Group{Size: int64(len(data)), Paths: []string{"dir/data.bin"}})
	if e != nil {
		t.Fatal(e)
	}
	logical := append(append([]byte{}, meta...), data...)
	m := engine.Manifest{ID: bid, ReplicaID: rid.String(), StreamID: format.NewID().String(), Seq: 0, Name: "name", Logical: int64(len(logical)), Files: 1, BeginPos: 2, DataPos: 3, Blocks: uint64(1 + (len(logical)-1)/format.PayloadSize), Contents: []engine.Content{{Data: int64(len(meta)), Size: int64(len(data)), Files: []engine.File{{Path: "dir/data.bin"}}}}}
	m.CommitPos = m.DataPos + m.Blocks
	m.EndPos = m.CommitPos + 2
	image, e := media.OpenImage(filepath.Join(dir, "data.img"), true, rid.String(), "")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { image.Close() })
	tapePath := filepath.Join(dir, "tape")
	if e = media.CreateSim(tapePath, 8<<30, false); e != nil {
		t.Fatal(e)
	}
	tape, e := media.OpenSim(tapePath, true)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { tape.Close() })
	enc, e := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if e != nil {
		t.Fatal(e)
	}
	defer enc.Close()
	// Position fixture's DATA at the normal absolute addresses; read-through
	// tests remain inside DATA and need no speculative control-record reads.
	for p := uint64(0); p < m.DataPos; p++ {
		if e = tape.Filemark(); e != nil {
			t.Fatal(e)
		}
	}
	for seq := uint64(0); seq < m.Blocks; seq++ {
		start := int(seq) * format.PayloadSize
		end := min(start+format.PayloadSize, len(logical))
		frame, e := format.AppendFrame(nil, format.Frame{Kind: format.Data, Batch: bid, Seq: seq, First: format.NoEntry, Payload: logical[start:end]}, enc)
		if e != nil {
			t.Fatal(e)
		}
		if e = tape.WriteRecord(frame); e != nil {
			t.Fatal(e)
		}
	}
	return image, tape, m, data
}

type observed struct {
	media.Device
	mu             sync.Mutex
	reads, locates int
}

func (d *observed) ReadRecordInto(b []byte) (int, error) {
	d.mu.Lock()
	d.reads++
	d.mu.Unlock()
	return d.Device.ReadRecordInto(b)
}
func (d *observed) Locate(p uint64) error {
	d.mu.Lock()
	d.locates++
	d.mu.Unlock()
	return d.Device.Locate(p)
}
func TestReadThroughConcurrentMissAndOfflineErrors(t *testing.T) {
	img, tape, m, data := fileFixture(t)
	spy := &observed{Device: tape}
	// Cache first and last DATA blocks to bound the missing gap to its middle.
	buf := make([]byte, format.BlockSize)
	for _, p := range []uint64{m.DataPos, m.CommitPos - 1} {
		tape.Locate(p)
		n, e := tape.ReadRecordInto(buf)
		if e != nil {
			t.Fatal(e)
		}
		img.Locate(p)
		if e = img.WriteRecord(buf[:n]); e != nil {
			t.Fatal(e)
		}
	}
	r, e := NewReader(context.Background(), img, spy, []engine.BatchMeta{engine.Meta(m)}, 1, 8<<20)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for j := 0; j < 12; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out := make([]byte, 4096)
			off := int64(format.PayloadSize) + 100
			n, e := r.Read(context.Background(), engine.Meta(m), m.Contents[0].Data, m.Contents[0].Size, off, out)
			if e != nil {
				errs <- e
				return
			}
			if n != len(out) || !bytes.Equal(out, data[off:off+int64(n)]) {
				errs <- errors.New("wrong file bytes")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if spy.locates != 1 || spy.reads != 1 {
		t.Fatal("duplicate miss read tape twice", spy.locates, spy.reads)
	}
	offline, e := media.OpenImage(filepath.Join(t.TempDir(), "missing.img"), true, m.ReplicaID, "")
	if e != nil {
		t.Fatal(e)
	}
	defer offline.Close()
	rr, e := NewReader(context.Background(), offline, nil, nil, 16, 8<<20)
	if e != nil {
		t.Fatal(e)
	}
	defer rr.Close()
	if n, e := rr.Read(context.Background(), engine.Meta(m), m.Contents[0].Data, m.Contents[0].Size, 0, make([]byte, 100)); n != 0 || !errors.Is(e, media.ErrMissing) {
		t.Fatal("hole became EOF/zeros", n, e)
	}
}
func TestFuseWireReadOnlyTreeAndRanges(t *testing.T) {
	img, tape, m, data := fileFixture(t)
	buf := make([]byte, format.BlockSize)
	for p := m.DataPos; p < m.CommitPos; p++ {
		tape.Locate(p)
		n, e := tape.ReadRecordInto(buf)
		if e != nil {
			t.Fatal(e)
		}
		img.Locate(p)
		if e = img.WriteRecord(buf[:n]); e != nil {
			t.Fatal(e)
		}
	}
	r, e := NewReader(context.Background(), img, nil, nil, 16, 8<<20)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	tree, e := NewTree([]engine.Manifest{m}, r)
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	init := make([]byte, 16)
	native.PutUint32(init, 7)
	native.PutUint32(init[4:], 31)
	if out, errno, _ := tree.dispatch(ctx, 26, 0, init); errno != 0 || len(out) != 64 || native.Uint32(out) != 7 {
		t.Fatal("INIT", errno)
	}
	dir, errno, _ := tree.dispatch(ctx, 1, 1, []byte("dir\x00"))
	if errno != 0 {
		t.Fatal(errno)
	}
	dirID := native.Uint64(dir)
	file, errno, _ := tree.dispatch(ctx, 1, dirID, []byte("data.bin\x00"))
	if errno != 0 {
		t.Fatal(errno)
	}
	id := native.Uint64(file)
	req := make([]byte, 40)
	off := uint64(format.PayloadSize - 100)
	native.PutUint64(req[8:], off)
	native.PutUint32(req[16:], 4096)
	out, errno, _ := tree.dispatch(ctx, 15, id, req)
	if errno != 0 || !bytes.Equal(out, data[off:off+4096]) {
		t.Fatal("cross-record read", errno)
	}
	native.PutUint64(req[8:], uint64(len(data)))
	if out, errno, _ = tree.dispatch(ctx, 15, id, req); errno != 0 || len(out) != 0 {
		t.Fatal("true EOF", errno)
	}
	if _, errno, _ = tree.dispatch(ctx, 16, id, req); errno != unix.EROFS {
		t.Fatal("write allowed", errno)
	}
	if _, errno, _ = tree.dispatch(ctx, 15, id, req[:8]); errno != unix.EINVAL {
		t.Fatal("short read request accepted")
	}
	tree2, e := NewTree([]engine.Manifest{m, m}, r)
	if e == nil || tree2 != nil {
		t.Fatal("duplicate mounted archives accepted")
	}
}
func TestFuseProtocolTransport(t *testing.T) {
	tree, e := NewTree(nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	pair, e := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
	if e != nil {
		t.Fatal(e)
	}
	server := os.NewFile(uintptr(pair[0]), "fuse-test")
	client := os.NewFile(uintptr(pair[1]), "kernel-test")
	defer server.Close()
	defer client.Close()
	done := make(chan error, 1)
	go func() { done <- tree.serve(context.Background(), server) }()
	send := func(op uint32, payload []byte) {
		h := make([]byte, 40)
		native.PutUint32(h, uint32(40+len(payload)))
		native.PutUint32(h[4:], op)
		native.PutUint64(h[8:], 77)
		native.PutUint64(h[16:], 1)
		if _, e := client.Write(append(h, payload...)); e != nil {
			t.Fatal(e)
		}
	}
	init := make([]byte, 16)
	native.PutUint32(init, 7)
	native.PutUint32(init[4:], 31)
	send(26, init)
	reply := make([]byte, 256)
	n, e := client.Read(reply)
	if e != nil || n != 80 || binary.NativeEndian.Uint64(reply[8:]) != 77 {
		t.Fatal("wire reply", n, e)
	}
	send(38, nil)
	if e = <-done; e != nil {
		t.Fatal(e)
	}
}
