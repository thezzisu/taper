package engine

import "github.com/thezzisu/taper/internal/catalog"

type Status struct {
	Members                  int   `json:"member_tapes"`
	Committed                int   `json:"committed_tapes"`
	Verified                 int   `json:"verified_tapes"`
	Covered                  bool  `json:"all_members_covered"`
	HistoricalDuplicates     int   `json:"historical_duplicate_contents"`
	HistoricalDuplicateBytes int64 `json:"historical_duplicate_bytes"`
}

func BatchStatus(s *catalog.Store, m Manifest) (v Status, err error) {
	err = s.DB.QueryRow("SELECT COUNT(*) FROM media WHERE replica_id=?", m.ReplicaID).Scan(&v.Members)
	if err != nil {
		return
	}
	err = s.DB.QueryRow("SELECT COUNT(DISTINCT media_id),COUNT(DISTINCT CASE WHEN verified IS NOT NULL THEN media_id END) FROM replicas WHERE batch_id=? AND state='committed'", m.ID.String()).Scan(&v.Committed, &v.Verified)
	if err != nil {
		return
	}
	v.Covered = v.Members > 0 && v.Committed == v.Members
	err = s.DB.QueryRow("SELECT COUNT(*),COALESCE(SUM(c.size),0) FROM contents c WHERE c.batch_id=? AND EXISTS(SELECT 1 FROM contents old JOIN batches b ON b.id=old.batch_id WHERE old.batch_id<>c.batch_id AND old.hash=c.hash AND old.size=c.size AND b.state='committed')", m.ID.String()).Scan(&v.HistoricalDuplicates, &v.HistoricalDuplicateBytes)
	return
}
