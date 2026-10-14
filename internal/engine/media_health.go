package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
	"time"
)

func recordMediaFailure(s *catalog.Store, id string, cause error) error {
	// Keep the last measurements and their timestamp, but prohibit a budget
	// until a later successful sample establishes current device state.
	_, blocked := s.DB.Exec("UPDATE media_health SET unhealthy=1 WHERE media_id=?", id)
	b, _ := json.Marshal(map[string]string{"sample_error": cause.Error()})
	_, recorded := s.DB.Exec("INSERT INTO media_samples(media_id,sampled,info) VALUES(?,?,?)", id, time.Now().UTC().Format(time.RFC3339Nano), string(b))
	return errors.Join(blocked, recorded)
}

func recordMediaInfo(s *catalog.Store, id string, info media.Info) error {
	if _, e := time.Parse(time.RFC3339Nano, info.Sampled); e != nil {
		return fmt.Errorf("missing device sample time: %w", e)
	}
	b, e := json.Marshal(info)
	if e != nil {
		return e
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("UPDATE media SET total=?,remaining=?,sampled=? WHERE id=?", info.Total, info.Remaining, info.Sampled, id); e != nil {
		return e
	}
	if _, e = tx.Exec("INSERT INTO media_health VALUES(?,?,?,?) ON CONFLICT(media_id) DO UPDATE SET unhealthy=excluded.unhealthy,info=excluded.info,sampled=excluded.sampled", id, len(info.Alerts) > 0 || info.EarlyWarning, string(b), info.Sampled); e != nil {
		return e
	}
	if _, e = tx.Exec("INSERT INTO media_samples(media_id,sampled,info) VALUES(?,?,?)", id, info.Sampled, string(b)); e != nil {
		return e
	}
	return tx.Commit()
}
func checkMediaInfo(info media.Info) error {
	if info.MaxRecord < format.BlockSize {
		return fmt.Errorf("drive maximum record %d is below required %d bytes", info.MaxRecord, format.BlockSize)
	}
	if info.EarlyWarning {
		return fmt.Errorf("tape is at early-warning end of partition; no further archive writes")
	}
	if len(info.Alerts) > 0 {
		return fmt.Errorf("tape has active TapeAlert flags %v; resolve them before writing", info.Alerts)
	}
	return nil
}
