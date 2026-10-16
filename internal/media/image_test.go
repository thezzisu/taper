package media

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thezzisu/taper/internal/format"
)

func imageFrame(t *testing.T, id format.ID, payload []byte) []byte {
	t.Helper()
	b, err := format.AppendFrame(nil, format.Frame{Kind: format.Data, Batch: id, Seq: 0, First: format.NoEntry, Payload: payload}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestImageRecordsFilemarksAndEncryption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.img")
	replica := format.NewID().String()
	key := bytes.Repeat([]byte{9}, 32)
	w, err := OpenImage(path, true, replica, KeyID(key))
	if err != nil {
		t.Fatal(err)
	}
	if err = w.SetKey(key); err != nil {
		t.Fatal(err)
	}
	batch := format.NewID()
	frame, err := format.AppendFrame(nil, format.Frame{Kind: format.Data, Batch: batch, Seq: 0, First: format.NoEntry, Payload: []byte("image data")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.WriteRecord(frame); err != nil {
		t.Fatal(err)
	}
	if err = w.Filemark(); err != nil {
		t.Fatal(err)
	}
	if !w.HasRange(0, 2) {
		t.Fatal("image coverage bitmap incorrect", w.state(0), w.state(1))
	}
	if w.HasObject(2) {
		t.Fatal("hole reported as present")
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := OpenImage(path, false, replica, KeyID(key))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err = r.SetKey(key); err != nil {
		t.Fatal(err)
	}
	got, err := r.ReadRecord()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatal("decrypted record differs")
	}
	if _, err = r.ReadRecord(); err != ErrFilemark {
		t.Fatal("filemark state not preserved", err)
	}
	if err = r.PunchHole(0, 2); err == nil {
		t.Fatal("read-only image punched a hole")
	}
}

func TestImageRejectsIdentityMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.img")
	w, err := OpenImage(path, true, format.NewID().String(), "")
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	if _, err = OpenImage(path, false, format.NewID().String(), ""); err == nil {
		t.Fatal("identity mismatch accepted")
	}
}

func TestImageHolesAreNotEODAndPublicationIsDurable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sparse.img")
	rid := format.NewID().String()
	img, e := OpenImage(file, true, rid, "")
	if e != nil {
		t.Fatal(e)
	}
	img.Locate(8)
	frame := imageFrame(t, format.NewID(), []byte("durable"))
	if e = img.WriteRecord(frame); e != nil {
		t.Fatal(e)
	}
	// Drop the process-owned state before Flush: a raw slot is not coverage.
	if e = img.f.Close(); e != nil {
		t.Fatal(e)
	}
	img.f = nil
	reopened, e := OpenImage(file, true, rid, "")
	if e != nil {
		t.Fatal(e)
	}
	if reopened.HasObject(8) {
		t.Fatal("unpublished slot accepted")
	}
	reopened.Locate(8)
	if e = reopened.WriteRecord(frame); e != nil {
		t.Fatal(e)
	}
	if e = reopened.Flush(); e != nil {
		t.Fatal(e)
	}
	reopened.Close()
	reopened, e = OpenImage(file, true, rid, "")
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	if !reopened.HasObject(8) {
		t.Fatal("durable record lost")
	}
	buf := make([]byte, format.BlockSize)
	if _, e = reopened.ReadAt(7, buf); !errors.Is(e, ErrMissing) {
		t.Fatal("hole reported as EOD", e)
	}
	if e = reopened.PunchHole(8, 9); e != nil {
		t.Fatal(e)
	}
	if _, e = reopened.ReadAt(8, buf); !errors.Is(e, ErrMissing) {
		t.Fatal("pruned record reported as valid or EOD", e)
	}
}
func TestImageCiphertextIdentityAndCorruption(t *testing.T) {
	file := filepath.Join(t.TempDir(), "encrypted.img")
	rid := format.NewID().String()
	key := bytes.Repeat([]byte{4}, 32)
	img, e := OpenImage(file, true, rid, KeyID(key))
	if e != nil {
		t.Fatal(e)
	}
	frame := imageFrame(t, format.NewID(), []byte("secret-data"))
	if e = img.SetKey(key); e != nil {
		t.Fatal(e)
	}
	if e = img.WriteRecord(frame); e != nil {
		t.Fatal(e)
	}
	if e = img.Close(); e != nil {
		t.Fatal(e)
	}
	all, e := os.ReadFile(file)
	if e != nil {
		t.Fatal(e)
	}
	raw := all[ImageHeaderSize : ImageHeaderSize+len(frame)]
	if bytes.Contains(raw, []byte("TAPERBLK")) || bytes.Contains(raw, []byte("secret-data")) || !bytes.Equal(raw[44:48], frame[44:48]) {
		t.Fatal("ciphertext layout is wrong")
	}
	if _, e = OpenImage(file, false, rid, ""); e == nil {
		t.Fatal("empty expected key ID accepted encrypted image")
	}
	img, e = OpenImage(file, false, rid, KeyID(key))
	if e != nil {
		t.Fatal(e)
	}
	if e = img.SetKey(bytes.Repeat([]byte{5}, 32)); e == nil {
		t.Fatal("wrong key accepted")
	}
	img.Close()
	f, e := os.OpenFile(file, os.O_RDWR, 0)
	if e != nil {
		t.Fatal(e)
	}
	raw[20] ^= 1
	f.WriteAt(raw, int64(ImageHeaderSize))
	f.Close()
	img, e = OpenImage(file, false, rid, KeyID(key))
	if e != nil {
		t.Fatal(e)
	}
	defer img.Close()
	img.SetKey(key)
	if _, e = img.ReadRecord(); e == nil {
		t.Fatal("damaged encrypted header accepted")
	}
}

func TestImageBufferedReadsSeeksAndSparseTails(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "encrypted"}[encrypted], func(t *testing.T) {
			key, keyID := []byte(nil), ""
			if encrypted {
				key = bytes.Repeat([]byte{7}, 32)
				keyID = KeyID(key)
			}
			file, rid := filepath.Join(t.TempDir(), "buffered.img"), format.NewID().String()
			img, err := OpenImage(file, true, rid, keyID)
			if err != nil {
				t.Fatal(err)
			}
			if err = img.SetKey(key); err != nil {
				t.Fatal(err)
			}
			frames := [][]byte{imageFrame(t, format.NewID(), bytes.Repeat([]byte{19}, format.PayloadSize)), imageFrame(t, format.NewID(), []byte("short"))}
			buf := make([]byte, format.BlockSize)
			for _, pos := range []uint64{3, 4, 5, 12, 2} {
				frame := frames[0]
				if pos == 5 {
					frame = frames[1]
				}
				before := append([]byte(nil), frame...)
				if err = img.Locate(pos); err != nil {
					t.Fatal(err)
				}
				if err = img.WriteRecord(frame); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(frame, before) {
					t.Fatal("writer mutated source frame")
				}
				n, e := img.ReadAt(pos, buf)
				if e != nil || !bytes.Equal(frame, buf[:n]) {
					t.Fatal("buffered read mismatch", pos, e)
				}
			}
			if err = img.Locate(13); err != nil {
				t.Fatal(err)
			}
			if err = img.Filemark(); err != nil {
				t.Fatal(err)
			}
			if err = img.Close(); err != nil {
				t.Fatal(err)
			}
			img, err = OpenImage(file, false, rid, keyID)
			if err != nil {
				t.Fatal(err)
			}
			defer img.Close()
			if err = img.SetKey(key); err != nil {
				t.Fatal(err)
			}
			for _, pos := range []uint64{2, 3, 4, 5, 12} {
				frame := frames[0]
				if pos == 5 {
					frame = frames[1]
				}
				n, e := img.ReadAt(pos, buf)
				if e != nil || !bytes.Equal(frame, buf[:n]) {
					t.Fatal("published read mismatch", pos, e)
				}
			}
			if _, e := img.ReadAt(8, buf); e != ErrMissing {
				t.Fatal("hole became data", e)
			}
			if _, e := img.ReadAt(13, buf); e != ErrFilemark {
				t.Fatal("filemark lost", e)
			}
			f, e := os.Open(file)
			if e != nil {
				t.Fatal(e)
			}
			defer f.Close()
			tail := make([]byte, 4096)
			if _, e = f.ReadAt(tail, slot(5)+4096); e != nil || !allZero(tail) {
				t.Fatal("short slot tail not sparse/zero", e)
			}
		})
	}
}

func TestImageDrainedRecordsRemainUnpublishedUntilFlush(t *testing.T) {
	file, rid := filepath.Join(t.TempDir(), "unpublished.img"), format.NewID().String()
	img, err := OpenImage(file, true, rid, "")
	if err != nil {
		t.Fatal(err)
	}
	frame := imageFrame(t, format.NewID(), make([]byte, format.PayloadSize))
	for i := 0; i < 7; i++ {
		if err = img.WriteRecord(frame); err != nil {
			t.Fatal(err)
		}
	}
	if st, e := img.f.Stat(); e != nil || st.Size() <= ImageHeaderSize {
		t.Fatal("buffer never drained", e)
	}
	if err = img.f.Close(); err != nil {
		t.Fatal(err)
	}
	img.f = nil
	reopened, err := OpenImage(file, false, rid, "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.MaxPos() != 0 || reopened.HasObject(0) {
		t.Fatal("unpublished data claimed durable coverage")
	}
}
