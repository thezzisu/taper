package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	container "github.com/thezzisu/taper/internal/archive"
	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
	"golang.org/x/sys/unix"
)

const maxArchiveDepth = 16
const maxArchiveExpanded = 64 << 30

// ArchiveEntry is catalog-only metadata, never an additional tape content.
// Entry identifies a member by its ordinal chain, preserving duplicate names.
type ArchiveEntry struct {
	Ordinal   int    `json:"-"`
	Entry     string `json:"-"`
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Size      int64  `json:"size"`
	Format    string `json:"format,omitempty"`
	State     string `json:"state,omitempty"`
	Message   string `json:"message,omitempty"`
	Encrypted bool   `json:"encrypted,omitempty"`
	Password  []byte `json:"-"`
	Volumes   string `json:"-"`
}

func ArchiveEntries(s *catalog.Store, batch string) ([]ArchiveEntry, error) {
	rows, err := s.DB.Query("SELECT ordinal,entry,path,kind,size,format,state,message,encrypted FROM archive_entries WHERE batch_id=? ORDER BY ordinal,entry", batch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []ArchiveEntry
	for rows.Next() {
		var r ArchiveEntry
		if err = rows.Scan(&r.Ordinal, &r.Entry, &r.Path, &r.Kind, &r.Size, &r.Format, &r.State, &r.Message, &r.Encrypted); err != nil {
			return nil, err
		}
		entries = append(entries, r)
	}
	return entries, rows.Err()
}

type archiveIndexOptions struct {
	Target   string
	Password []byte
	Remember bool
	Prompt   func(string) ([]byte, error)
	Force    bool
}

type ArchiveIndexSummary struct {
	Containers int      `json:"containers"`
	Members    int      `json:"members"`
	Locked     []string `json:"locked,omitempty"`
	Issues     []string `json:"issues,omitempty"`
}

func SummarizeArchiveIndex(s *catalog.Store, m Manifest) (ArchiveIndexSummary, error) {
	var result ArchiveIndexSummary
	rows, err := ArchiveEntries(s, m.ID.String())
	if err != nil {
		return result, err
	}
	for _, r := range rows {
		var outer string
		for _, f := range m.Contents[r.Ordinal].Files {
			if container.IsArchiveName(f.Path) {
				outer = f.Path
				break
			}
		}
		if outer == "" {
			continue
		}
		if r.Entry != "" {
			result.Members++
		}
		if r.Kind != "archive" {
			continue
		}
		result.Containers++
		full := outer
		if r.Path != "" {
			full += "!/" + r.Path
		}
		if r.State == "locked" {
			result.Locked = append(result.Locked, full)
		}
		if r.State != "complete" && r.State != "locked" {
			result.Issues = append(result.Issues, full+": "+r.State+" ("+r.Message+")")
		}
	}
	return result, nil
}

type indexedInput struct {
	Size  int64  `json:"size"`
	Hash  string `json:"hash"`
	open  func() (*os.File, error)
	mtime int64
}
type archiveInputs struct {
	files   map[string]indexedInput
	opened  map[string]*os.File
	used    map[string]indexedInput
	missing map[string]bool
}

func (g *archiveInputs) source(name string) (container.Source, error) {
	v, ok := g.files[name]
	if !ok {
		g.missing[name] = true
		return container.Source{}, os.ErrNotExist
	}
	f := g.opened[name]
	if f == nil {
		var err error
		f, err = v.open()
		if err != nil {
			return container.Source{}, err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return container.Source{}, err
		}
		if !st.Mode().IsRegular() || st.Size() != v.Size || (v.mtime != 0 && st.ModTime().UnixNano() != v.mtime) {
			f.Close()
			return container.Source{}, errors.New("container source changed after planning")
		}
		g.opened[name] = f
		g.used[name] = v
	}
	return container.Source{ReaderAt: f, Size: v.Size}, nil
}
func (g *archiveInputs) close() error {
	var result error
	for name, f := range g.opened {
		st, err := f.Stat()
		v := g.files[name]
		if err == nil && (st.Size() != v.Size || v.mtime != 0 && st.ModTime().UnixNano() != v.mtime) {
			err = errors.New("container source changed during indexing")
		}
		result = errors.Join(result, err, f.Close())
	}
	g.opened = nil
	return result
}
func newArchiveInputs() *archiveInputs {
	return &archiveInputs{map[string]indexedInput{}, map[string]*os.File{}, map[string]indexedInput{}, map[string]bool{}}
}

type archiveDependencies struct {
	Engine  string                     `json:"engine"`
	Name    string                     `json:"name"`
	Files   map[string]dependencyInput `json:"files"`
	Missing []string                   `json:"missing,omitempty"`
}
type dependencyInput struct {
	Size  int64  `json:"size"`
	Hash  string `json:"hash"`
	MTime int64  `json:"mtime"`
}

func dependenciesMatch(encoded, name string, g *archiveInputs) bool {
	var d archiveDependencies
	if json.Unmarshal([]byte(encoded), &d) != nil || d.Engine != container.Version || d.Name != name || len(d.Files) == 0 {
		return false
	}
	for name, want := range d.Files {
		got, ok := g.files[name]
		if !ok || want.Hash != got.Hash || want.Size != got.Size || want.MTime != got.mtime {
			return false
		}
	}
	for _, name := range d.Missing {
		if _, ok := g.files[name]; ok {
			return false
		}
	}
	return true
}

func indexArchives(ctx context.Context, s *catalog.Store, m Manifest, o archiveIndexOptions) error {
	if o.Target == "" && !o.Force {
		var pending int
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM contents c WHERE batch_id=? AND NOT EXISTS (SELECT 1 FROM archive_scans a WHERE a.batch_id=c.batch_id AND a.ordinal=c.ordinal)", m.ID.String()).Scan(&pending); err != nil {
			return err
		}
		if pending == 0 {
			return nil
		}
	}
	root, err := os.OpenRoot(m.Source)
	if err != nil {
		return err
	}
	defer root.Close()
	e, err := container.New(ctx)
	if err != nil {
		return err
	}
	defer e.Close()
	groups := map[string]*archiveInputs{}
	for _, c := range m.Contents {
		for _, f := range c.Files {
			dir := filepath.Dir(f.Source)
			g := groups[dir]
			if g == nil {
				g = newArchiveInputs()
				groups[dir] = g
			}
			rel := f.Source
			g.files[filepath.Base(rel)] = indexedInput{Size: c.Size, Hash: c.Hash, mtime: f.MTime, open: func() (*os.File, error) { return root.Open(rel) }}
		}
	}
	defer func() {
		for _, g := range groups {
			g.close()
		}
	}()
	done := map[int]bool{}
	volumeOnly := map[string]bool{}
	recordVolumes := func(ordinal int, encoded string) {
		var d archiveDependencies
		if json.Unmarshal([]byte(encoded), &d) != nil {
			return
		}
		for _, f := range m.Contents[ordinal].Files {
			if filepath.Base(f.Source) != d.Name {
				continue
			}
			for name := range d.Files {
				if name != d.Name {
					volumeOnly[filepath.Join(filepath.Dir(f.Source), name)] = true
				}
			}
		}
	}
	checked, err := s.DB.Query("SELECT a.ordinal,COALESCE(e.volumes,'') FROM archive_scans a LEFT JOIN archive_entries e ON e.batch_id=a.batch_id AND e.ordinal=a.ordinal AND e.entry='' WHERE a.batch_id=?", m.ID.String())
	if err != nil {
		return err
	}
	for checked.Next() {
		var ordinal int
		var deps string
		if err = checked.Scan(&ordinal, &deps); err != nil {
			checked.Close()
			return err
		}
		done[ordinal] = true
		recordVolumes(ordinal, deps)
	}
	err = checked.Err()
	checked.Close()
	if err != nil {
		return err
	}
	var plain []int
	flushPlain := func() error {
		if len(plain) == 0 {
			return nil
		}
		tx, err := s.DB.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		stmt, err := tx.Prepare("INSERT OR IGNORE INTO archive_scans VALUES(?,?)")
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, i := range plain {
			if _, err = stmt.Exec(m.ID.String(), i); err != nil {
				return err
			}
		}
		if err = tx.Commit(); err == nil {
			plain = nil
		}
		return err
	}
	matched := false
	for ordinal, c := range m.Contents {
		if err = ctx.Err(); err != nil {
			return err
		}
		if o.Target == "" && !o.Force && done[ordinal] {
			continue
		}
		var file *File
		for i := range c.Files {
			f := &c.Files[i]
			if !container.IsArchiveName(f.Source) {
				continue
			}
			if o.Target != "" {
				if o.Target == f.Path || strings.HasPrefix(o.Target, f.Path+"!/") {
					file = f
					break
				}
			} else if !volumeOnly[f.Source] && !continuation(filepath.Base(f.Source)) {
				file = f
				break
			}
		}
		if file == nil {
			if o.Target == "" {
				plain = append(plain, ordinal)
				if len(plain) >= 1000 {
					if err = flushPlain(); err != nil {
						return err
					}
				}
			}
			continue
		}
		matched = true
		if err = flushPlain(); err != nil {
			return err
		}
		g := groups[filepath.Dir(file.Source)]
		name := filepath.Base(file.Source)
		if o.Target == "" && !o.Force {

			if deps, err := reuseArchiveIndex(s, m, ordinal, name, g); err != nil {
				return err
			} else if deps != "" {
				recordVolumes(ordinal, deps)
				if _, err = s.DB.Exec("INSERT OR IGNORE INTO archive_scans VALUES(?,?)", m.ID.String(), ordinal); err != nil {
					return err
				}
				continue
			}
		}
		saved := map[string][]byte{}
		rows, err := s.DB.Query("SELECT entry,password FROM archive_entries WHERE batch_id=? AND ordinal=? AND password IS NOT NULL", m.ID.String(), ordinal)
		if err != nil {
			return err
		}
		for rows.Next() {
			var key string
			var pass []byte
			if err = rows.Scan(&key, &pass); err != nil {
				rows.Close()
				return err
			}
			saved[key] = pass
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		scan := archiveIndexer{engine: e, options: o, prefix: file.Path, saved: saved, remaining: maxArchiveExpanded}
		scan.rows = append(scan.rows, ArchiveEntry{Ordinal: ordinal, Kind: "archive", Size: c.Size})
		err = scan.scan(ctx, g, name, 0, 0, nil)
		if err != nil {
			for _, p := range saved {
				clear(p)
			}
			return err
		}
		if o.Target != "" {
			targetPath := strings.TrimPrefix(o.Target, file.Path+"!/")
			if o.Target == file.Path {
				targetPath = ""
			}
			var selected *ArchiveEntry
			for i := range scan.rows {
				if scan.rows[i].Path == targetPath {
					selected = &scan.rows[i]
					break
				}
			}
			if selected == nil || (selected.State != "complete" && selected.State != "partial") {
				for _, p := range saved {
					clear(p)
				}
				if selected == nil {
					return errors.New("cannot reach inner container; an ancestor password is required")
				}
				return fmt.Errorf("container remains %s: %s", selected.State, selected.Message)
			}
			old, err := ArchiveEntries(s, m.ID.String())
			if err != nil {
				return err
			}
			var keep []ArchiveEntry
			for _, r := range old {
				if r.Ordinal == ordinal {
					r.Password = append([]byte(nil), saved[r.Entry]...)
					keep = append(keep, r)
				}
			}
			scan.rows = keepKnownSubtrees(scan.rows, keep)
		}
		for _, p := range saved {
			clear(p)
		}
		var missing []string
		for n := range g.missing {
			missing = append(missing, n)
		}
		sort.Strings(missing)
		dep := map[string]dependencyInput{}
		for n, v := range g.used {
			dep[n] = dependencyInput{v.Size, v.Hash, v.mtime}
			if n != name {
				volumeOnly[filepath.Join(filepath.Dir(file.Source), n)] = true
			}
		}
		deps, err := json.Marshal(archiveDependencies{container.Version, name, dep, missing})
		if err != nil {
			return err
		}
		scan.rows[0].Volumes = string(deps)
		if err = g.close(); err != nil {
			return err
		}
		g.opened = map[string]*os.File{}
		g.used = map[string]indexedInput{}
		g.missing = map[string]bool{}
		if scan.rows[0].Kind != "archive" {
			scan.rows = nil
		}
		if err = saveArchiveIndex(s, m.ID.String(), ordinal, scan.rows); err != nil {
			return err
		}
		for _, r := range scan.rows {
			clear(r.Password)
		}
	}
	if o.Target != "" && !matched {
		return errors.New("no staged container matched")
	}
	return flushPlain()
}

func reuseArchiveIndex(s *catalog.Store, m Manifest, ordinal int, name string, g *archiveInputs) (string, error) {
	c := m.Contents[ordinal]
	rows, err := s.DB.Query(`SELECT e.batch_id,e.ordinal,e.volumes FROM archive_entries e JOIN contents c ON c.batch_id=e.batch_id AND c.ordinal=e.ordinal WHERE c.hash=? AND c.size=? AND e.entry='' AND e.batch_id!=? AND e.state IN ('complete','partial','locked') LIMIT 8`, c.Hash, c.Size, m.ID.String())
	if err != nil {
		return "", err
	}
	var batch, dependencies string
	var oldOrdinal int
	for rows.Next() {
		var b, d string
		var n int
		if err = rows.Scan(&b, &n, &d); err != nil {
			rows.Close()
			return "", err
		}
		if dependenciesMatch(d, name, g) {
			batch, oldOrdinal, dependencies = b, n, d
			break
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || batch == "" {
		return "", err
	}
	_, err = s.DB.Exec(`INSERT OR REPLACE INTO archive_entries SELECT ?,?,entry,path,kind,size,format,state,message,encrypted,password,volumes FROM archive_entries WHERE batch_id=? AND ordinal=?`, m.ID.String(), ordinal, batch, oldOrdinal)
	return dependencies, err
}

func saveArchiveIndex(s *catalog.Store, batch string, ordinal int, entries []ArchiveEntry) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM archive_entries WHERE batch_id=? AND ordinal=?", batch, ordinal); err != nil {
		return err
	}
	stmt, err := tx.Prepare("INSERT INTO archive_entries VALUES(?,?,?,?,?,?,?,?,?,?,?,?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range entries {
		var password any
		if r.Password != nil {
			password = r.Password
		}
		if _, err = stmt.Exec(batch, ordinal, r.Entry, r.Path, r.Kind, r.Size, r.Format, r.State, r.Message, r.Encrypted, password, r.Volumes); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("INSERT OR IGNORE INTO archive_scans VALUES(?,?)", batch, ordinal); err != nil {
		return err
	}
	return tx.Commit()
}

// StageUnlock only indexes local staged bytes. Tape access and backup layout are
// unchanged. Missing ancestor passwords can be supplied by the optional prompt.
func StageUnlock(ctx context.Context, s *catalog.Store, target string, password []byte, remember bool, prompt func(string) ([]byte, error)) error {
	if target == "" {
		return errors.New("provide a staged container path")
	}
	release, err := lockStage(s)
	if err != nil {
		return err
	}
	defer release()
	st, err := LoadStage(s)
	if err != nil {
		return err
	}
	if st.Plan == "" {
		return errors.New("run stage plan before unlocking a container")
	}
	m, err := Load(s, st.Plan)
	if err != nil {
		return err
	}
	if err = checkSelectedFiles(m); err != nil {
		return err
	}
	entries, err := ArchiveEntries(s, m.ID.String())
	if err != nil {
		return err
	}
	found := false
	for _, r := range entries {
		if r.Kind != "archive" {
			continue
		}
		for _, f := range m.Contents[r.Ordinal].Files {
			full := f.Path
			if r.Path != "" {
				full += "!/" + r.Path
			}
			if full == target {
				found = true
			}
		}
	}
	if !found {
		return errors.New("no indexed staged container matched; use the container paths reported by stage plan")
	}
	if err = indexArchives(ctx, s, m, archiveIndexOptions{Target: target, Password: password, Remember: remember, Prompt: prompt}); err != nil {
		return err
	}
	entries, err = ArchiveEntries(s, m.ID.String())
	if err != nil {
		return err
	}
	for _, r := range entries {
		for _, f := range m.Contents[r.Ordinal].Files {
			full := f.Path
			if r.Path != "" {
				full += "!/" + r.Path
			}
			if full == target {
				if r.State != "complete" && r.State != "partial" {
					return fmt.Errorf("container remains %s: %s", r.State, r.Message)
				}
				return nil
			}
		}
	}
	return errors.New("could not reach the requested inner container; an ancestor may need its password")
}

type archiveIndexer struct {
	engine    *container.Engine
	options   archiveIndexOptions
	prefix    string
	saved     map[string][]byte
	rows      []ArchiveEntry
	remaining int64
}

func (s *archiveIndexer) scan(ctx context.Context, g *archiveInputs, name string, row, depth int, inherited []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > maxArchiveDepth || len(s.rows) >= container.MaxEntries {
		s.fail(row, "limit", container.ErrLimit)
		return nil
	}
	full := s.prefix
	if s.rows[row].Path != "" {
		full += "!/" + s.rows[row].Path
	}
	key := s.rows[row].Entry
	password := s.saved[key]
	if password == nil {
		password = inherited
	}
	if s.options.Target == full {
		password = s.options.Password
	}
	source, err := g.source(name)
	if err != nil {
		state := "error"
		if errors.Is(err, container.ErrPassword) {
			state = "locked"
		}
		if errors.Is(err, container.ErrLimit) {
			state = "limit"
		}
		s.fail(row, state, err)
		return nil
	}
	limited, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	reader, err := s.engine.Open(limited, name, source, g.source, password)
	if errors.Is(err, container.ErrPassword) && s.options.Prompt != nil {
		pass, e := s.options.Prompt(full)
		if e != nil {
			return e
		}
		if pass != nil {
			defer clear(pass)
			password = pass
			reader, err = s.engine.Open(limited, name, source, g.source, password)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		state := "error"
		if errors.Is(err, container.ErrUnsupported) {
			state = "unsupported"
		}
		if errors.Is(err, container.ErrPassword) {
			state = "locked"
			s.rows[row].Encrypted = true
		}
		if errors.Is(err, container.ErrNotArchive) {
			state = "unsupported"
		}
		if errors.Is(err, container.ErrLimit) {
			state = "limit"
		}
		s.fail(row, state, err)
		return nil
	}
	defer reader.Close()
	if !container.IsArchiveFormat(reader.Format) {
		s.rows[row].Kind = "file"
		return nil
	}
	s.rows[row].Format = reader.Format
	s.rows[row].State = "complete"
	s.rows[row].Encrypted = reader.Encrypted
	if reader.Warning != "" {
		s.rows[row].State = "partial"
		s.rows[row].Message = reader.Warning
	}
	if old := s.saved[key]; old != nil {
		s.rows[row].Password = append([]byte{}, old...)
	}
	children := map[string]int{}
	items := map[string]container.Item{}
	ambiguous := map[string]bool{}
	for _, item := range reader.Items {
		if len(s.rows) >= container.MaxEntries {
			s.fail(row, "limit", container.ErrLimit)
			return nil
		}
		member := archiveMemberPath(item.Path)
		if s.rows[row].Path != "" {
			member = s.rows[row].Path + "!/" + member
		}
		entry := strconv.FormatUint(uint64(item.Index), 10)
		if key != "" {
			entry = key + "/" + entry
		}
		kind := "file"
		if item.Directory {
			kind = "directory"
		}
		if item.Link {
			kind = "link"
		}
		r := ArchiveEntry{Entry: entry, Path: member, Kind: kind, Size: item.Size, Encrypted: item.Encrypted}
		_, duplicate := items[item.Path]
		if !duplicate && !item.Directory && !item.Link && container.IsArchiveName(item.Path) && !continuation(path.Base(item.Path)) {
			r.Kind = "archive"
			children[item.Path] = len(s.rows)
		}
		s.rows = append(s.rows, r)
		if _, exists := items[item.Path]; !exists {
			items[item.Path] = item
		} else {
			ambiguous[item.Path] = true
			if first, ok := children[item.Path]; ok {
				s.rows[first].Kind = "file"
				s.rows[first].Message = "duplicate member name; nested expansion skipped"
			}
			delete(children, item.Path)
			s.rows[len(s.rows)-1].Message = "duplicate member name; nested expansion skipped"
		}
		s.rows[row].Encrypted = s.rows[row].Encrypted || item.Encrypted
	}
	for name := range ambiguous {
		delete(items, name)
	}
	outputs := map[uint32]io.Writer{}
	temporary := newArchiveInputs()
	defer temporary.close()
	var scratch string
	defer func() {
		if scratch != "" {
			os.RemoveAll(scratch)
		}
	}()
	var writers []*os.File
	defer func() {
		for _, w := range writers {
			w.Close()
		}
	}()
	createOutput := func(size int64) (*os.File, error) {
		if size < 0 || size > s.remaining {
			return nil, container.ErrLimit
		}
		if scratch == "" {
			var err error
			scratch, err = os.MkdirTemp("", "taper-index-")
			if err != nil {
				return nil, err
			}
			var space unix.Statfs_t
			if err = unix.Statfs(scratch, &space); err != nil {
				return nil, err
			}
			s.remaining = min(s.remaining, int64(space.Bavail)*int64(space.Bsize)-(1<<30))
		}
		if size > s.remaining {
			return nil, container.ErrLimit
		}
		return os.CreateTemp(scratch, "entry-")
	}
	for name, child := range children {
		for _, part := range relatedMembers(items, name) {
			item := items[part]
			if _, ok := outputs[item.Index]; ok {
				continue
			}
			if item.Encrypted && password == nil {
				s.fail(child, "locked", container.ErrPassword)
				s.rows[row].State = "partial"
				continue
			}
			out, err := createOutput(item.Size)
			if err != nil {
				s.fail(child, "limit", err)
				s.rows[row].State = "partial"
				continue
			}
			writers = append(writers, out)
			outputs[item.Index] = &archiveBudgetWriter{out, &s.remaining}
			file := out.Name()
			temporary.files[part] = indexedInput{Size: item.Size, open: func() (*os.File, error) { return os.Open(file) }}
		}
	}
	// Validate a supplied password with a real encrypted member, even when only
	// filenames are needed. Choose the smallest to avoid an unnecessary large read.
	if password != nil && s.rows[row].Encrypted {
		var smallest *container.Item
		for i := range reader.Items {
			v := &reader.Items[i]
			if v.Encrypted && !v.Directory && !v.Link && v.Size >= 0 && (smallest == nil || v.Size < smallest.Size) {
				smallest = v
			}
		}
		if smallest != nil && outputs[smallest.Index] == nil {
			if smallest.Size <= s.remaining {
				outputs[smallest.Index] = &archiveBudgetWriter{io.Discard, &s.remaining}
			} else {
				s.fail(row, "limit", container.ErrLimit)
				return nil
			}
		}
	}
	if err = reader.Extract(limited, outputs); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		state := "error"
		if errors.Is(err, container.ErrLimit) {
			state = "limit"
		} else if errors.Is(err, container.ErrUnsupported) {
			state = "unsupported"
		} else if errors.Is(err, container.ErrPassword) {
			state = "locked"
		}
		s.fail(row, state, err)
		for _, child := range children {
			if s.rows[child].State == "" {
				s.fail(child, state, err)
			}
		}
		return nil
	}
	reader.Close()
	for _, w := range writers {
		if err = w.Close(); err != nil {
			return err
		}
	}
	writers = nil
	if password == nil && s.rows[row].Encrypted {
		s.fail(row, "locked", container.ErrPassword)
	}
	if password != nil && s.rows[row].Encrypted && s.options.Remember {
		s.rows[row].Password = append([]byte{}, password...)
	}
	for name, v := range temporary.files {
		f, err := v.open()
		if err != nil {
			return err
		}
		st, err := f.Stat()
		f.Close()
		if err != nil {
			return err
		}
		v.Size = st.Size()
		temporary.files[name] = v
	}
	nameForParent := name
	// Supply unusually named volume companions through the upstream callback.
	// Common candidates/volumes above are extracted together for solid archives.
	for name, item := range items {
		if _, ok := temporary.files[name]; ok || item.Directory || item.Link {
			continue
		}
		var file string
		temporary.files[name] = indexedInput{Size: item.Size, open: func() (*os.File, error) {
			if file != "" {
				return os.Open(file)
			}
			out, err := createOutput(item.Size)
			if err != nil {
				return nil, err
			}
			defer out.Close()
			r, err := s.engine.Open(limited, nameForParent, source, g.source, password)
			if err != nil {
				return nil, err
			}
			defer r.Close()
			if err = r.Extract(limited, map[uint32]io.Writer{item.Index: &archiveBudgetWriter{out, &s.remaining}}); err != nil {
				return nil, err
			}
			file = out.Name()
			return os.Open(file)
		}}
	}
	names := make([]string, 0, len(children))
	for n := range children {
		names = append(names, n)
	}
	sort.Strings(names)
	volumeOnly := map[string]bool{}
	for _, name := range names {
		child := children[name]
		if volumeOnly[name] {
			s.rows[child].Kind, s.rows[child].State, s.rows[child].Message = "file", "", "archive volume"
			continue
		}
		if s.rows[child].State != "" {
			continue
		}
		// Volume names are resolved within this member's directory, never as host paths.
		group := newArchiveInputs()
		for n, v := range temporary.files {
			if path.Dir(n) == path.Dir(name) {
				group.files[path.Base(n)] = v
			}
		}
		childPassword := password
		childPath := s.prefix + "!/" + s.rows[child].Path
		if s.options.Target == childPath || strings.HasPrefix(s.options.Target, childPath+"!/") {
			childPassword = s.options.Password
		}
		if err = s.scan(ctx, group, path.Base(name), child, depth+1, childPassword); err != nil {
			group.close()
			return err
		}
		for part := range group.used {
			if part != path.Base(name) {
				volumeOnly[path.Join(path.Dir(name), part)] = true
			}
		}
		if err = group.close(); err != nil {
			return err
		}
		if s.rows[child].Kind == "archive" && s.rows[child].State != "complete" && s.rows[row].State == "complete" {
			s.rows[row].State = "partial"
		}
	}
	return nil
}

// An unsuccessful refresh must not erase already indexed immutable bytes.
func keepKnownSubtrees(next, old []ArchiveEntry) []ArchiveEntry {
	known := map[string]ArchiveEntry{}
	for _, r := range old {
		known[r.Entry] = r
	}
	var preserve []string
	for _, r := range next {
		before, ok := known[r.Entry]
		if ok && r.Kind == "archive" && r.State != "complete" && r.State != "partial" && (before.State == "complete" || before.State == "partial") {
			preserve = append(preserve, r.Entry)
		}
	}
	inside := func(key string) bool {
		for _, p := range preserve {
			if p == "" || key == p || strings.HasPrefix(key, p+"/") {
				return true
			}
		}
		return false
	}
	var merged []ArchiveEntry
	for _, r := range next {
		if !inside(r.Entry) {
			merged = append(merged, r)
		}
	}
	for _, r := range old {
		if inside(r.Entry) {
			merged = append(merged, r)
		}
	}
	for i, r := range merged {
		if r.Kind != "archive" || r.State != "partial" || r.Message != "" {
			continue
		}
		state := "complete"
		for _, child := range merged {
			if child.Entry != r.Entry && child.Kind == "archive" && (r.Entry == "" || strings.HasPrefix(child.Entry, r.Entry+"/")) && child.State != "complete" {
				state = "partial"
				break
			}
		}
		merged[i].State = state
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Entry < merged[j].Entry })
	return merged
}

func (s *archiveIndexer) fail(row int, state string, err error) {
	s.rows[row].State = state
	s.rows[row].Message = err.Error()
}

type archiveBudgetWriter struct {
	w         io.Writer
	remaining *int64
}

func (w *archiveBudgetWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > *w.remaining {
		return 0, container.ErrLimit
	}
	n, err := w.w.Write(p)
	*w.remaining -= int64(n)
	return n, err
}

func archiveMemberPath(p string) string {
	parts := strings.Split(format.Normalize(strings.ReplaceAll(p, "\\", "/")), "/")
	for i, s := range parts {
		switch s {
		case "":
			parts[i] = "%00"
		case ".":
			parts[i] = "%2E"
		case "..":
			parts[i] = "%2E%2E"
		default:
			parts[i] = strings.ReplaceAll(s, "!", "%21")
		}
	}
	return strings.Join(parts, "/")
}

var rarPart = regexp.MustCompile(`(?i)^(.*\.part)([0-9]+)(\.rar)$`)
var numberedVolume = regexp.MustCompile(`^(.*\.)([0-9]{2,})$`)
var letterVolume = regexp.MustCompile(`(?i)^(.*\.)([a-z]*a[a-z])$`)
var oldRARVolume = regexp.MustCompile(`(?i)^\.[r-z][0-9]{2}$`)
var zipVolume = regexp.MustCompile(`(?i)^\.z[0-9]+$`)

func continuation(name string) bool {
	if m := rarPart.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[2])
		return n > 1
	}
	if m := numberedVolume.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[2])
		return n > 1
	}
	ext := strings.ToLower(path.Ext(name))
	return oldRARVolume.MatchString(ext) || zipVolume.MatchString(ext)
}
func relatedMembers(items map[string]container.Item, name string) []string {
	names := []string{name}
	base := path.Base(name)
	dir := path.Dir(name)
	part := rarPart.FindStringSubmatch(base)
	number := numberedVolume.FindStringSubmatch(base)
	letters := letterVolume.FindStringSubmatch(base)
	for n, v := range items {
		if n == name || v.Directory || v.Link || path.Dir(n) != dir {
			continue
		}
		other := path.Base(n)
		match := false
		if part != nil {
			m := rarPart.FindStringSubmatch(other)
			match = m != nil && strings.EqualFold(m[1], part[1])
		}
		if number != nil {
			m := numberedVolume.FindStringSubmatch(other)
			match = m != nil && m[1] == number[1]
		}
		if letters != nil {
			m := letterVolume.FindStringSubmatch(other)
			match = m != nil && m[1] == letters[1]
		}
		if strings.EqualFold(path.Ext(base), ".zip") {
			match = strings.TrimSuffix(base, path.Ext(base)) == strings.TrimSuffix(other, path.Ext(other)) && zipVolume.MatchString(path.Ext(other))
		}
		if strings.EqualFold(path.Ext(base), ".rar") && part == nil {
			match = strings.EqualFold(strings.TrimSuffix(base, path.Ext(base)), strings.TrimSuffix(other, path.Ext(other))) && continuation(other)
		}
		if match {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}
