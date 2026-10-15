package engine

import (
	"fmt"
	"github.com/thezzisu/taper/internal/catalog"
)

type Recommendation struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Serial    string `json:"serial,omitempty"`
	Barcode   string `json:"barcode,omitempty"`
	Remaining int64  `json:"write_budget_bytes"`
	Reason    string `json:"reason"`
}

func Recommend(s *catalog.Store, m Manifest) ([]Recommendation, error) {
	budget, e := ReplicaBudget(s, m.ReplicaID)
	if e != nil {
		return nil, e
	}
	r, e := ResolveReplicaSet(s, m.ReplicaID)
	if e != nil {
		return nil, e
	}
	reason := "fits all member budgets after reserving missing committed batches"
	if budget < Budget(m) {
		reason = "replica set cannot fit this batch; bind adequate media or choose another set"
	}
	return []Recommendation{{ID: r.ID, Label: r.Name, Remaining: budget, Reason: fmt.Sprintf("%s (%d members)", reason, r.Members)}}, nil
}
