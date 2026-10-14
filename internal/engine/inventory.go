package engine

import (
	"context"
	"fmt"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/policy"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type InventoryFile struct {
	Path   string `json:"path"`
	Source string `json:"source"`
	Size   int64  `json:"size"`
	MTime  int64  `json:"mtime"`
}
type Issue struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}
type Inventory struct {
	Root     string          `json:"root"`
	Files    []InventoryFile `json:"files,omitempty"`
	Issues   []Issue         `json:"issues,omitempty"`
	Identity SourceIdentity  `json:"identity"`
	Excluded []Exclusion     `json:"excluded,omitempty"`
}

func InspectPaths(ctx context.Context, root string, rules policy.Rules, paths []string) (v Inventory, err error) {
	if err = rules.Validate(); err != nil {
		return
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return
	}
	st, e := os.Stat(root)
	if e != nil {
		return v, e
	}
	if !st.IsDir() {
		return v, fmt.Errorf("source must be a directory")
	}
	v.Root = root
	if v.Identity, err = IdentifySource(root); err != nil {
		return
	}
	seen := map[string]string{}
	var total int64
	visit := func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		if p == root || d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		canonical := format.Normalize(filepath.ToSlash(rel))
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !rules.Keep(canonical) {
			v.Excluded = append(v.Excluded, Exclusion{Path: canonical, Size: info.Size(), Reason: "rule"})
			return nil
		}
		if e = format.ValidPath(canonical); e != nil {
			v.Issues = append(v.Issues, Issue{canonical, e.Error()})
			return nil
		}
		if !info.Mode().IsRegular() {
			v.Issues = append(v.Issues, Issue{canonical, "not a regular file: " + info.Mode().String()})
			return nil
		}
		if old, ok := seen[canonical]; ok {
			if old == rel {
				return nil
			}
			return fmt.Errorf("normalized path collision: %s", canonical)
		}
		seen[canonical] = rel
		total += info.Size()
		if total < 0 {
			return fmt.Errorf("source size overflow")
		}
		v.Files = append(v.Files, InventoryFile{canonical, rel, info.Size(), info.ModTime().UnixNano()})
		return nil
	}
	for _, p := range paths {
		p = filepath.Clean(p)
		if filepath.IsAbs(p) || p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
			return v, fmt.Errorf("path must be relative to source: %s", p)
		}
		if err = filepath.WalkDir(filepath.Join(root, p), visit); err != nil {
			return
		}
	}
	if err == nil && len(v.Files) == 0 && len(v.Excluded) == 0 {
		err = fmt.Errorf("source directory %s is empty; check that the filesystem is mounted", root)
	}
	if err == nil {
		id, e := IdentifySource(root)
		if e != nil {
			return v, e
		}
		if id != v.Identity {
			return v, fmt.Errorf("source root changed during inventory")
		}
	}
	return
}
