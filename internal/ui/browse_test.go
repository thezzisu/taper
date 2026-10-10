package ui

import (
	"bytes"
	"github.com/thezzisu/taper/internal/engine"
	"github.com/thezzisu/taper/internal/format"
	"strings"
	"testing"
)

func TestBrowseRootAndPrefix(t *testing.T) {
	m := engine.Manifest{ID: format.NewID(), Name: "test", Contents: []engine.Content{{Size: 5, Files: []engine.File{{Path: "dir/data"}}}}}
	var out bytes.Buffer
	if e := Browse(m, nil, strings.NewReader("p\n\n1\np\n\nu\nq\n"), &out); e != nil {
		t.Fatal(e)
	}
	s := out.String()
	if !strings.Contains(s, "--batch "+m.ID.String()+" 'dir'") || strings.Contains(s, "--batch "+m.ID.String()+" \"\"") {
		t.Fatal("bad restore guidance", s)
	}
}
