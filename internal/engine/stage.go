package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
	"github.com/thezzisu/taper/internal/policy"
	"golang.org/x/sys/unix"
)

type Stage struct {
	ReplicaID string          `json:"replica_id,omitempty"`
	Source    string          `json:"source,omitempty"`
	Identity  SourceIdentity  `json:"identity"`
	Rules     policy.Rules    `json:"rules"`
	Plan      string          `json:"plan,omitempty"`
	Files     []InventoryFile `json:"files"`
	Excluded  []Exclusion     `json:"excluded,omitempty"`
}

// The lock spans scanning/writing, without holding a SQLite transaction open.
// Process exit releases it, so interrupted commands cannot leave a stale lease.
func lockStage(s *catalog.Store) (func(), error) {
	f, e := os.OpenFile(s.Path+".stage.lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("staging area is in use by another command")
	}
	return func() { f.Close() }, nil
}

func LoadStage(s *catalog.Store) (v Stage, err error) {
	v.Files = []InventoryFile{}
	tx, err := s.DB.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	var identity, rules string
	var replicaID sql.NullString
	var plan sql.NullString
	err = tx.QueryRow("SELECT replica_id,source,identity,rules,plan_id FROM stage WHERE id=1").Scan(&replicaID, &v.Source, &identity, &rules, &plan)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil
	}
	if err != nil {
		return
	}
	v.Plan = plan.String
	v.ReplicaID = replicaID.String

	if err = json.Unmarshal([]byte(identity), &v.Identity); err != nil {
		return
	}
	if err = json.Unmarshal([]byte(rules), &v.Rules); err != nil {
		return
	}
	rows, err := tx.Query("SELECT path,source_path,size,mtime FROM stage_files ORDER BY path")
	if err != nil {
		return
	}
	for rows.Next() {
		var f InventoryFile
		if err = rows.Scan(&f.Path, &f.Source, &f.Size, &f.MTime); err != nil {
			rows.Close()
			return
		}
		v.Files = append(v.Files, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	rows, err = tx.Query("SELECT path,size,reason FROM stage_exclusions ORDER BY path")
	if err != nil {
		return
	}
	for rows.Next() {
		var x Exclusion
		if err = rows.Scan(&x.Path, &x.Size, &x.Reason); err != nil {
			rows.Close()
			return
		}
		v.Excluded = append(v.Excluded, x)
	}
	err = rows.Err()
	rows.Close()
	return
}

func StageAdd(ctx context.Context, s *catalog.Store, root string, paths []string, rules *policy.Rules) (int, error) {
	release, e := lockStage(s)
	if e != nil {
		return 0, e
	}
	defer release()
	v, e := LoadStage(s)
	if e != nil {
		return 0, e
	}
	if root == "" {
		root = v.Source
	}
	if root == "" {
		root = "."
	}
	root, e = filepath.Abs(root)
	if e != nil {
		return 0, e
	}
	if v.Source != "" && v.Source != root {
		return 0, errors.New("one source root per stage; finish it or use stage rm . before changing source")
	}
	if rules != nil {
		if e = rules.Validate(); e != nil {
			return 0, e
		}
		old, _ := json.Marshal(v.Rules)
		next, _ := json.Marshal(rules)
		if v.Source != "" && string(old) != string(next) {
			return 0, errors.New("stage rules differ; clear and add again with the revised rules")
		}
		v.Rules = *rules
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	inv, e := InspectPaths(ctx, root, v.Rules, paths)
	if e != nil {
		return 0, e
	}
	if len(inv.Issues) > 0 {
		return 0, fmt.Errorf("unsupported source entry: %s", inv.Issues[0].Path)
	}
	if v.Source != "" && inv.Identity != v.Identity {
		return 0, errors.New("source root changed; clear and add again")
	}
	id, _ := json.Marshal(inv.Identity)
	policyJSON, _ := json.Marshal(v.Rules)
	tx, e := s.DB.Begin()
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	if e = invalidateStagePlan(tx); e != nil {
		return 0, e
	}
	if _, e = tx.Exec("INSERT INTO stage(id,replica_id,source,identity,rules,plan_id) VALUES(1,NULL,?,?,?,NULL) ON CONFLICT(id) DO UPDATE SET source=excluded.source,identity=excluded.identity,rules=excluded.rules,plan_id=NULL", root, string(id), string(policyJSON)); e != nil {
		return 0, fmt.Errorf("save stage: %w", e)
	}
	selected := map[string]bool{}
	for _, f := range inv.Files {
		selected[f.Path] = true
	}
	for _, f := range v.Files {
		for _, p := range paths {
			p = filepath.ToSlash(filepath.Clean(p))
			if (p == "." || matchesPrefix(filepath.ToSlash(f.Source), p)) && !selected[f.Path] {
				if _, e = tx.Exec("DELETE FROM stage_files WHERE path=?", f.Path); e != nil {
					return 0, e
				}
				break
			}
		}
	}
	for _, f := range inv.Files {
		if _, e = tx.Exec("INSERT INTO stage_files VALUES(?,?,?,?) ON CONFLICT(path) DO UPDATE SET source_path=excluded.source_path,size=excluded.size,mtime=excluded.mtime", f.Path, f.Source, f.Size, f.MTime); e != nil {
			return 0, e
		}
		if _, e = tx.Exec("DELETE FROM stage_exclusions WHERE path=?", f.Path); e != nil {
			return 0, e
		}
	}
	for _, x := range inv.Excluded {
		if _, e = tx.Exec("INSERT INTO stage_exclusions VALUES(?,?,?) ON CONFLICT(path) DO UPDATE SET size=excluded.size,reason=excluded.reason", x.Path, x.Size, x.Reason); e != nil {
			return 0, e
		}
	}
	return len(inv.Files), tx.Commit()
}

func matchesPrefix(p, prefix string) bool { return p == prefix || strings.HasPrefix(p, prefix+"/") }

func StageRemove(s *catalog.Store, paths []string) (int, error) {
	if len(paths) == 0 {
		return 0, errors.New("provide staged paths, or . to clear the stage")
	}
	release, e := lockStage(s)
	if e != nil {
		return 0, e
	}
	defer release()
	v, e := LoadStage(s)
	if e != nil {
		return 0, e
	}
	clearAll := false
	for i, p := range paths {
		p = strings.TrimSuffix(p, "/")
		if p == "." {
			clearAll = true
		} else if e = format.ValidPath(p); e != nil {
			return 0, e
		}
		paths[i] = p
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return 0, e
	}
	defer tx.Rollback()
	if clearAll {
		if e = invalidateStagePlan(tx); e != nil {
			return 0, e
		}
		if _, e = tx.Exec("DELETE FROM stage_files; DELETE FROM stage_exclusions; DELETE FROM stage"); e != nil {
			return 0, e
		}
		return len(v.Files), tx.Commit()
	}
	removed := 0
	for _, f := range v.Files {
		for _, p := range paths {
			if matchesPrefix(f.Path, p) {
				if _, e = tx.Exec("DELETE FROM stage_files WHERE path=?", f.Path); e != nil {
					return 0, e
				}
				if _, e = tx.Exec("INSERT INTO stage_exclusions VALUES(?,?,'stage rm') ON CONFLICT(path) DO UPDATE SET size=excluded.size,reason=excluded.reason", f.Path, f.Size); e != nil {
					return 0, e
				}
				removed++
				break
			}
		}
	}
	if removed == 0 {
		return 0, errors.New("no staged paths matched")
	}
	if e = invalidateStagePlan(tx); e != nil {
		return 0, e
	}
	return removed, tx.Commit()
}

func invalidateStagePlan(tx *sql.Tx) error {
	var old sql.NullString
	e := tx.QueryRow("SELECT plan_id FROM stage WHERE id=1").Scan(&old)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE stage SET plan_id=NULL WHERE id=1"); e != nil {
		return e
	}
	if !old.Valid {
		return nil
	}
	var replicas int
	if e = tx.QueryRow("SELECT COUNT(*) FROM replicas WHERE batch_id=?", old.String).Scan(&replicas); e != nil {
		return e
	}
	var state string
	if e = tx.QueryRow("SELECT state FROM batches WHERE id=?", old.String).Scan(&state); e != nil {
		return e
	}
	if replicas > 0 || state != "planned" {
		return errors.New("batch has started; resolve its unfinished data before changing the stage")
	}
	for _, table := range []string{"files", "contents", "exclusions", "batch_sources"} {
		if _, e = tx.Exec("DELETE FROM "+table+" WHERE batch_id=?", old.String); e != nil {
			return e
		}
	}
	_, e = tx.Exec("DELETE FROM batches WHERE id=? AND state='planned'", old.String)
	return e
}

func StagePlan(ctx context.Context, s *catalog.Store, name, replica string, noCache bool, progress func(int64)) (m Manifest, err error) {
	release, err := lockStage(s)
	if err != nil {
		return
	}
	defer release()
	v, err := LoadStage(s)
	if err != nil {
		return
	}
	if len(v.Files) == 0 {
		return m, errors.New("staging area is empty")
	}
	if replica == "" {
		replica = v.ReplicaID
	}
	r, err := ResolveReplicaSet(s, replica)
	if err != nil {
		return m, err
	}
	if err = CheckSource(Manifest{Source: v.Source, SourceIdentity: v.Identity}); err != nil {
		return
	}
	if v.Plan != "" {
		m, err = Load(s, v.Plan)
		if err != nil {
			return
		}
		if m.ReplicaID != r.ID {
			return m, errors.New("plan is bound to another replica set; restage before changing it")
		}
		var state string
		if err = s.DB.QueryRow("SELECT state FROM batches WHERE id=?", v.Plan).Scan(&state); err != nil {
			return
		}
		if state != "planned" {
			return m, errors.New("batch already started; resolve it before replanning")
		}
		if err = checkSelectedFiles(m); err != nil {
			return
		}
	}
	if v.Plan == "" || noCache {
		var cache *HashCache
		if !noCache {
			cache, err = NewHashCache(s, v.Source)
			if err != nil {
				return
			}
		}
		if name == "" {
			name = "archive"
		}
		next, e := scanInventory(ctx, Inventory{Root: v.Source, Files: v.Files, Identity: v.Identity, Excluded: v.Excluded}, name, v.Rules, progress, cache)
		if e != nil {
			return m, e
		}
		if v.Plan != "" {
			if next.Digest != m.Digest {
				return m, errors.New("source differs from the existing plan; restage it")
			}
		} else {
			m = next
			m.ReplicaID = r.ID
			if err = assignBatch(s, &m); err != nil {
				return
			}
		}
	}
	budget, err := ReplicaBudget(s, r.ID)
	if err != nil {
		return
	}
	if Budget(m) > budget {
		return m, fmt.Errorf("archive requires %d encoded bytes; replica set budget is %d", Budget(m), budget)
	}
	if v.Plan == "" {
		if err = Save(s, m); err != nil {
			return
		}
		if _, err = s.DB.Exec("UPDATE stage SET replica_id=?,plan_id=? WHERE id=1", r.ID, m.ID.String()); err != nil {
			return
		}
	}
	err = indexArchives(ctx, s, m, archiveIndexOptions{Force: noCache})
	return
}

// StageWrite prepares a complete image before any physical write when both
// destinations are supplied. Original sources are streamed once.
func StageWrite(ctx context.Context, s *catalog.Store, d media.Device, key []byte, o WriteOptions) (archive, replica string, err error) {
	release, err := lockStage(s)
	if err != nil {
		return
	}
	defer release()
	v, err := LoadStage(s)
	if err != nil {
		return
	}
	if v.Plan == "" {
		return "", "", errors.New("run stage plan after the last staging change")
	}
	m, err := Load(s, v.Plan)
	if err != nil {
		return
	}
	archive = m.ID.String()
	if d == nil && o.Image == nil {
		return archive, "", errors.New("provide --device, --image, or both")
	}
	// Validate the target, including its prefix, before preparing an image.
	if d != nil {
		if _, err = CheckWriteTarget(s, d, Meta(m)); err != nil {
			return
		}
	}
	if o.Image != nil {
		if o.Image.ReplicaID != m.ReplicaID {
			return archive, "", errors.New("image belongs to another replica set")
		}
		k, e := ReplicaKey(s, m.ReplicaID, key)
		if e != nil {
			return archive, "", e
		}
		defer clear(k)
		if err = o.Image.SetKey(k); err != nil {
			return
		}
		var prepared int64
		if prepared, err = PrepareImageArchive(ctx, s, o.Image, m, o); err != nil {
			return
		}
		if d != nil {
			if o.Progress != nil {
				notify := o.Progress
				o.Progress = func(n uint64, b int64) { notify(m.Blocks+min(n, m.Blocks), prepared+b) }
			}
			replica, err = writeImageArchive(ctx, s, d, o.Image, Meta(m), key, o)
			if err != nil {
				return
			}
		}
	} else {
		replica, err = WriteArchive(ctx, s, d, m, key, o)
		if err != nil {
			return
		}
	}
	_, err = s.DB.Exec("DELETE FROM stage_files; DELETE FROM stage_exclusions; DELETE FROM stage")
	return
}

func checkSelectedFiles(m Manifest) error {
	if e := CheckSource(m); e != nil {
		return e
	}
	root, e := os.OpenRoot(m.Source)
	if e != nil {
		return e
	}
	defer root.Close()
	for _, c := range m.Contents {
		for _, f := range c.Files {
			st, e := root.Lstat(f.Source)
			if e != nil {
				return e
			}
			if !st.Mode().IsRegular() || st.Size() != c.Size || st.ModTime().UnixNano() != f.MTime {
				return fmt.Errorf("source changed; add and plan again: %s", f.Path)
			}
		}
	}
	return nil
}
