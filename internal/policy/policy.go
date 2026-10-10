package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

type Rules struct {
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

func Load(name string) (r Rules, err error) {
	if name == "" {
		return r, nil
	}
	b, e := os.ReadFile(name)
	if e != nil {
		return r, e
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&r)
	if err == nil {
		var extra any
		if e := decoder.Decode(&extra); e != io.EOF {
			err = fmt.Errorf("rule file must contain exactly one JSON object")
		}
	}
	if err == nil {
		err = r.Validate()
	}
	return
}
func (r Rules) Validate() error {
	for _, list := range [][]string{r.Include, r.Exclude} {
		for _, p := range list {
			if p == "" || len(p) > 65536 || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
				return fmt.Errorf("invalid rule %q", p)
			}
			for _, c := range strings.Split(p, "/") {
				if c == "" || c == "." || c == ".." {
					return fmt.Errorf("invalid rule %q", p)
				}
			}
			if _, err := path.Match(p, ""); err != nil {
				return err
			}
		}
	}
	return nil
}
func matches(p, rule string) bool {
	prefix := strings.TrimSuffix(rule, "/**")
	if p == prefix || strings.HasPrefix(p, prefix+"/") {
		return true
	}
	var match func([]string, []string) bool
	seen := map[[2]int]bool{}
	results := map[[2]int]bool{}
	match = func(p, r []string) bool {
		key := [2]int{len(p), len(r)}
		if seen[key] {
			return results[key]
		}
		seen[key] = true
		var value bool
		defer func() { results[key] = value }()
		if len(r) == 0 {
			value = len(p) == 0
			return value
		}
		if r[0] == "**" {
			value = match(p, r[1:]) || len(p) > 0 && match(p[1:], r)
			return value
		}
		if len(p) == 0 {
			return false
		}
		ok, _ := path.Match(r[0], p[0])
		value = ok && match(p[1:], r[1:])
		return value
	}
	return match(strings.Split(p, "/"), strings.Split(rule, "/"))
}
func (r Rules) Keep(p string) bool {
	included := len(r.Include) == 0
	for _, x := range r.Include {
		if matches(p, x) {
			included = true
		}
	}
	for _, x := range r.Exclude {
		if matches(p, x) {
			return false
		}
	}
	return included
}
