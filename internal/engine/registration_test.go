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
		label, err := RegisterMedia(s, spy, "test", nil, true, enabled)
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
			if _, err = StagePlan(context.Background(), s, "data", 1, false, nil); err != nil {
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

func TestUnknownCompressionPreferenceRequiresRegistration(t *testing.T) {
	s, d, root := setup(t)
	l, err := RegisterMedia(s, d, "test", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prepareMedia(s, d, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("UPDATE media SET hardware_compression=NULL WHERE id=?", l.ID); err != nil {
		t.Fatal(err)
	}
	spy := &observedTape{Device: d}
	if _, err = prepareMedia(s, spy, nil); err == nil {
		t.Fatal("unknown preference guessed")
	}
	if spy.keys != 0 || spy.compressions != 0 || spy.writes != 0 || spy.locates != 0 {
		t.Fatal("unknown preference touched tape settings", spy)
	}
	if err = os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = StageAdd(context.Background(), s, root, nil, nil); err != nil {
		t.Fatal(err)
	}
	m, err := StagePlan(context.Background(), s, "data", 1, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := Recommend(s, m)
	if err != nil || len(recs) != 1 || recs[0].ID != "" {
		t.Fatal("unknown preference recommended", recs, err)
	}
	if _, err = RegisterMedia(s, spy, "test", nil, false, false); err != nil {
		t.Fatal(err)
	}
	if spy.compressions != 0 {
		t.Fatal("registration changed hardware")
	}
	if _, err = prepareMedia(s, spy, nil); err != nil {
		t.Fatal(err)
	}
}
