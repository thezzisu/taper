package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTapeCompressionPreferenceAppliedBeforeEachWrite(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		s, d, root := setup(t)
		spy := &observedTape{Device: d}
		label, err := registerTestMember(s, spy, "test", nil, true, enabled)
		if err != nil || spy.compressions != 0 {
			t.Fatal("registration changed drive settings", err, spy)
		}
		var saved bool
		if err = s.DB.QueryRow("SELECT hardware_compression FROM media WHERE id=?", label.ID).Scan(&saved); err != nil || saved != enabled {
			t.Fatal("preference was not saved", saved, err)
		}
		for i := 0; i < 2; i++ {
			if err = os.WriteFile(filepath.Join(root, "file"), []byte{byte(i)}, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = StageAdd(context.Background(), s, root, nil, nil); err != nil {
				t.Fatal(err)
			}
			if _, err = StagePlan(context.Background(), s, "data", "", false, nil); err != nil {
				t.Fatal(err)
			}
			if err = d.SetCompression(!enabled); err != nil { // Another application changes the drive.
				t.Fatal(err)
			}
			beforeWrites, beforeMarks := spy.writes, spy.marks
			spy.compressionError = errors.New("compression setting failed")
			if _, _, err = StageWrite(context.Background(), s, spy, nil, WriteOptions{Memory: 8 << 20}); !errors.Is(err, spy.compressionError) {
				t.Fatal("compression failure was ignored", err)
			}
			if spy.writes != beforeWrites || spy.marks != beforeMarks {
				t.Fatal("data written before compression was configured")
			}
			spy.compressionError = nil
			if _, _, err = StageWrite(context.Background(), s, spy, nil, WriteOptions{Memory: 8 << 20}); err != nil {
				t.Fatal(err)
			}
			info, err := d.Info()
			if err != nil || info.Compression != enabled {
				t.Fatal("write did not use the saved setting", info.Compression, err)
			}
		}
	}
}

func TestReplicaCompressionIsAuthoritative(t *testing.T) {
	s, d, root := setup(t)
	m, e := registerTestMember(s, d, "test", nil, true, false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec("UPDATE media SET hardware_compression=NULL WHERE id=?", m.ID); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, "data"), []byte("data"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = StageAdd(context.Background(), s, root, nil, nil); e != nil {
		t.Fatal(e)
	}
	if _, e = StagePlan(context.Background(), s, "data", "", false, nil); e != nil {
		t.Fatal(e)
	}
	if _, _, e = StageWrite(context.Background(), s, d, nil, WriteOptions{Memory: 8 << 20}); e != nil {
		t.Fatal(e)
	}
	info, e := d.Info()
	if e != nil || info.Compression {
		t.Fatal("set compression not applied", info, e)
	}
}
