package engine

import (
	"errors"
	"fmt"
	"strings"
)

func ParseArchiveRange(spec string, ordered []BatchMeta) ([]BatchMeta, error) {
	if len(ordered) == 0 {
		if spec == "" {
			return []BatchMeta{}, nil
		}
		return nil, errors.New("no archives are available")
	}
	if spec == "" {
		return append([]BatchMeta(nil), ordered...), nil
	}
	if spec == "-" || strings.Count(spec, "-") > 1 {
		return nil, errors.New("archive range must be FIRST, FIRST-LAST, FIRST-, or -LAST")
	}
	firstSpec, lastSpec := spec, spec
	if i := strings.IndexByte(spec, '-'); i >= 0 {
		firstSpec, lastSpec = spec[:i], spec[i+1:]
	}
	find := func(token string, first bool) (int, error) {
		if token == "" {
			if first {
				return 0, nil
			}
			return len(ordered) - 1, nil
		}
		if len(token) < 6 || len(token) > 32 {
			return -1, fmt.Errorf("archive selector %q must be a 6–32 character hex prefix", token)
		}
		for _, r := range token {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return -1, fmt.Errorf("archive selector %q is not hexadecimal", token)
			}
		}
		match := -1
		lower := strings.ToLower(token)
		for i, b := range ordered {
			id := strings.ToLower(b.ID)
			if strings.HasPrefix(id, lower) {
				if match >= 0 {
					return -1, fmt.Errorf("archive selector %q is ambiguous", token)
				}
				match = i
			}
		}
		if match < 0 {
			return -1, fmt.Errorf("archive selector %q did not match an archive", token)
		}
		return match, nil
	}
	a, e := find(firstSpec, true)
	if e != nil {
		return nil, e
	}
	b, e := find(lastSpec, false)
	if e != nil {
		return nil, e
	}
	if a > b {
		return nil, errors.New("archive range is reversed")
	}
	return append([]BatchMeta(nil), ordered[a:b+1]...), nil
}
