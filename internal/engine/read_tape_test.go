package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/media"
	"github.com/thezzisu/taper/internal/policy"
)

func TestWholeTapeVerifyAndReindexStaySequential(t *testing.T) {
	s, d, root := setup(t)
	makeSource(t, root)
	registerTestMember(s, d, "test", nil, true, true)
	var ids []string
	var middle uint64
	for i := 0; i < 3; i++ {
		m, e := scanSource(context.Background(), root, "archive", policy.Rules{})
		if e != nil {
			t.Fatal(e)
		}
		if e = saveTestManifest(s, &m); e != nil {
			t.Fatal(e)
		}
		if _, e = WriteArchive(context.Background(), s, d, m, nil, WriteOptions{Memory: 8 << 20, Zstd: i == 1}); e != nil {
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
		var result TapeResult
		var e error
		if reindex {
			result, e = ReindexTape(context.Background(), target, spy, nil, ReadOptions{Memory: 8 << 20, HardwareCompression: testCompression()})
		} else {
			result, e = verifyAll(context.Background(), target, spy, nil, ReadOptions{Memory: 8 << 20, HardwareCompression: testCompression()})
		}
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
	result, e := ReindexTape(context.Background(), fresh, spy, nil, ReadOptions{Memory: 8 << 20, HardwareCompression: testCompression()})
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
	registerTestMember(s, d, "keyed", key, true, true)
	m, e := scanSource(context.Background(), root, "keyed", policy.Rules{})
	if e != nil {
		t.Fatal(e)
	}
	saveTestManifest(s, &m)
	if _, e = WriteArchive(context.Background(), s, d, m, nil, WriteOptions{Memory: 8 << 20}); e != nil {
		t.Fatal(e)
	}
	result, e := verifyAll(context.Background(), s, d, nil, ReadOptions{Memory: 8 << 20, HardwareCompression: testCompression()})
	if e != nil || !result.ReachedEOD {
		t.Fatal(result, e)
	}
}

func verifyAll(ctx context.Context, s *catalog.Store, d media.Device, key []byte, o ReadOptions) (TapeResult, error) {
	p, e := PlanVerification(s, d, "")
	if e != nil {
		return TapeResult{}, e
	}
	return VerifyTape(ctx, s, d, p, key, o)
}
