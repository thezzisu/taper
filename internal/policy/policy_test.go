package policy

import "testing"

func TestExplicitRules(t *testing.T) {
	r := Rules{Include: []string{"data/**", "keep.txt"}, Exclude: []string{"**/*.vmem", "data/cache/**"}}
	for p, want := range map[string]bool{"data/a": true, "data/inner/a": true, "keep.txt": true, "data/cache/a": false, "data/inner/a.vmem": false, "other/a": false} {
		if r.Keep(p) != want {
			t.Fatal(p, want)
		}
	}
	if !(Rules{}).Keep("Games/save.dat") {
		t.Fatal("implicit exclusion")
	}
}
