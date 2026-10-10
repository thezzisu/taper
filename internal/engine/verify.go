package engine

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/klauspost/compress/zstd"
	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
	"time"
)

// VerifyFull checks controls, every packed sidecar, file hashes and the durable filemark.
// The device has already been unlocked by Restore.
func VerifyFull(ctx context.Context, s *catalog.Store, d media.Device, m Manifest, rep Replica) (reads, locates uint64, err error) {
	if err = d.Locate(rep.Begin); err != nil {
		return
	}
	locates = 1
	b, err := d.ReadRecord()
	if err != nil {
		return
	}
	f, err := format.DecodeFrame(b, nil)
	if err != nil {
		return
	}
	if f.Kind != format.Begin || f.Batch != m.ID {
		return 0, locates, errors.New("invalid BEGIN record")
	}
	var begin Boundary
	if err = json.Unmarshal(f.Payload, &begin); err != nil {
		return
	}
	if begin.Batch != m.ID.String() || begin.Replica != rep.ID || begin.Media != rep.Media || begin.Digest != m.Digest {
		return 0, locates, errors.New("replica BEGIN differs from catalog")
	}
	dec, e := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(4<<20), zstd.WithDecoderMaxWindow(format.BlockSize))
	if e != nil {
		return 0, locates, e
	}
	defer dec.Close()
	rebuilt, end, commit, e := readBatch(ctx, d, dec, begin)
	if e != nil {
		return 0, locates, e
	}
	if rebuilt.Digest != m.Digest || end.Blocks != rep.Blocks || commit != rep.Commit {
		return 0, locates, errors.New("replica layout differs from catalog")
	}
	reads = rep.Blocks + 2
	_, err = s.DB.Exec("UPDATE replicas SET verified=? WHERE id=?", time.Now().UTC().Format(time.RFC3339), rep.ID)
	return
}
