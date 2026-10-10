package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
)

type Replica struct {
	ID       string `json:"id"`
	Media    string `json:"media"`
	Label    string `json:"label"`
	Data     uint64 `json:"data_position"`
	Blocks   uint64 `json:"blocks"`
	Verified bool   `json:"verified"`
	Begin    uint64 `json:"begin_position"`
	Commit   uint64 `json:"commit_position"`
}

func Replicas(s *catalog.Store, batch string) (out []Replica, err error) {
	rows, err := s.DB.Query("SELECT r.id,r.media_id,m.label,r.data_pos,r.blocks,r.verified IS NOT NULL,r.begin_pos,r.commit_pos FROM replicas r JOIN media m ON m.id=r.media_id WHERE r.batch_id=? AND r.state='committed' ORDER BY r.verified IS NOT NULL DESC,m.state='open' DESC,r.id", batch)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var r Replica
		if err = rows.Scan(&r.ID, &r.Media, &r.Label, &r.Data, &r.Blocks, &r.Verified, &r.Begin, &r.Commit); err != nil {
			return
		}
		out = append(out, r)
	}
	err = rows.Err()
	return
}
func selected(c Content, prefixes []string) Content {
	out := c
	out.Files = nil
	for _, f := range c.Files {
		keep := len(prefixes) == 0
		for _, p := range prefixes {
			p = strings.TrimSuffix(p, "/")
			if f.Path == p || strings.HasPrefix(f.Path, p+"/") {
				keep = true
			}
		}
		if keep {
			out.Files = append(out.Files, f)
		}
	}
	return out
}

type BlockReader struct {
	d       media.Device
	batch   format.ID
	base    uint64
	dec     *zstd.Decoder
	seq     uint64
	payload []byte
	have    bool
	Reads   uint64
	Locates uint64
	MaxGap  uint64
	ctx     context.Context
}

func newReader(ctx context.Context, d media.Device, batch format.ID, base uint64) (*BlockReader, error) {
	dec, e := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(4<<20), zstd.WithDecoderMaxWindow(format.BlockSize))
	if e != nil {
		return nil, e
	}
	return &BlockReader{d: d, batch: batch, base: base, dec: dec, MaxGap: 256, ctx: ctx}, nil
}
func (r *BlockReader) block(seq uint64) ([]byte, error) {
	if e := r.ctx.Err(); e != nil {
		return nil, e
	}
	if r.have && seq == r.seq {
		return r.payload, nil
	}
	if !r.have || seq < r.seq || seq-r.seq > r.MaxGap {
		if e := r.d.Locate(r.base + seq); e != nil {
			return nil, e
		}
		r.Locates++
		r.have = false
	} else {
		for next := r.seq + 1; next < seq; next++ {
			if _, e := r.d.ReadRecord(); e != nil {
				return nil, e
			}
			r.Reads++
		}
	}
	b, e := r.d.ReadRecord()
	if e != nil {
		return nil, e
	}
	r.Reads++
	f, e := format.DecodeFrame(b, r.dec)
	if e != nil {
		return nil, e
	}
	if f.Kind != format.Data || f.Batch != r.batch || f.Seq != seq {
		return nil, errors.New("record identity mismatch")
	}
	r.seq = seq
	r.payload = f.Payload
	r.have = true
	return r.payload, nil
}
func (r *BlockReader) Copy(w io.Writer, offset, size int64) error {
	if offset < 0 || size < 0 {
		return errors.New("invalid extent")
	}
	for size > 0 {
		seq := uint64(offset / format.PayloadSize)
		in := int(offset % format.PayloadSize)
		b, e := r.block(seq)
		if e != nil {
			return e
		}
		if in >= len(b) {
			return errors.New("extent exceeds payload")
		}
		n := int64(min(int64(len(b)-in), size))
		written, we := w.Write(b[in : in+int(n)])
		if we != nil {
			return we
		}
		if written != int(n) {
			return io.ErrShortWrite
		}
		if e != nil {
			return e
		}
		offset += n
		size -= n
	}
	return nil
}

func Restore(ctx context.Context, s *catalog.Store, d media.Device, m Manifest, rep Replica, dest string, prefixes []string, key []byte, verifyOnly bool) (reads, locates uint64, err error) {
	if err = ValidateManifest(m); err != nil {
		return
	}
	selectedCount := 0
	for _, c := range m.Contents {
		selectedCount += len(selected(c, prefixes).Files)
	}
	if selectedCount == 0 {
		return 0, 0, errors.New("no files match the requested prefixes")
	}
	if rep.Blocks != uint64(1+(m.Logical-1)/format.PayloadSize) {
		return 0, 0, errors.New("replica length differs from manifest")
	}
	l, e := UnlockMedia(s, d, key)
	if e != nil {
		return 0, 0, e
	}
	if l.ID != rep.Media {
		return 0, 0, fmt.Errorf("insert tape %s (%s)", rep.Label, rep.Media)
	}
	if verifyOnly && len(prefixes) == 0 {
		return VerifyFull(ctx, s, d, m, rep)
	}
	r, e := newReader(ctx, d, m.ID, rep.Data)
	if e != nil {
		return 0, 0, e
	}
	defer r.dec.Close()
	defer func() { reads = r.Reads; locates = r.Locates }()
	var root *os.Root
	if !verifyOnly {
		if e = os.MkdirAll(dest, 0700); e != nil {
			return 0, 0, e
		}
		root, e = os.OpenRoot(dest)
		if e != nil {
			return 0, 0, e
		}
		defer root.Close()
		for _, c := range m.Contents {
			for _, f := range selected(c, prefixes).Files {
				if e = format.ValidPath(f.Path); e != nil {
					return 0, 0, e
				}
				if _, e = root.Lstat(f.Path); !os.IsNotExist(e) {
					return 0, 0, fmt.Errorf("target already exists or is inaccessible: %s", f.Path)
				}
			}
		}
	}
	for _, c := range m.Contents {
		c = selected(c, prefixes)
		if len(c.Files) == 0 {
			continue
		}
		h := sha256.New()
		var output *os.File
		writer := io.Writer(h)
		temp := ""
		primary := c.Files[0].Path
		if !verifyOnly {
			if e = root.MkdirAll(path.Dir(primary), 0700); e != nil {
				return 0, 0, e
			}
			temp = primary + ".taper-" + format.NewID().String() + ".tmp"
			output, e = root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				return 0, 0, e
			}
			writer = io.MultiWriter(h, output)
		}
		e = r.Copy(writer, c.Data, c.Size)
		if output != nil {
			if e == nil {
				e = output.Sync()
			}
			ce := output.Close()
			if e == nil {
				e = ce
			}
		}
		if e == nil && hex.EncodeToString(h.Sum(nil)) != c.Hash {
			e = fmt.Errorf("SHA-256 mismatch: %s", primary)
		}
		if e != nil {
			if root != nil {
				root.Remove(temp)
			}
			return 0, 0, e
		}
		if !verifyOnly {
			if e = root.Link(temp, primary); e != nil {
				root.Remove(temp)
				return 0, 0, e
			}
			root.Remove(temp)
			for _, f := range c.Files[1:] {
				if e = root.MkdirAll(path.Dir(f.Path), 0700); e != nil {
					return 0, 0, e
				}
				src, e := root.Open(primary)
				if e != nil {
					return 0, 0, e
				}
				aliasTemp := path.Join(path.Dir(f.Path), ".taper-"+format.NewID().String()+".tmp")
				dst, e := root.OpenFile(aliasTemp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if e != nil {
					src.Close()
					return 0, 0, e
				}
				_, e = io.Copy(dst, src)
				if e == nil {
					e = dst.Sync()
				}
				dst.Close()
				src.Close()
				if e != nil {
					root.Remove(aliasTemp)
					return 0, 0, e
				}
				if e = root.Link(aliasTemp, f.Path); e != nil {
					root.Remove(aliasTemp)
					return 0, 0, e
				}
				root.Remove(aliasTemp)
			}
		}
	}
	if verifyOnly && len(prefixes) == 0 {
		_, err = s.DB.Exec("UPDATE replicas SET verified=? WHERE id=?", time.Now().UTC().Format(time.RFC3339), rep.ID)
	}
	return
}

// Reindex streams each batch into hashes, without spooling its file contents.
// Incomplete batches never become committed backups. Corrupt batches are reported.
func Reindex(ctx context.Context, s *catalog.Store, d media.Device, key []byte) (imported int, err error) {
	l, err := ReadLabel(d)
	if err != nil {
		return
	}
	info, err := d.Info()
	if err != nil {
		return
	}
	if info.Serial != l.Serial {
		return 0, errors.New("media serial mismatch")
	}
	if l.KeyID != "" && media.KeyID(key) != l.KeyID {
		return 0, errors.New("matching recovery key required")
	}
	if err = d.SetKey(key); err != nil {
		return
	}
	if _, err = s.DB.Exec("INSERT INTO media VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET remaining=excluded.remaining,sampled=excluded.sampled", l.ID, l.Name, l.Serial, info.Total, info.Remaining, time.Now().UTC().Format(time.RFC3339), "open", l.KeyID, key); err != nil {
		return
	}
	dec, e := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(4<<20), zstd.WithDecoderMaxWindow(format.BlockSize))
	if e != nil {
		return 0, e
	}
	defer dec.Close()
	var issues []error
	for {
		if e = ctx.Err(); e != nil {
			return imported, e
		}
		pos, e := d.Position()
		if e != nil {
			return imported, e
		}
		b, e := d.ReadRecord()
		if e == io.EOF {
			return imported, errors.Join(issues...)
		}
		if e == media.ErrFilemark {
			continue
		}
		if e != nil {
			return imported, e
		}
		frame, e := format.DecodeFrame(b, dec)
		if e != nil {
			issues = append(issues, fmt.Errorf("corrupt record at %d: %w", pos, e))
			continue
		}
		if frame.Kind != format.Begin {
			continue
		}
		var begin Boundary
		if e = json.Unmarshal(frame.Payload, &begin); e != nil {
			issues = append(issues, e)
			continue
		}
		if begin.Media != l.ID {
			issues = append(issues, errors.New("batch on wrong medium"))
			continue
		}
		dataPos, e := d.Position()
		if e != nil {
			return imported, e
		}
		m, end, commitPos, e := readBatch(ctx, d, dec, begin)
		if e != nil {
			issues = append(issues, fmt.Errorf("batch %s not imported: %w", begin.Batch, e))
			continue
		}
		var exists int
		if e = s.DB.QueryRow("SELECT COUNT(*) FROM batches WHERE id=?", begin.Batch).Scan(&exists); e != nil {
			return imported, e
		}
		if exists == 0 {
			if e = Save(s, m); e != nil {
				return imported, e
			}
		} else {
			var digest string
			if e = s.DB.QueryRow("SELECT digest FROM batches WHERE id=?", begin.Batch).Scan(&digest); e != nil || digest != m.Digest {
				return imported, errors.New("existing batch differs")
			}
		}
		if _, e = s.DB.Exec("INSERT INTO replicas(id,batch_id,media_id,begin_pos,data_pos,commit_pos,blocks,state,verified) VALUES(?,?,?,?,?,?,?,'committed',?) ON CONFLICT(id) DO NOTHING", begin.Replica, begin.Batch, l.ID, pos, dataPos, commitPos, end.Blocks, time.Now().UTC().Format(time.RFC3339)); e != nil {
			return imported, e
		}
		if _, e = s.DB.Exec("UPDATE batches SET state='committed' WHERE id=?", begin.Batch); e != nil {
			return imported, e
		}
		imported++
	}
}
func readBatch(ctx context.Context, d media.Device, dec *zstd.Decoder, begin Boundary) (m Manifest, end Boundary, commitPos uint64, err error) {
	id, e := format.ParseID(begin.Batch)
	if e != nil {
		return m, end, 0, e
	}
	if begin.Logical < 0 || begin.Files < 0 {
		return m, end, 0, errors.New("invalid batch boundary")
	}
	m = Manifest{ID: id, Name: begin.Name, Created: time.Now().UTC().Format(time.RFC3339), Source: "reindexed", Digest: begin.Digest, Logical: begin.Logical, Files: begin.Files}
	dataPos, e := d.Position()
	if e != nil {
		return m, end, 0, e
	}
	stream := &batchStream{d: d, dec: dec, batch: id, ctx: ctx, position: dataPos}
	logical := int64(0)
	count := 0
	for logical < m.Logical {
		g, n, e := readStreamGroup(stream)
		if e != nil {
			return m, end, 0, e
		}
		c := Content{Size: g.Size, Meta: logical, Data: logical + n}
		if g.Size > m.Logical-c.Data {
			return m, end, 0, errors.New("content exceeds batch")
		}
		for _, p := range g.Paths {
			c.Files = append(c.Files, File{Path: p})
		}
		h := sha256.New()
		if _, e = io.CopyN(h, stream, g.Size); e != nil {
			return m, end, 0, e
		}
		c.Hash = hex.EncodeToString(h.Sum(nil))
		m.Contents = append(m.Contents, c)
		logical = c.Data + c.Size
		count += len(g.Paths)
	}
	if len(stream.pending) != 0 || count != m.Files {
		return m, end, 0, errors.New("trailing bytes or file-count mismatch")
	}
	commitPos, e = d.Position()
	if e != nil {
		return m, end, 0, e
	}
	b, e := d.ReadRecord()
	if e != nil {
		return m, end, commitPos, e
	}
	f, e := format.DecodeFrame(b, dec)
	if e != nil {
		return m, end, commitPos, e
	}
	if f.Kind == format.Begin {
		d.Locate(commitPos)
	}
	if f.Kind != format.Commit || f.Batch != id {
		return m, end, commitPos, errors.New("missing COMMIT")
	}
	if e = json.Unmarshal(f.Payload, &end); e != nil {
		return m, end, commitPos, e
	}
	expected := begin
	expected.Blocks = stream.seq
	if end != expected || Digest(m.Contents) != m.Digest {
		return m, end, commitPos, errors.New("commit digest mismatch")
	}
	if _, e = d.ReadRecord(); e != media.ErrFilemark {
		return m, end, commitPos, errors.New("missing durable filemark")
	}
	return m, end, commitPos, nil
}
