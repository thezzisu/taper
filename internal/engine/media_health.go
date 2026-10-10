package engine

import (
	"encoding/json"
	"fmt"
	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/media"
	"time"
)

func recordMediaInfo(s *catalog.Store, id string, info media.Info) error {
	b, e := json.Marshal(info)
	if e != nil {
		return e
	}
	_, e = s.DB.Exec("INSERT INTO media_health VALUES(?,?,?,?) ON CONFLICT(media_id) DO UPDATE SET unhealthy=excluded.unhealthy,info=excluded.info,sampled=excluded.sampled", id, len(info.Alerts) > 0, string(b), time.Now().UTC().Format(time.RFC3339))
	return e
}
func checkMediaInfo(info media.Info) error {
	if len(info.Alerts) > 0 {
		return fmt.Errorf("tape has active TapeAlert flags %v; resolve them before writing", info.Alerts)
	}
	return nil
}
