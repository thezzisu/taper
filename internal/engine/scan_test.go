package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/policy"
	"os"
	"path/filepath"
	"testing"
)

func scanSource(ctx context.Context, root, name string, rules policy.Rules) (Manifest, error) {
	inv, err := InspectPaths(ctx, root, rules, []string{"."})
	if err != nil {
		return Manifest{}, err
	}
	return scanInventory(ctx, inv, name, rules, nil, nil)
}

func planSource(ctx context.Context, s *catalog.Store, root, name string) (m Manifest, read int64, err error) {
	if _, err = StageAdd(ctx, s, root, nil, nil); err != nil {
		return
	}
	m, err = StagePlan(ctx, s, name, 1, false, func(n int64) { read = n })
	return
}

func TestEmptyAndUnsupportedSourcePreflight(t *testing.T) {
	root := t.TempDir()
	if _, e := scanSource(context.Background(), root, "empty", policy.Rules{}); e == nil {
		t.Fatal("accepted empty mountpoint")
	}
	os.WriteFile(filepath.Join(root, "data"), []byte("keep"), 0600)
	os.Symlink("missing", filepath.Join(root, "link"))
	v, e := InspectPaths(context.Background(), root, policy.Rules{}, []string{"."})
	if e != nil || len(v.Issues) != 1 {
		t.Fatal(v, e)
	}
	if _, e = scanSource(context.Background(), root, "invalid", policy.Rules{}); e == nil {
		t.Fatal("silently dropped unsupported file")
	}
	if _, e = scanSource(context.Background(), root, "valid", policy.Rules{Exclude: []string{"link"}}); e != nil {
		t.Fatal(e)
	}
}

func TestResumePartOfLargeFile(t *testing.T) {
	s, _, root := setup(t)
	data := bytes.Repeat([]byte("checkpoint-data"), 100000)
	name := filepath.Join(root, "large")
	os.WriteFile(name, data, 0600)
	info, e := os.Stat(name)
	if e != nil {
		t.Fatal(e)
	}
	cache, e := NewHashCache(s, root)
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.New()
	h.Write(data[:100000])
	if e = cache.Checkpoint("large", info, h, 100000); e != nil {
		t.Fatal(e)
	}
	m, read, e := planSource(context.Background(), s, root, "resume")
	if e != nil {
		t.Fatal(e)
	}
	want := sha256.Sum256(data)
	if read != int64(len(data)-100000) || m.Contents[0].Hash != hex.EncodeToString(want[:]) {
		t.Fatalf("resume mismatch: %+v", m)
	}
}
func TestCacheResumeAndSourceIdentity(t *testing.T) {
	s, _, root := setup(t)
	os.WriteFile(filepath.Join(root, "data"), []byte("data"), 0600)
	m, read, e := planSource(context.Background(), s, root, "first")
	if e != nil || read != 4 {
		t.Fatal(m, e)
	}
	m, read, e = planSource(context.Background(), s, root, "resumed")
	if e != nil || read != 0 {
		t.Fatal(m, e)
	}
	os.WriteFile(filepath.Join(root, "data"), []byte("DIFF"), 0600)
	updated, read, e := planSource(context.Background(), s, root, "changed")
	if e != nil || read != 4 || updated.Contents[0].Hash == m.Contents[0].Hash {
		t.Fatal("stale hash reused", e)
	}
	if e = os.Rename(root, root+"-old"); e != nil {
		t.Fatal(e)
	}
	os.Mkdir(root, 0700)
	if e = CheckSource(m); e == nil {
		t.Fatal("replacement source root accepted")
	}
}
