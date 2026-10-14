package engine

import "github.com/thezzisu/taper/internal/catalog"

type Replica struct {
	ID               string  `json:"id"`
	Media            string  `json:"media"`
	Label            string  `json:"label"`
	Data             uint64  `json:"data_position"`
	Blocks           uint64  `json:"blocks"`
	Verified         bool    `json:"verified"`
	Begin            uint64  `json:"begin_position"`
	Commit           uint64  `json:"commit_position"`
	ConfirmedBytes   *int64  `json:"confirmed_bytes"`
	ConfirmedRecords *int64  `json:"confirmed_records"`
	End              *uint64 `json:"end_position"`
}

func Replicas(s *catalog.Store, batch string) (out []Replica, err error) {
	rows, err := s.DB.Query("SELECT r.id,r.media_id,m.label,r.data_pos,r.blocks,r.verified IS NOT NULL,r.begin_pos,r.commit_pos,r.confirmed_bytes,r.confirmed_records,r.end_pos FROM replicas r JOIN media m ON m.id=r.media_id WHERE r.batch_id=? AND r.state='committed' ORDER BY r.verified IS NOT NULL DESC,m.state='open' DESC,r.id", batch)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var r Replica
		if err = rows.Scan(&r.ID, &r.Media, &r.Label, &r.Data, &r.Blocks, &r.Verified, &r.Begin, &r.Commit, &r.ConfirmedBytes, &r.ConfirmedRecords, &r.End); err != nil {
			return
		}
		out = append(out, r)
	}
	err = rows.Err()
	return
}
