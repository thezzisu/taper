package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/thezzisu/taper/internal/media"
	"github.com/thezzisu/taper/internal/policy"
)

type capacityDevice struct {
	media.Device
	info media.Info
}

type sampleFailureDevice struct {
	media.Device
	failed        bool
	beforeFailure func()
}

func (d *sampleFailureDevice) Filemark() error {
	if err := d.Device.Filemark(); err != nil {
		return err
	}
	d.failed = true
	return nil
}
func (d *sampleFailureDevice) Info() (media.Info, error) {
	if d.failed {
		if d.beforeFailure != nil {
			d.beforeFailure()
		}
		return media.Info{}, errors.New("sample unavailable after committed write")
	}
	return d.Device.Info()
}

func TestMissingPostWriteSampleDoesNotReuseOldFreeSpace(t *testing.T) {
	s, d, root := setup(t)
	makeSource(t, root)
	l, err := RegisterMedia(s, d, "test", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prepareMedia(s, d, nil); err != nil {
		t.Fatal(err)
	}
	m, err := scanSource(context.Background(), root, "first", policy.Rules{})
	if err != nil {
		t.Fatal(err)
	}
	if err = Save(s, m); err != nil {
		t.Fatal(err)
	}
	device := &sampleFailureDevice{Device: d, beforeFailure: func() {
		var state string
		var blocked bool
		if err := s.DB.QueryRow("SELECT r.state,h.unhealthy FROM replicas r JOIN media_health h ON h.media_id=r.media_id WHERE r.batch_id=?", m.ID.String()).Scan(&state, &blocked); err != nil || state != "committed" || !blocked {
			t.Fatal("commit left pre-write capacity eligible before final sample", state, blocked, err)
		}
	}}
	id, err := WriteArchive(context.Background(), s, device, m, nil, WriteOptions{Memory: 8 << 20})
	if err == nil || !strings.Contains(err.Error(), "sample unavailable") {
		t.Fatal("missing sample ignored", err)
	}
	var state string
	var n int64
	if err = s.DB.QueryRow("SELECT state,confirmed_bytes FROM replicas WHERE id=?", id).Scan(&state, &n); err != nil || state != "committed" || n != Budget(m) {
		t.Fatal("durable commit facts lost", state, n, err)
	}
	var blocked bool
	if err = s.DB.QueryRow("SELECT unhealthy FROM media_health WHERE media_id=?", l.ID).Scan(&blocked); err != nil || !blocked {
		t.Fatal("stale pre-write capacity remained eligible", blocked, err)
	}
	next := m
	next.ID[0] ^= 1
	if err = Save(s, next); err != nil {
		t.Fatal(err)
	}
	recs, err := Recommend(s, next)
	if err != nil || len(recs) != 1 || recs[0].ID != "" {
		t.Fatal("stale free capacity reused", recs, err)
	}
}

func TestDeviceMeasurementsAreImmutableHistory(t *testing.T) {
	s, d, _ := setup(t)
	l, err := RegisterMedia(s, d, "test", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	info, err := d.Info()
	if err != nil {
		t.Fatal(err)
	}
	info.Total, info.Remaining, info.NativeLimit = 2499053<<20, 267911<<20, 2_500_000_000_000
	info.Sampled = "2026-01-02T03:04:05.123456789Z"
	if err = recordMediaInfo(s, l.ID, info); err != nil {
		t.Fatal(err)
	}
	info.Remaining = 110320 << 20
	info.EarlyWarning = true
	info.Sampled = "2026-01-02T03:05:05.987654321Z"
	if err = recordMediaInfo(s, l.ID, info); err != nil {
		t.Fatal(err)
	}
	var saved, stamp string
	if err = s.DB.QueryRow("SELECT info,sampled FROM media_samples WHERE media_id=? AND sampled=?", l.ID, "2026-01-02T03:04:05.123456789Z").Scan(&saved, &stamp); err != nil {
		t.Fatal(err)
	}
	var older media.Info
	if err = json.Unmarshal([]byte(saved), &older); err != nil {
		t.Fatal(err)
	}
	if older.Total != 2499053<<20 || older.Remaining != 267911<<20 || older.Sampled != stamp || older.EarlyWarning {
		t.Fatal("historical device facts changed", older)
	}
	var total, remaining int64
	if err = s.DB.QueryRow("SELECT total,remaining,sampled FROM media WHERE id=?", l.ID).Scan(&total, &remaining, &stamp); err != nil {
		t.Fatal(err)
	}
	if total != info.Total || remaining != info.Remaining || stamp != info.Sampled {
		t.Fatal("budget or save time replaced raw measurements", total, remaining, stamp)
	}
}

type byteObserved struct {
	media.Device
	bytes   int64
	records int64
}

func (d *byteObserved) WriteRecord(b []byte) error {
	if err := d.Device.WriteRecord(b); err != nil {
		return err
	}
	d.bytes += int64(len(b))
	d.records++
	return nil
}

func TestExactEncodedBytesAndDeviceBoundaries(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		s, d, root := setup(t)
		makeSource(t, root)
		if _, err := RegisterMedia(s, d, "test", nil, false, false); err != nil {
			t.Fatal(err)
		}
		if _, err := prepareMedia(s, d, nil); err != nil {
			t.Fatal(err)
		}
		m, err := scanSource(context.Background(), root, "quoted \"name\" 磁带", policy.Rules{})
		if err != nil {
			t.Fatal(err)
		}
		if err = Save(s, m); err != nil {
			t.Fatal(err)
		}
		spy := &byteObserved{Device: d}
		id, err := WriteArchive(context.Background(), s, spy, m, nil, WriteOptions{Memory: 8 << 20, Zstd: compressed})
		if err != nil {
			t.Fatal(err)
		}
		if (!compressed && spy.bytes != Budget(m)) || spy.bytes > Budget(m) {
			t.Fatal("record budget differs from serialized device writes", spy.bytes, Budget(m))
		}
		var n, records int64
		var end uint64
		if err = s.DB.QueryRow("SELECT confirmed_bytes,confirmed_records,end_pos FROM replicas WHERE id=?", id).Scan(&n, &records, &end); err != nil {
			t.Fatal(err)
		}
		position, _ := d.Position()
		if n != spy.bytes || records != spy.records || end != position {
			t.Fatal("device-confirmed facts lost", n, records, end, spy.bytes, spy.records, position)
		}
		if result, err := verifyAll(context.Background(), s, d, nil, ReadOptions{Memory: 8 << 20}); err != nil || result.Archives != 1 {
			t.Fatal("reader disagreed with exact write counts", result, err)
		}
	}
}

func (d *capacityDevice) Info() (media.Info, error) { return d.info, nil }

func TestCapacityAndEarlyWarningStopBeforeAppend(t *testing.T) {
	s, d, root := setup(t)
	makeSource(t, root)
	label, err := RegisterMedia(s, d, "test", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prepareMedia(s, d, nil); err != nil {
		t.Fatal(err)
	}
	m, err := scanSource(context.Background(), root, "pending", policy.Rules{})
	if err != nil {
		t.Fatal(err)
	}
	if err = Save(s, m); err != nil {
		t.Fatal(err)
	}
	info, err := d.Info()
	if err != nil {
		t.Fatal(err)
	}
	for _, warning := range []bool{false, true} {
		info.Remaining = Reserve(info.Total) + Budget(m) - 1
		info.EarlyWarning = warning
		if warning {
			info.Remaining = info.Total
		} // A positive capacity counter must not override EOP.
		spy := &observedTape{Device: d}
		device := &capacityDevice{Device: spy, info: info}
		_, err = WriteArchive(context.Background(), s, device, m, nil, WriteOptions{Memory: 8 << 20})
		if err == nil || warning && !strings.Contains(err.Error(), "early-warning") {
			t.Fatal("unsafe append allowed", err)
		}
		if spy.writes != 0 || spy.marks != 0 || spy.ends != 0 || spy.locates != 0 || spy.reads != 0 {
			t.Fatal("rejected archive touched tape records", spy)
		}
	}
	var unhealthy bool
	if err = s.DB.QueryRow("SELECT unhealthy FROM media_health WHERE media_id=?", label.ID).Scan(&unhealthy); err != nil || !unhealthy {
		t.Fatal("early warning not retained offline", unhealthy, err)
	}
	recommendations, err := Recommend(s, m)
	if err != nil || len(recommendations) != 1 || recommendations[0].ID != "" {
		t.Fatal("early-warning tape recommended", recommendations, err)
	}
}
