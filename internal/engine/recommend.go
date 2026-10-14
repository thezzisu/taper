package engine

import (
	"encoding/json"
	"fmt"
	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/media"
	"math"
)

type Recommendation struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Serial    string `json:"serial,omitempty"`
	Barcode   string `json:"barcode,omitempty"`
	Remaining int64  `json:"write_budget_bytes"`
	Reason    string `json:"reason"`
}

func Recommend(s *catalog.Store, m Manifest) (out []Recommendation, err error) {
	var required, committed int
	err = s.DB.QueryRow("SELECT required_replicas,(SELECT COUNT(DISTINCT media_id) FROM replicas WHERE batch_id=batches.id AND state='committed') FROM batches WHERE id=?", m.ID.String()).Scan(&required, &committed)
	if err != nil {
		return nil, err
	}
	if committed >= required {
		return []Recommendation{}, nil
	}
	rows, err := s.DB.Query("SELECT id,label,serial,total,remaining,sampled,COALESCE((SELECT info FROM media_health WHERE media_id=media.id),'{}') FROM media WHERE state IN ('open','registered') AND hardware_compression IS NOT NULL AND COALESCE((SELECT unhealthy FROM media_health WHERE media_id=media.id),0)=0 AND id NOT IN (SELECT media_id FROM replicas WHERE state IN ('writing','failed')) AND id NOT IN (SELECT media_id FROM replicas WHERE batch_id=? AND state='committed') ORDER BY remaining ASC", m.ID.String())
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var r Recommendation
		var total int64
		var sampled string
		var storedInfo string
		if err = rows.Scan(&r.ID, &r.Label, &r.Serial, &total, &r.Remaining, &sampled, &storedInfo); err != nil {
			return
		}
		var info media.Info
		if err = json.Unmarshal([]byte(storedInfo), &info); err != nil {
			return nil, err
		}
		r.Barcode = info.Barcode
		info.Total, info.Remaining = total, r.Remaining
		limit, available, e := info.Budget()
		if e != nil {
			continue
		}
		if available-Reserve(limit) >= Budget(m) {
			r.Remaining = available - Reserve(limit)
			r.Reason = "fits record-byte budget from device sample " + sampled
			out = append(out, r)
		}
	}
	if err = rows.Err(); err != nil {
		return
	}
	if len(out) == 0 {
		budget := Budget(m)
		reserve := max(int64(2<<30), 1+(budget-1)/99)
		if budget > math.MaxInt64-reserve {
			return nil, fmt.Errorf("archive and reserve exceed supported integer range")
		}
		minimum := budget + reserve
		out = append(out, Recommendation{Reason: fmt.Sprintf("insert a new tape with at least %d native bytes total including reserve, or manually split the batch", minimum)})
	}
	return
}
