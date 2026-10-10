package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/policy"
)

func TestWholeTapeVerifyAndReindexStaySequential(t *testing.T) {
	s, d, root := setup(t)
	makeSource(t, root)
	RegisterMedia(s, d, "test", nil, true)
	var ids []string
	var middle uint64
	for i := 0; i < 3; i++ {
		m, e := Scan(context.Background(), root, "archive", policy.Rules{})
		if e != nil {
			t.Fatal(e)
		}
		if e = Save(s, m); e != nil {
			t.Fatal(e)
		}
		if _, e = WriteArchive(context.Background(), s, d, m, nil, WriteOptions{Memory: 8 << 20, AllowFrozen: true, Zstd: i == 1}); e != nil {
			t.Fatal(e)
		}
		rs, _ := Replicas(s, m.ID.String())
		if i == 1 {
			middle = rs[0].Data
		}
		ids = append(ids, m.ID.String())
	}
	end, _ := d.Position()
	for _, reindex := range []bool{false, true} {
		target := s
		if reindex {
			var e error
			target, e = catalog.Create(filepath.Join(t.TempDir(), "fresh.db"), []byte("test"))
			if e != nil {
				t.Fatal(e)
			}
			defer target.Close()
		}
		spy := &observedTape{Device: d}
		result, e := ScanTape(context.Background(), target, spy, nil, reindex, ReadOptions{Memory: 8 << 20})
		if e != nil || !result.ReachedEOD || result.Archives != 3 || result.Records != end {
			t.Fatal(result, e)
		}
		if spy.locates != 1 || spy.ends != 0 || spy.writes != 0 || uint64(spy.reads) != end+1 {
			t.Fatalf("scan was not one complete forward pass: %+v", spy)
		}
	}
	fresh, e := catalog.Create(filepath.Join(t.TempDir(), "damaged.db"), []byte("test"))
	if e != nil {
		t.Fatal(e)
	}
	defer fresh.Close()
	spy := &observedTape{Device: d, corruptEnabled: true, corrupt: middle}
	result, e := ScanTape(context.Background(), fresh, spy, nil, true, ReadOptions{Memory: 8 << 20})
	if e == nil || !result.ReachedEOD || result.Imported != 2 || result.Records != end || spy.locates != 1 {
		t.Fatal("corrupt middle archive prevented full sequential scan", result, e, spy.locates)
	}
	for _, id := range []string{ids[0], ids[2]} {
		if _, e = Load(fresh, id); e != nil {
			t.Fatal("intact archive lost", e)
		}
	}
	if _, e = Load(fresh, ids[1]); e == nil {
		t.Fatal("corrupt archive imported")
	}
}

func TestWholeTapeUsesSavedKey(t *testing.T) {
	s, d, root := setup(t)
	os.WriteFile(filepath.Join(root, "secret"), []byte("test"), 0600)
	key := make([]byte, 32)
	key[0] = 19
	RegisterMedia(s, d, "keyed", key, true)
	m, e := Scan(context.Background(), root, "keyed", policy.Rules{})
	if e != nil {
		t.Fatal(e)
	}
	Save(s, m)
	if _, e = WriteArchive(context.Background(), s, d, m, nil, WriteOptions{Memory: 8 << 20, AllowFrozen: true}); e != nil {
		t.Fatal(e)
	}
	result, e := ScanTape(context.Background(), s, d, nil, false, ReadOptions{Memory: 8 << 20})
	if e != nil || !result.ReachedEOD {
		t.Fatal(result, e)
	}
}
