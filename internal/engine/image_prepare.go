package engine

import (
	"context"
	"errors"

	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/media"
)

func PrepareImageArchive(ctx context.Context, s *catalog.Store, img *media.Image, m Manifest, o WriteOptions) (int64, error) {
	if e := ValidateManifest(m); e != nil {
		return 0, e
	}
	if m.EndPos > media.ImageMaxObjects {
		return 0, errors.New("archive exceeds image bitmap capacity")
	}
	if img.ReplicaID != m.ReplicaID {
		return 0, errors.New("image belongs to another replica set")
	}
	var state string
	if e := s.DB.QueryRow("SELECT state FROM batches WHERE id=?", m.ID.String()).Scan(&state); e != nil {
		return 0, e
	}
	if img.HasRange(m.BeginPos, m.EndPos) {
		// Disk-only validation recovers a completed image after a catalog commit
		// interruption without rereading the original source or touching a tape.
		n, e := validateImageBatch(ctx, img, Meta(m), o.Memory)
		if e != nil {
			return 0, e
		}
		_, e = s.DB.Exec("UPDATE batches SET state='committed',encoded_bytes=? WHERE id=?", n, m.ID.String())
		return n, e
	}
	if state != "planned" {
		return 0, errors.New("image write was interrupted; inspect its local data before retrying")
	}
	for p := m.BeginPos; p < m.EndPos; p++ {
		if img.HasObject(p) {
			return 0, errors.New("image has a partial or conflicting batch")
		}
	}
	if e := checkSelectedFiles(m); e != nil {
		return 0, e
	}
	p, e := supply(ctx, m, o)
	if e != nil {
		return 0, e
	}
	defer p.Close()
	// Keep the reserved stream identity after any output, preventing encrypted
	// LBA reuse for different data after a failure or staging edit.
	if _, e = s.DB.Exec("UPDATE batches SET state='writing' WHERE id=?", m.ID.String()); e != nil {
		return 0, e
	}
	if e = img.Locate(m.BeginPos); e != nil {
		return 0, e
	}
	n, e := packWrite(p, img, m, o)
	if e != nil {
		return n, e
	}
	if e = img.Flush(); e != nil {
		return n, e
	}
	_, e = s.DB.Exec("UPDATE batches SET state='committed',encoded_bytes=? WHERE id=?", n, m.ID.String())
	return n, e
}
