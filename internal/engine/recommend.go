package engine

import (
	"fmt"
	"github.com/thezzisu/taper/internal/catalog"
)

type Recommendation struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Remaining int64  `json:"remaining_native_bytes"`
	Reason    string `json:"reason"`
}

func Recommend(s *catalog.Store, m Manifest) (out []Recommendation, err error) {
	rows, err := s.DB.Query("SELECT id,label,total,remaining,sampled FROM media WHERE state='open' AND COALESCE((SELECT unhealthy FROM media_health WHERE media_id=media.id),0)=0 AND id NOT IN (SELECT media_id FROM replicas WHERE batch_id=? AND state='committed') ORDER BY remaining ASC", m.ID.String())
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var r Recommendation
		var total int64
		var sampled string
		if err = rows.Scan(&r.ID, &r.Label, &total, &r.Remaining, &sampled); err != nil {
			return
		}
		if r.Remaining-Reserve(total) >= Budget(m) {
			r.Reason = "fits native budget; capacity sampled " + sampled
			out = append(out, r)
		}
	}
	if err = rows.Err(); err != nil {
		return
	}
	if len(out) == 0 {
		out = append(out, Recommendation{Reason: fmt.Sprintf("insert a new tape with at least %d native bytes available, or manually split the batch", Budget(m))})
	}
	return
}
