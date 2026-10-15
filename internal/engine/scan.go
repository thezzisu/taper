package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/policy"
)

type File struct {
	Path   string `json:"path"`
	Source string `json:"source"`
	MTime  int64  `json:"mtime"`
}
type Content struct {
	Size  int64  `json:"size"`
	Hash  string `json:"hash"`
	Meta  int64  `json:"meta"`
	Data  int64  `json:"data"`
	Files []File `json:"files"`
}
type Exclusion struct {
	Path   string
	Size   int64
	Reason string
}
type Manifest struct {
	ID             format.ID
	ReplicaID      string
	StreamID       string
	Seq            int
	BeginPos       uint64
	DataPos        uint64
	CommitPos      uint64
	Blocks         uint64
	EndPos         uint64
	EncodedBytes   *int64
	Name           string
	Source         string
	Created        string
	Rules          policy.Rules
	Contents       []Content
	Excluded       []Exclusion
	Logical        int64
	Files          int
	Digest         string
	SourceIdentity SourceIdentity
}
type Summary struct {
	Directory string `json:"directory"`
	Files     int    `json:"files"`
	Bytes     int64  `json:"bytes"`
}

type scanSink struct {
	ctx    context.Context
	w      io.Writer
	total  *int64
	notify func(int64)
}

func (s scanSink) Write(b []byte) (int, error) {
	if e := s.ctx.Err(); e != nil {
		return 0, e
	}
	n, e := s.w.Write(b)
	*s.total += int64(n)
	if s.notify != nil {
		s.notify(*s.total)
	}
	return n, e
}
func scanInventory(ctx context.Context, inventory Inventory, name string, rules policy.Rules, progress func(int64), cache *HashCache) (m Manifest, err error) {
	var readBytes int64
	root := inventory.Root
	if len(inventory.Issues) > 0 {
		return m, fmt.Errorf("source preflight: %d unsupported entries; first: %s", len(inventory.Issues), inventory.Issues[0].Path)
	}
	if err = rules.Validate(); err != nil {
		return
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return
	}
	m = Manifest{ID: format.NewID(), Name: name, Source: root, Created: time.Now().UTC().Format(time.RFC3339), Rules: rules}
	m.SourceIdentity = inventory.Identity
	m.Excluded = inventory.Excluded
	byHash := map[string]int{}
	buffer := make([]byte, 4<<20)
	secure, err := os.OpenRoot(root)
	if err != nil {
		return m, err
	}
	if err = CheckSource(m); err != nil {
		return m, err
	}
	defer secure.Close()
	visit := func(item InventoryFile) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		rel, canonical := item.Source, item.Path
		f, e := secure.Open(rel)
		if e != nil {
			return e
		}
		before, e := f.Stat()
		if e != nil {
			f.Close()
			return e
		}
		if before.Size() != item.Size || before.ModTime().UnixNano() != item.MTime {
			f.Close()
			return fmt.Errorf("source changed since selection; add it again: %s", canonical)
		}
		h := sha256.New()
		hash := ""
		hit := false
		if cache != nil {
			hash, hit, e = cache.Get(rel, before)
			if e != nil {
				f.Close()
				return e
			}
		}
		n := before.Size()
		if !hit {
			n, e = hashFile(ctx, f, before, h, scanSink{ctx: ctx, w: h, total: &readBytes, notify: progress}, buffer, cache, rel)
			if e != nil {
				f.Close()
				return e
			}
			if n != before.Size() {
				f.Close()
				return fmt.Errorf("source size changed during scan: %s", canonical)
			}
			hash = hex.EncodeToString(h.Sum(nil))
		}
		after, se := f.Stat()
		f.Close()
		if se != nil {
			return se
		}
		if !before.Mode().IsRegular() || n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
			return fmt.Errorf("source changed during scan: %s", canonical)
		}
		if cache != nil && !hit {
			if e = cache.Put(rel, before, hash); e != nil {
				return e
			}
		}
		key := fmt.Sprintf("%s:%d", hash, n)
		idx, ok := byHash[key]
		if !ok {
			idx = len(m.Contents)
			byHash[key] = idx
			m.Contents = append(m.Contents, Content{Size: n, Hash: hash})
		}
		m.Contents[idx].Files = append(m.Contents[idx].Files, File{canonical, rel, before.ModTime().UnixNano()})
		m.Files++
		return nil
	}
	for _, item := range inventory.Files {
		if err = visit(item); err != nil {
			return m, err
		}
	}
	if err = CheckSource(m); err != nil {
		return m, err
	}
	sort.Slice(m.Contents, func(i, j int) bool { return m.Contents[i].Files[0].Path < m.Contents[j].Files[0].Path })
	for i := range m.Contents {
		c := &m.Contents[i]
		sort.Slice(c.Files, func(i, j int) bool { return c.Files[i].Path < c.Files[j].Path })
		paths := []string{}
		for _, f := range c.Files {
			paths = append(paths, f.Path)
		}
		b, e := format.EncodeGroup(format.Group{Size: c.Size, Paths: paths})
		if e != nil {
			return m, e
		}
		c.Meta = m.Logical
		c.Data = m.Logical + int64(len(b))
		m.Logical = c.Data + c.Size
	}
	m.Digest = Digest(m.Contents)
	return m, nil
}

func hashFile(ctx context.Context, f *os.File, info os.FileInfo, h hash.Hash, sink scanSink, buffer []byte, cache *HashCache, p string) (total int64, err error) {
	if cache != nil {
		total, err = cache.Resume(p, info, h)
		if err != nil {
			return
		}
		if total > 0 {
			if _, err = f.Seek(total, io.SeekStart); err != nil {
				return
			}
		}
	}
	lastOffset := total
	lastTime := time.Now()
	for {
		if err = ctx.Err(); err != nil {
			return
		}
		n, e := f.Read(buffer)
		if n > 0 {
			if _, err = sink.Write(buffer[:n]); err != nil {
				return
			}
			total += int64(n)
			if cache != nil && (total-lastOffset >= 1<<30 || time.Since(lastTime) > 30*time.Second) {
				if err = cache.Checkpoint(p, info, h, total); err != nil {
					return
				}
				lastOffset = total
				lastTime = time.Now()
			}
		}
		if e == io.EOF {
			return total, nil
		}
		if e != nil {
			return total, e
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
}
func Digest(contents []Content) string {
	type item struct {
		p string
		s int64
		h string
	}
	var files []item
	for _, c := range contents {
		for _, f := range c.Files {
			files = append(files, item{f.Path, c.Size, c.Hash})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].p < files[j].p })
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%d:%s:%d:%s\n", len(f.p), f.p, f.s, f.h)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func (m Manifest) Summaries() []Summary {
	groups := map[string]*Summary{}
	for _, c := range m.Contents {
		for _, f := range c.Files {
			top := strings.Split(f.Path, "/")[0]
			g := groups[top]
			if g == nil {
				g = &Summary{Directory: top}
				groups[top] = g
			}
			g.Files++
			g.Bytes += c.Size
		}
	}
	out := []Summary{}
	for _, g := range groups {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out
}
func Save(s *catalog.Store, m Manifest) error {
	if err := ValidateManifest(m); err != nil {
		return err
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	rules, _ := json.Marshal(m.Rules)
	if m.ReplicaID == "" || m.StreamID == "" || m.BeginPos < 2 {
		return fmt.Errorf("batch must have an assigned replica-set stream and layout")
	}

	if _, e = tx.Exec("INSERT INTO batches(id,replica_id,stream_id,seq,name,source,created,rules,digest,logical_bytes,files,begin_pos,data_pos,commit_pos,blocks,end_pos,encoded_bytes,state) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?, 'planned')", m.ID.String(), m.ReplicaID, m.StreamID, m.Seq, m.Name, m.Source, m.Created, string(rules), m.Digest, m.Logical, m.Files, m.BeginPos, m.DataPos, m.CommitPos, m.Blocks, m.EndPos, m.EncodedBytes); e != nil {
		return fmt.Errorf("insert batch: %w", e)
	}
	if _, e = tx.Exec("INSERT INTO batch_sources VALUES(?,?,?,?)", m.ID.String(), m.SourceIdentity.Device, m.SourceIdentity.Inode, m.SourceIdentity.FSID); e != nil {
		return fmt.Errorf("insert batch source: %w", e)
	}
	for i, c := range m.Contents {
		if _, e = tx.Exec("INSERT INTO contents(batch_id,ordinal,size,hash,meta_offset,data_offset) VALUES(?,?,?,?,?,?)", m.ID.String(), i, c.Size, c.Hash, c.Meta, c.Data); e != nil {
			return fmt.Errorf("insert content: %w", e)
		}
		for _, f := range c.Files {
			if _, e = tx.Exec("INSERT INTO files VALUES(?,?,?,?,?)", m.ID.String(), f.Path, i, f.Source, f.MTime); e != nil {
				return fmt.Errorf("insert file: %w", e)
			}
		}
	}
	for _, x := range m.Excluded {
		if _, e = tx.Exec("INSERT INTO exclusions VALUES(?,?,?,?)", m.ID.String(), x.Path, x.Size, x.Reason); e != nil {
			return fmt.Errorf("insert exclusion: %w", e)
		}
	}
	return tx.Commit()
}
func Load(s *catalog.Store, id string) (m Manifest, err error) {
	m.ID, err = format.ParseID(id)
	if err != nil {
		return
	}
	id = m.ID.String()
	var rules string
	err = s.DB.QueryRow("SELECT replica_id,stream_id,seq,name,source,created,rules,digest,logical_bytes,files,begin_pos,data_pos,commit_pos,blocks,end_pos,encoded_bytes FROM batches WHERE id=?", id).Scan(&m.ReplicaID, &m.StreamID, &m.Seq, &m.Name, &m.Source, &m.Created, &rules, &m.Digest, &m.Logical, &m.Files, &m.BeginPos, &m.DataPos, &m.CommitPos, &m.Blocks, &m.EndPos, &m.EncodedBytes)
	if err != nil {
		return
	}
	err = s.DB.QueryRow("SELECT device,inode,fsid FROM batch_sources WHERE batch_id=?", id).Scan(&m.SourceIdentity.Device, &m.SourceIdentity.Inode, &m.SourceIdentity.FSID)
	if err != nil {
		return
	}
	if err = json.Unmarshal([]byte(rules), &m.Rules); err != nil {
		return
	}
	rows, err := s.DB.Query("SELECT ordinal,size,hash,meta_offset,data_offset FROM contents WHERE batch_id=? ORDER BY ordinal", id)
	if err != nil {
		return
	}
	for rows.Next() {
		var i int
		var c Content
		if err = rows.Scan(&i, &c.Size, &c.Hash, &c.Meta, &c.Data); err != nil {
			rows.Close()
			return
		}
		if i != len(m.Contents) {
			rows.Close()
			err = fmt.Errorf("noncontiguous content index")
			return
		}
		m.Contents = append(m.Contents, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	rows, err = s.DB.Query("SELECT path,source_path,mtime,ordinal FROM files WHERE batch_id=? ORDER BY path", id)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var f File
		var i int
		if err = rows.Scan(&f.Path, &f.Source, &f.MTime, &i); err != nil {
			return
		}
		if i < 0 || i >= len(m.Contents) {
			err = fmt.Errorf("invalid content reference")
			return
		}
		m.Contents[i].Files = append(m.Contents[i].Files, f)
	}
	err = rows.Err()
	return
}
