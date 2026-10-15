package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
	"github.com/thezzisu/taper/internal/policy"
)

func verificationFixture(t *testing.T) (*catalog.Store, *media.Sim, []string) {
	t.Helper()
	s, d, root := setup(t)
	makeSource(t, root)
	if _, err := registerTestMember(s, d, "verify", nil, true, false); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < 3; i++ {
		if i == 2 {
			if err := d.Filemark(); err != nil {
				t.Fatal(err)
			}
		}
		m, err := scanSource(context.Background(), root, "archive", policy.Rules{})
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			r, e := ResolveReplicaSet(s, "")
			if e != nil {
				t.Fatal(e)
			}
			m.ReplicaID = r.ID
			if e = assignBatch(s, &m); e != nil {
				t.Fatal(e)
			}
			m.BeginPos++
			m.DataPos++
			m.CommitPos++
			m.EndPos++
		}
		if err = saveTestManifest(s, &m); err != nil {
			t.Fatal(err)
		}
		if _, err = WriteArchive(context.Background(), s, d, m, nil, WriteOptions{Memory: 8 << 20, Zstd: i == 1}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID.String())
	}
	return s, d, ids
}

func TestVerifySelectedRangesAndEOD(t *testing.T) {
	for _, selection := range []string{"whole", "middle", "last-two", "all-range", "same-id"} {
		t.Run(selection, func(t *testing.T) {
			s, d, ids := verificationFixture(t)
			var selected []string
			wantArchives, wantEOD := 3, true
			switch selection {
			case "middle":
				selected, wantArchives, wantEOD = []string{ids[1]}, 1, false
			case "last-two":
				selected, wantArchives = ids[1:], 2
			case "all-range":
				selected = []string{ids[0], ids[2]}
			case "same-id":
				selected, wantArchives = []string{ids[2], ids[2]}, 1
			}
			spy := &observedTape{Device: d}
			p, err := PlanVerification(s, spy, testRange(selected))
			if err != nil || len(p.Archives) != wantArchives || p.CheckEOD != wantEOD {
				t.Fatal(p, err)
			}
			if spy.reads+spy.locates+spy.keys+spy.writes+spy.marks != 0 {
				t.Fatal("planning touched tape records", spy)
			}
			result, err := VerifyTape(context.Background(), s, spy, p, nil, ReadOptions{Memory: 8 << 20, HardwareCompression: testCompression()})
			if err != nil || !result.Complete || result.Archives != wantArchives || result.EODChecked != wantEOD || result.ReachedEOD != wantEOD {
				t.Fatal(result, err)
			}
			wantReads := p.End - p.Start
			if wantEOD {
				wantReads++
			}
			if uint64(spy.reads) != wantReads || spy.locates != 1 || spy.ends+spy.writes+spy.marks != 0 {
				t.Fatal("read crossed approved bounds", spy, p)
			}
			var verified int
			if err = s.DB.QueryRow("SELECT COUNT(*) FROM replicas WHERE verified IS NOT NULL").Scan(&verified); err != nil || verified != wantArchives {
				t.Fatal("unselected replica changed", verified, err)
			}
			if result.Position == nil || *result.Position != p.End || result.Records != p.End-p.Start {
				t.Fatal("result position/count", result)
			}
		})
	}
}

func TestVerifyRejectsInvalidCatalogScopeBeforeMoving(t *testing.T) {
	s, d, ids := verificationFixture(t)
	for _, selected := range [][]string{{"bad"}, {ids[2], ids[0]}, {format.NewID().String()}} {
		spy := &observedTape{Device: d}
		if _, err := PlanVerification(s, spy, testRange(selected)); err == nil {
			t.Fatal("invalid selection admitted", selected)
		}
		if spy.reads+spy.locates+spy.keys != 0 {
			t.Fatal("invalid scope moved tape", spy)
		}
	}
	otherPath := filepath.Join(t.TempDir(), "other.tape")
	if err := media.CreateSim(otherPath, 8<<30, false); err != nil {
		t.Fatal(err)
	}
	other, err := media.OpenSim(otherPath, true)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err = registerTestMember(s, other, "other", nil, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err = prepareMedia(s, other, nil); err != nil {
		t.Fatal(err)
	}
	spy := &observedTape{Device: other}
	if _, err = PlanVerification(s, spy, ids[0]); err == nil {
		t.Fatal("wrong cartridge admitted", err)
	}
	if spy.reads+spy.locates+spy.keys != 0 {
		t.Fatal("wrong cartridge read", spy)
	}
	p, err := PlanVerification(s, d, testRange(ids[1:]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("UPDATE replicas SET begin_pos=begin_pos-1 WHERE batch_id=?", ids[1]); err != nil {
		t.Fatal(err)
	}
	spy = &observedTape{Device: d}
	if _, err = VerifyTape(context.Background(), s, spy, p, nil, ReadOptions{Memory: 8 << 20, HardwareCompression: testCompression()}); err == nil {
		t.Fatal("changed scope admitted")
	}
	if spy.reads+spy.locates+spy.keys != 0 {
		t.Fatal("changed scope moved tape", spy)
	}
}

type badGap struct {
	media.Device
	position uint64
}

func (d *badGap) ReadRecordInto(b []byte) (int, error) {
	p, _ := d.Device.Position()
	n, e := d.Device.ReadRecordInto(b)
	if p == d.position {
		copy(b, "unexpected record")
		return 17, nil
	}
	return n, e
}

type earlyEnd struct {
	media.Device
	position uint64
}

func (d *earlyEnd) ReadRecordInto(b []byte) (int, error) {
	p, _ := d.Device.Position()
	if p == d.position {
		return 0, media.ErrEnd
	}
	return d.Device.ReadRecordInto(b)
}

func TestVerifyRejectsCorruptionBoundariesAndTrailingRecords(t *testing.T) {
	for _, failure := range []string{"corrupt", "gap", "early-eod", "trailing-record", "trailing-filemark", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			s, d, ids := verificationFixture(t)
			p, e := PlanVerification(s, d, testRange(ids[1:]))
			if e != nil {
				t.Fatal(e)
			}
			var device media.Device = d
			ctx := context.Background()
			switch failure {
			case "corrupt":
				device = &observedTape{Device: d, corruptEnabled: true, corrupt: p.Archives[0].Replica.Data}
			case "gap":
				device = &badGap{d, *p.Archives[0].Replica.End}
			case "early-eod":
				device = &earlyEnd{d, p.Archives[1].Replica.Data}
			case "trailing-record":
				if e = d.WriteRecord([]byte("old tail")); e != nil {
					t.Fatal(e)
				}
			case "trailing-filemark":
				if e = d.Filemark(); e != nil {
					t.Fatal(e)
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			spy := &observedTape{Device: device}
			result, e := VerifyTape(ctx, s, spy, p, nil, ReadOptions{Memory: 8 << 20, HardwareCompression: testCompression()})
			if e == nil || result.Complete || result.ReachedEOD {
				t.Fatal("invalid stream accepted", result, e)
			}
			if uint64(spy.reads) > p.End-p.Start+1 || spy.locates > 1 {
				t.Fatal("unexpected read beyond limit", spy)
			}
			if failure == "trailing-record" || failure == "trailing-filemark" {
				if uint64(spy.reads) != p.End-p.Start+1 || !result.EODChecked {
					t.Fatal("did not check exactly one EOD object", result, spy)
				}
			}
			if failure == "cancelled" && (!errors.Is(e, context.Canceled) || spy.reads+spy.locates+spy.keys != 0) {
				t.Fatal("cancelled request accessed tape", spy, e)
			}
			var n int
			s.DB.QueryRow("SELECT COUNT(*) FROM replicas WHERE verified IS NOT NULL").Scan(&n)
			if n != 0 {
				t.Fatal("failed range marked verified")
			}
		})
	}
}
