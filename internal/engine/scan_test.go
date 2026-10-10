package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/thezzisu/taper/internal/policy"
	"os"
	"path/filepath"
	"testing"
)

func TestEmptyAndUnsupportedSourcePreflight(t *testing.T) {
	root := t.TempDir()
	if _, e := Scan(context.Background(), root, "empty", policy.Rules{}); e == nil {
		t.Fatal("accepted empty mountpoint")
	}
	os.WriteFile(filepath.Join(root, "data"), []byte("keep"), 0600)
	os.Symlink("missing", filepath.Join(root, "link"))
	v, e := InspectFiles(context.Background(), root, policy.Rules{})
	if e != nil || len(v.Issues) != 1 {
		t.Fatal(v, e)
	}
	if _, e = Scan(context.Background(), root, "invalid", policy.Rules{}); e == nil {
		t.Fatal("silently dropped unsupported file")
	}
	if _, e = Scan(context.Background(), root, "valid", policy.Rules{Exclude: []string{"link"}}); e != nil {
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
	m, e := ScanCached(context.Background(), root, "resume", policy.Rules{}, nil, cache)
	if e != nil {
		t.Fatal(e)
	}
	want := sha256.Sum256(data)
	if m.ResumedBytes != 100000 || m.ReadBytes != int64(len(data)-100000) || m.Contents[0].Hash != hex.EncodeToString(want[:]) {
		t.Fatalf("resume mismatch: %+v", m)
	}
}
func TestCacheResumeAndSourceIdentity(t *testing.T) {
	s, _, root := setup(t)
	os.WriteFile(filepath.Join(root, "data"), []byte("data"), 0600)
	cache, e := NewHashCache(s, root)
	if e != nil {
		t.Fatal(e)
	}
	m, e := ScanCached(context.Background(), root, "first", policy.Rules{}, nil, cache)
	if e != nil || m.ReadBytes != 4 {
		t.Fatal(m, e)
	}
	m, e = ScanCached(context.Background(), root, "resumed", policy.Rules{}, nil, cache)
	if e != nil || m.CachedFiles != 1 || m.ReadBytes != 0 {
		t.Fatal(m, e)
	}
	os.WriteFile(filepath.Join(root, "data"), []byte("DIFF"), 0600)
	updated, e := ScanCached(context.Background(), root, "changed", policy.Rules{}, nil, cache)
	if e != nil || updated.CachedFiles != 0 || updated.Contents[0].Hash == m.Contents[0].Hash {
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
