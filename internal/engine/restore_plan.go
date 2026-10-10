package engine

import (
	"errors"
	"github.com/thezzisu/taper/internal/format"
)

type Interval struct {
	First uint64 `json:"first_record"`
	Last  uint64 `json:"last_record"`
}
type RestorePlan struct {
	Files       int        `json:"files"`
	OutputBytes int64      `json:"output_bytes"`
	UniqueBytes int64      `json:"unique_content_bytes"`
	Records     uint64     `json:"records_to_read"`
	Intervals   []Interval `json:"relative_record_intervals"`
	Replicas    []Replica  `json:"replicas"`
}

func PlanRestore(m Manifest, prefixes []string, replicas []Replica) (p RestorePlan, err error) {
	if err = ValidateManifest(m); err != nil {
		return
	}
	p.Replicas = replicas
	for _, c := range m.Contents {
		files := selected(c, prefixes).Files
		if len(files) == 0 {
			continue
		}
		p.Files += len(files)
		p.OutputBytes += int64(len(files)) * c.Size
		p.UniqueBytes += c.Size
		if c.Size == 0 {
			continue
		}
		i := Interval{uint64(c.Data / format.PayloadSize), uint64((c.Data + c.Size - 1) / format.PayloadSize)}
		if n := len(p.Intervals); n > 0 && i.First <= p.Intervals[n-1].Last+256 {
			p.Intervals[n-1].Last = max(i.Last, p.Intervals[n-1].Last)
		} else {
			p.Intervals = append(p.Intervals, i)
		}
	}
	if p.Files == 0 {
		return p, errors.New("no files match requested prefixes")
	}
	for _, i := range p.Intervals {
		p.Records += i.Last - i.First + 1
	}
	return
}
