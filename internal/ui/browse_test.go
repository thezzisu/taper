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
	if e := Browse(m, nil, nil, strings.NewReader("p\n\n1\np\n\nu\nq\n"), &out); e != nil {
		t.Fatal(e)
	}
	s := out.String()
	if !strings.Contains(s, "Archive: "+m.ID.String()) || !strings.Contains(s, "Path: /dir") {
		t.Fatal("bad offline details", s)
	}
}

func TestBrowseUsesUnifiedTapeStrip(t *testing.T) {
	m := engine.Manifest{ID: format.NewID(), Name: "browse", Contents: []engine.Content{{Size: 1, Files: []engine.File{{Path: "x"}}}}}
	end := uint64(6)
	reps := []engine.Replica{{Label: "123456L6", Media: "media", End: &end}}
	var out bytes.Buffer
	if err := Browse(m, reps, nil, strings.NewReader("q\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "123456L6") || !strings.Contains(out.String(), "unverified") {
		t.Fatal(out.String())
	}
}

func TestBrowseNestedCatalogTree(t *testing.T) {
	m := engine.Manifest{ID: format.NewID(), Name: "nested", Contents: []engine.Content{{Size: 20, Files: []engine.File{{Path: "outer.zip"}}}}}
	index := []engine.ArchiveEntry{{Ordinal: 0, Kind: "archive", State: "complete"}, {Ordinal: 0, Entry: "0", Path: "notes.txt", Kind: "file", Size: 5}}
	var out bytes.Buffer
	if err := Browse(m, nil, index, strings.NewReader("2\np\n\nq\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "notes.txt") || !strings.Contains(out.String(), "Container: outer.zip") {
		t.Fatal("nested offline tree missing", out.String())
	}
}

func TestBrowseFilesWithArchiveDuplicate(t *testing.T) {
	m := engine.Manifest{Contents: []engine.Content{{Size: 20, Files: []engine.File{{Path: "report.docx"}, {Path: "app.apk"}, {Path: "setup.exe"}, {Path: "unknown.bin"}, {Path: "copy.zip"}}}}}
	index := []engine.ArchiveEntry{{Ordinal: 0, Kind: "archive", State: "complete"}, {Ordinal: 0, Entry: "0", Path: "word/document.xml", Kind: "file", Size: 5}}
	var paths []string
	for _, entry := range browseEntries(m, index) {
		paths = append(paths, entry.path)
	}
	got := strings.Join(paths, "\n")
	if !strings.Contains(got, "copy.zip!/word/document.xml") {
		t.Fatal(got)
	}
	for _, name := range []string{"report.docx", "app.apk", "setup.exe", "unknown.bin"} {
		if !strings.Contains(got, name+"\n") || strings.Contains(got, name+"!/") {
			t.Fatal(got)
		}
	}
}
