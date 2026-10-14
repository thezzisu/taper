package engine

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestNestedVolumeFamilies(t *testing.T) {
	ctx := context.Background()
	s, _, root := setup(t)
	files := map[string][]byte{}
	for _, name := range []string{"split.zip", "split.z01", "split.z02", "split.z03", "rar3-old.rar", "rar3-old.r00", "rar3-old.r01"} {
		b, err := os.ReadFile("../archive/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		files["sub/"+name] = b
	}
	b, err := os.ReadFile("../archive/testdata/plain.7z")
	if err != nil {
		t.Fatal(err)
	}
	files["sub/split.7z.aa"], files["sub/split.7z.ab"] = b[:len(b)/2], b[len(b)/2:]
	if err = os.WriteFile(filepath.Join(root, "outer.zip"), zipData(t, files), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = StageAdd(ctx, s, root, nil, nil); err != nil {
		t.Fatal(err)
	}
	m, err := StagePlan(ctx, s, "volumes", 1, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ArchiveEntries(s, m.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"sub/split.zip!/payload.bin", "sub/split.7z.aa!/hello.txt"} {
		assertIndexedPath(t, rows, p)
	}
	for _, row := range rows {
		if row.Kind == "archive" && row.State != "complete" {
			t.Errorf("%s: %s %s", row.Path, row.State, row.Message)
		}
	}
}

func TestMissingVolumeAndVisibleEncryptedHeaders(t *testing.T) {
	for _, name := range []string{"split.zip", "rar3-old.rar", "encrypted.zip", "zip-encrypted-directory.zip"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s, _, root := setup(t)
			b, err := os.ReadFile("../archive/testdata/" + name)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(root, name), b, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = StageAdd(ctx, s, root, nil, nil); err != nil {
				t.Fatal(err)
			}
			m, err := StagePlan(ctx, s, "partial", 1, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := ArchiveEntries(s, m.ID.String())
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) == 0 || rows[0].State == "complete" {
				t.Fatalf("missing volume/password reported complete: %+v", rows)
			}
			if name == "encrypted.zip" {
				if rows[0].State != "locked" {
					t.Fatal(rows[0])
				}
				if err = StageUnlock(ctx, s, name, []byte("wrong"), true, nil); err == nil {
					t.Fatal("wrong password accepted")
				}
				if err = StageUnlock(ctx, s, name, []byte("test-password"), true, nil); err != nil {
					t.Fatal(err)
				}
			} else {
				summary, err := SummarizeArchiveIndex(s, m)
				if err != nil {
					t.Fatal(err)
				}
				if len(summary.Issues)+len(summary.Locked) == 0 {
					t.Fatal("incomplete archive was hidden from summary")
				}
			}
		})
	}
}

func TestDuplicateArchiveMemberNames(t *testing.T) {
	ctx := context.Background()
	s, _, root := setup(t)
	var out bytes.Buffer
	z := zip.NewWriter(&out)
	for i := 0; i < 3; i++ {
		w, err := z.Create("same.zip")
		if err != nil {
			t.Fatal(err)
		}
		w.Write(zipData(t, map[string][]byte{fmt.Sprint(i): []byte("data")}))
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "duplicate.zip"), out.Bytes(), 0600)
	if _, err := StageAdd(ctx, s, root, nil, nil); err != nil {
		t.Fatal(err)
	}
	m, err := StagePlan(ctx, s, "duplicates", 1, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ArchiveEntries(s, m.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatal("duplicate names lost or ambiguously expanded", rows)
	}
	for _, r := range rows[1:] {
		if r.Kind != "file" || r.Message == "" {
			t.Fatal(r)
		}
	}
}

func zipData(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	z := zip.NewWriter(&out)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestNestedIndexUnlockAndOfflineCatalog(t *testing.T) {
	ctx := context.Background()
	s, simulated, root := setup(t)
	locked, err := os.ReadFile("../archive/testdata/encrypted.7z")
	if err != nil {
		t.Fatal(err)
	}
	data := zipData(t, map[string][]byte{"locked.7z": locked, "another.zip": zipData(t, map[string][]byte{"中文/notes.txt": []byte("notes")})})
	if err = os.WriteFile(filepath.Join(root, "outer.zip"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = StageAdd(ctx, s, root, nil, nil); err != nil {
		t.Fatal(err)
	}
	m, err := StagePlan(ctx, s, "nested", 1, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := ArchiveEntries(s, m.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	summary, err := SummarizeArchiveIndex(s, m)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(summary.Locked, []string{"outer.zip!/locked.7z"}) {
		t.Fatal(summary)
	}
	assertIndexedPath(t, before, "another.zip!/中文/notes.txt")
	// A bad password must not erase the already known directory tree.
	if err = StageUnlock(ctx, s, "outer.zip!/locked.7z", []byte("wrong"), true, nil); err == nil {
		t.Fatal("wrong password accepted")
	}
	after, _ := ArchiveEntries(s, m.ID.String())
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed unlock changed the existing index")
	}
	if err = StageUnlock(ctx, s, "outer.zip!/locked.7z", []byte("test-password"), true, nil); err != nil {
		t.Fatal(err)
	}
	after, err = ArchiveEntries(s, m.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	assertIndexedPath(t, after, "locked.7z!/hello.txt")
	var stored []byte
	if err = s.DB.QueryRow("SELECT password FROM archive_entries WHERE batch_id=? AND path='locked.7z'", m.ID.String()).Scan(&stored); err != nil || string(stored) != "test-password" {
		t.Fatal("archive password was not retained in encrypted catalog", err)
	}
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(s.Path + suffix)
		if err == nil && bytes.Contains(b, []byte("test-password")) {
			t.Fatal("archive password leaked in catalog storage")
		}
	}
	// Cached planning succeeds without a usable extraction scratch directory.
	t.Setenv("TMPDIR", filepath.Join(root, "no-such-scratch"))
	again, err := StagePlan(ctx, s, "", 0, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != m.ID || again.Digest != m.Digest || Budget(again) != Budget(m) {
		t.Fatal("indexing changed the tape layout")
	}
	if _, err = RegisterMedia(s, simulated, "TEST", nil, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err = prepareMedia(s, simulated, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err = StageWrite(ctx, s, simulated, nil, WriteOptions{Memory: 8 << 20}); err != nil {
		t.Fatal(err)
	}
	if _, err = StageAdd(ctx, s, root, nil, nil); err != nil {
		t.Fatal(err)
	}
	reused, err := StagePlan(ctx, s, "reused", 1, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ArchiveEntries(s, reused.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	assertIndexedPath(t, rows, "locked.7z!/hello.txt")
	if err = os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	rows, err = ArchiveEntries(s, reused.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	assertIndexedPath(t, rows, "another.zip!/中文/notes.txt")
}

func assertIndexedPath(t *testing.T, entries []ArchiveEntry, p string) {
	t.Helper()
	for _, r := range entries {
		if r.Path == p {
			return
		}
	}
	t.Fatalf("missing indexed member %q: %+v", p, entries)
}

func TestIndexCompressedTarAndSplit7z(t *testing.T) {
	ctx := context.Background()
	s, _, root := setup(t)
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	body := zipData(t, map[string][]byte{"inside.txt": []byte("nested")})
	if err := tw.WriteHeader(&tar.Header{Name: "nested.zip", Size: int64(len(body)), Mode: 0600}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "tree.tgz"), out.Bytes(), 0600)
	plain, err := os.ReadFile("../archive/testdata/plain.7z")
	if err != nil {
		t.Fatal(err)
	}
	cut := len(plain) / 2
	os.WriteFile(filepath.Join(root, "split.7z.001"), plain[:cut], 0600)
	os.WriteFile(filepath.Join(root, "split.7z.002"), plain[cut:], 0600)
	if _, err = StageAdd(ctx, s, root, nil, nil); err != nil {
		t.Fatal(err)
	}
	m, err := StagePlan(ctx, s, "nested", 1, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ArchiveEntries(s, m.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	assertIndexedPath(t, rows, "tree.tar!/nested.zip!/inside.txt")
	assertIndexedPath(t, rows, "hello.txt")
	for _, r := range rows {
		if r.Kind == "archive" && r.State != "complete" {
			t.Fatalf("container incomplete: %+v", r)
		}
	}
}

func TestUnlockDoesNotRememberByDefault(t *testing.T) {
	ctx := context.Background()
	s, _, root := setup(t)
	b, err := os.ReadFile("../archive/testdata/encrypted.7z")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "locked.7z"), b, 0600)
	if _, err = StageAdd(ctx, s, root, nil, nil); err != nil {
		t.Fatal(err)
	}
	m, err := StagePlan(ctx, s, "locked", 1, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = StageUnlock(ctx, s, "locked.7z", []byte("test-password"), false, nil); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = s.DB.QueryRow("SELECT COUNT(*) FROM archive_entries WHERE password IS NOT NULL").Scan(&n); err != nil || n != 0 {
		t.Fatal("password retained without consent", n, err)
	}
	rows, err := ArchiveEntries(s, m.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	assertIndexedPath(t, rows, "hello.txt")
}

func TestArchiveMemberPathCannotEscapeOrCollideWithBoundary(t *testing.T) {
	ctx := context.Background()
	s, _, root := setup(t)
	b := zipData(t, map[string][]byte{"../outside.txt": []byte("safe"), "odd!/name.txt": []byte("bang")})
	os.WriteFile(filepath.Join(root, "paths.zip"), b, 0600)
	if _, err := StageAdd(ctx, s, root, nil, nil); err != nil {
		t.Fatal(err)
	}
	m, err := StagePlan(ctx, s, "paths", 1, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ArchiveEntries(s, m.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	assertIndexedPath(t, rows, "%2E%2E/outside.txt")
	assertIndexedPath(t, rows, "odd%21/name.txt")
	for _, r := range rows {
		if strings.HasPrefix(r.Path, "../") {
			t.Fatal("unsafe virtual path")
		}
	}
}

func TestNestedLegacyFilenameBytes(t *testing.T) {
	ctx := context.Background()
	s, _, root := setup(t)
	inner := zipData(t, map[string][]byte{"payload.txt": []byte("nested content")})
	part, err := os.ReadFile("../archive/testdata/plain.7z")
	if err != nil {
		t.Fatal(err)
	}
	const raw = "\xd6\xd0\xce\xc4"
	outer := zipData(t, map[string][]byte{
		raw + ".zip":       inner,
		"%D6%D0%CE%C4.zip": inner,
		"中文.zip":           inner,
		raw + ".7z.001":    part[:len(part)/2],
		raw + ".7z.002":    part[len(part)/2:],
	})
	if err = os.WriteFile(filepath.Join(root, "legacy.zip"), outer, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = StageAdd(ctx, s, root, nil, nil); err != nil {
		t.Fatal(err)
	}
	m, err := StagePlan(ctx, s, "legacy", 1, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ArchiveEntries(s, m.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"%D6%D0%CE%C4.zip!/payload.txt",
		"%25D6%25D0%25CE%25C4.zip!/payload.txt",
		"中文.zip!/payload.txt",
		"%D6%D0%CE%C4.7z.001!/hello.txt",
	} {
		assertIndexedPath(t, rows, path)
	}
	for _, row := range rows {
		if row.Kind == "archive" && row.State != "complete" {
			t.Errorf("%s: %s %s", row.Path, row.State, row.Message)
		}
	}
}

func TestUserArchivesStopAtFiles(t *testing.T) {
	ctx := context.Background()
	s, _, root := setup(t)
	// These all contain a valid ZIP header, which must not override their role
	// as files. Also exercise deduplication with identical, explicitly zipped data.
	packaged := zipData(t, map[string][]byte{"internal.xml": []byte("implementation details")})
	files := map[string][]byte{}
	for _, name := range []string{"manual.docx", "sheet.xlsx", "slides.pptx", "book.epub", "notes.odt", "app.apk", "library.jar", "setup.exe", "setup.msi", "data.cab", "disk.wim", "disk.iso", "animation.swf", "movie.flv", "unknown.bin"} {
		files[name] = packaged
		if err := os.WriteFile(filepath.Join(root, name), packaged, 0600); err != nil {
			t.Fatal(err)
		}
	}
	files["OST.zip"] = zipData(t, map[string][]byte{"Disc1/01 Opening.flac": []byte("music"), "cover.jpg": []byte("cover")})
	game := zipData(t, files)
	history := zipData(t, map[string][]byte{"game.zip": game})
	backup := zipData(t, map[string][]byte{"older.zip": history, "today.txt": []byte("new data")})
	if err := os.WriteFile(filepath.Join(root, "backup.zip"), backup, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "explicit.zip"), packaged, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := StageAdd(ctx, s, root, nil, nil); err != nil {
		t.Fatal(err)
	}
	m, err := StagePlan(ctx, s, "user-archives", 1, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ArchiveEntries(s, m.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	const gamePrefix = "older.zip!/game.zip!/"
	for name := range files {
		assertIndexedPath(t, rows, gamePrefix+name)
		if name == "OST.zip" {
			continue
		}
		for _, row := range rows {
			if strings.HasPrefix(row.Path, gamePrefix+name+"!/") {
				t.Fatalf("leaf file was expanded: %+v", row)
			}
		}
	}
	assertIndexedPath(t, rows, gamePrefix+"OST.zip!/Disc1/01 Opening.flac")
	assertIndexedPath(t, rows, "internal.xml") // explicit.zip still exposes real members.
	var originalFiles int
	for _, c := range m.Contents {
		originalFiles += len(c.Files)
	}
	if originalFiles != len(files)+1 { // 15 leaf files, backup.zip and explicit.zip.
		t.Fatalf("backup selection changed: %d files", originalFiles)
	}
}

func TestArchiveSuffixDoesNotExposeFileInternals(t *testing.T) {
	ctx := context.Background()
	s, _, root := setup(t)
	compressed, err := os.ReadFile("../archive/testdata/formats/SWF/plain.swf.gz")
	if err != nil {
		t.Fatal(err)
	}
	z, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	flash, err := io.ReadAll(z)
	z.Close()
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"misnamed.zip": flash,
		"outer.zip":    zipData(t, map[string][]byte{"animation.zip": flash, "OST/track.flac": []byte("music")}),
	} {
		if err = os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = StageAdd(ctx, s, root, nil, nil); err != nil {
		t.Fatal(err)
	}
	m, err := StagePlan(ctx, s, "suffix", 1, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ArchiveEntries(s, m.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	assertIndexedPath(t, rows, "animation.zip")
	assertIndexedPath(t, rows, "OST/track.flac")
	if len(rows) != 3 {
		t.Fatalf("file internals were indexed: %+v", rows)
	}
	for _, row := range rows {
		if m.Contents[row.Ordinal].Files[0].Path != "outer.zip" || (row.Kind == "archive" && row.State != "complete") {
			t.Fatalf("opaque member damaged the enclosing index: %+v", row)
		}
	}
}
