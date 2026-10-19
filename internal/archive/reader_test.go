package archive

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Golden digests are from native 7-Zip extraction, not the embedded bridge.
// Fixtures cover distinct codecs, crypto modes and native split ZIP/CAB sets.
func TestFormatCorpus(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, manifest := range []string{"cases.json", "format-cases.json"} {
		var cases []struct {
			Name, Password, SHA256, Format string
			Bytes                          int64
			Warning                        bool
		}
		b, err := os.ReadFile(filepath.Join("testdata", manifest))
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(b, &cases); err != nil {
			t.Fatal(err)
		}
		for _, tc := range cases {
			t.Run(tc.Name, func(t *testing.T) {
				open := func(name string) (Source, error) {
					file := filepath.Join("testdata", name)
					if manifest == "format-cases.json" {
						file = filepath.Join("testdata/formats", name+".gz")
					}
					b, err := os.ReadFile(file)
					if err == nil && manifest == "format-cases.json" {
						var z *gzip.Reader
						z, err = gzip.NewReader(bytes.NewReader(b))
						if err == nil {
							b, err = io.ReadAll(io.LimitReader(z, (128<<20)+1))
							z.Close()
							if len(b) > 128<<20 {
								err = ErrLimit
							}
						}
					}
					return Source{bytes.NewReader(b), int64(len(b))}, err
				}
				source, err := open(tc.Name)
				if err != nil {
					t.Fatal(err)
				}
				volume := func(name string) (Source, error) { return open(filepath.Join(filepath.Dir(tc.Name), name)) }
				r, err := e.Open(ctx, filepath.Base(tc.Name), source, volume, []byte(tc.Password))
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				if (r.Warning != "") != tc.Warning {
					t.Fatal(r.Warning)
				}
				if tc.Format != "" && r.Format != tc.Format {
					t.Fatalf("format %q, want %q", r.Format, tc.Format)
				}
				h := sha256.New()
				out := &countedWriter{Writer: h}
				outputs := map[uint32]io.Writer{}
				for _, item := range r.Items {
					if !item.Directory && !item.Link {
						outputs[item.Index] = out
					}
				}
				if err = r.Extract(ctx, outputs); err != nil {
					t.Fatal(err)
				}
				if out.size != tc.Bytes || fmt.Sprintf("%x", h.Sum(nil)) != tc.SHA256 {
					t.Fatal("native and embedded output differ")
				}
			})
		}
	}
}

type countedWriter struct {
	io.Writer
	size int64
}

func (w *countedWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.size += int64(n)
	return n, err
}

func TestSplitNamingAndEncryption(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, filename := range []string{"plain.7z", "encrypted.7z", "encrypted.zip"} {
		b, err := os.ReadFile("testdata/" + filename)
		if err != nil {
			t.Fatal(err)
		}
		for _, suffixes := range [][]string{{"001", "002"}, {"01", "02"}, {"00", "01"}, {"aa", "ab"}, {"aaa", "aab"}} {
			t.Run(filename+"."+suffixes[0], func(t *testing.T) {
				parts := map[string][]byte{filename + "." + suffixes[0]: b[:len(b)/2], filename + "." + suffixes[1]: b[len(b)/2:]}
				open := func(name string) (Source, error) {
					b, ok := parts[name]
					if !ok {
						return Source{}, os.ErrNotExist
					}
					return Source{bytes.NewReader(b), int64(len(b))}, nil
				}
				name := filename + "." + suffixes[0]
				if !IsArchiveName(name) {
					t.Fatal("first volume not recognized")
				}
				src, _ := open(name)
				r, err := e.Open(ctx, name, src, open, []byte("test-password"))
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				var out bytes.Buffer
				if err = r.Extract(ctx, map[uint32]io.Writer{0: &out}); err != nil {
					t.Fatal(err)
				}
				if out.String() != "hello archive\n" {
					t.Fatal("split output differs")
				}
			})
		}
	}
}

func TestEncryptedReaders(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, name := range []string{"plain.7z", "encrypted.7z", "encrypted.zip"} {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			v, err := e.Open(ctx, name, Source{bytes.NewReader(b), int64(len(b))}, nil, []byte("test-password"))
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			if len(v.Items) != 1 || v.Items[0].Path != "hello.txt" {
				t.Fatal(v.Items)
			}
			var result bytes.Buffer
			if err = v.Extract(ctx, map[uint32]io.Writer{0: &result}); err != nil {
				t.Fatal(err)
			}
			if result.String() != "hello archive\n" {
				t.Fatal("decoded content differs")
			}
		})
	}
	zipBytes, err := os.ReadFile("testdata/encrypted.zip")
	if err != nil {
		t.Fatal(err)
	}
	v, err := e.Open(ctx, "mystery.bin", Source{bytes.NewReader(zipBytes), int64(len(zipBytes))}, nil, []byte("test-password"))
	if err != nil {
		t.Fatal("signature detection failed:", err)
	}
	if len(v.Items) != 1 || v.Items[0].Path != "hello.txt" {
		t.Fatal(v.Items)
	}
	v.Close()
	b, _ := os.ReadFile("testdata/encrypted.7z")
	for _, password := range [][]byte{nil, []byte("wrong")} {
		v, err := e.Open(ctx, "encrypted.7z", Source{bytes.NewReader(b), int64(len(b))}, nil, password)
		if v != nil {
			v.Close()
		}
		if !errors.Is(err, ErrPassword) {
			t.Fatalf("expected locked archive, got %v", err)
		}
	}
}

func TestLegacyFilenameBytes(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	names := []string{"docs/\xd6\xd0\xce\xc4.txt", "docs/中文.txt", "docs/%D6%D0%CE%C4.txt", "raw-\xff.txt"}
	var input bytes.Buffer
	z := zip.NewWriter(&input)
	for _, name := range names {
		w, err := z.CreateHeader(&zip.FileHeader{Name: name, NonUTF8: true, Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = io.WriteString(w, name); err != nil {
			t.Fatal(err)
		}
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	for _, archiveName := range []string{"legacy.zip", "\xb5\xb5\xb0\xb8.zip"} {
		t.Run(fmt.Sprintf("%x", archiveName), func(t *testing.T) {
			r, err := e.Open(ctx, archiveName, Source{bytes.NewReader(input.Bytes()), int64(input.Len())}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if len(r.Items) != len(names) {
				t.Fatalf("got %d entries, want %d", len(r.Items), len(names))
			}
			outputs := map[uint32]io.Writer{}
			buffers := make([]bytes.Buffer, len(names))
			for i, item := range r.Items {
				if item.Path != names[i] {
					t.Fatalf("filename bytes changed: got %x, want %x", item.Path, names[i])
				}
				outputs[item.Index] = &buffers[i]
			}
			if err = r.Extract(ctx, outputs); err != nil {
				t.Fatal(err)
			}
			for i := range names {
				if buffers[i].String() != names[i] {
					t.Fatalf("entry %d content differs", i)
				}
			}
		})
	}
}

func TestArchiveTailWarning(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	plain, err := os.ReadFile("testdata/plain.7z")
	if err != nil {
		t.Fatal(err)
	}
	data := append(plain, []byte("unindexed trailing data")...)
	r, err := e.Open(ctx, "tail.7z", Source{bytes.NewReader(data), int64(len(data))}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Warning == "" {
		t.Error("unindexed trailing data was reported as a complete archive")
	}
	var out bytes.Buffer
	err = r.Extract(ctx, map[uint32]io.Writer{0: &out})
	r.Close()
	if err != nil || out.String() != "hello archive\n" {
		t.Fatalf("readable member was lost: %v", err)
	}
}

func TestLargeFileOffset(t *testing.T) {
	// A sparse stored member puts the next member and directory past 4 GiB.
	// No multi-gigabyte allocation or sequential source read is involved.
	f, err := os.Create(filepath.Join(t.TempDir(), "large.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const offset = 5 << 30
	sparse := &sparseOutput{file: f}
	z := zip.NewWriter(sparse)
	zeroes := make([]byte, 1<<20)
	// Let the writer compute ZIP64 sizes and the data descriptor while the
	// sparse sink skips the padding; raw headers can omit required local fields.
	padding, err := z.CreateHeader(&zip.FileHeader{Name: "padding", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if err = z.Flush(); err != nil {
		t.Fatal(err)
	}
	sparse.hole = true
	for i := int64(0); i < offset; i += int64(len(zeroes)) {
		if _, err = padding.Write(zeroes); err != nil {
			t.Fatal(err)
		}
	}
	if err = z.Flush(); err != nil {
		t.Fatal(err)
	}
	sparse.hole = false
	w, err := z.Create("large-offset.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Write([]byte("beyond four gigabytes"))
	if err != nil {
		t.Fatal(err)
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	info, _ := f.Stat()
	e, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	r, err := e.Open(context.Background(), "large.zip", Source{f, info.Size()}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if len(r.Items) != 2 || r.Items[1].Path != "large-offset.txt" {
		t.Fatalf("invalid ZIP64 fixture: items=%+v, warning=%q", r.Items, r.Warning)
	}
	var out bytes.Buffer
	if err = r.Extract(context.Background(), map[uint32]io.Writer{1: &out}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "beyond four gigabytes" {
		t.Fatal("large-offset extraction differs")
	}
}

type sparseOutput struct {
	file *os.File
	hole bool
}

func (s *sparseOutput) Write(p []byte) (int, error) {
	if s.hole {
		_, err := s.file.Seek(int64(len(p)), io.SeekCurrent)
		return len(p), err
	}
	return s.file.Write(p)
}

func TestRARPasswordAndVolumes(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	open := func(name string) (Source, error) {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		return Source{bytes.NewReader(b), int64(len(b))}, err
	}
	for _, name := range []string{"rar5-encrypted.rar", "rar5-data-encrypted.rar", "test.part01.rar"} {
		t.Run(name, func(t *testing.T) {
			s, err := open(name)
			if err != nil {
				t.Fatal(err)
			}
			r, err := e.Open(ctx, name, s, open, []byte("password"))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			outputs := map[uint32]io.Writer{}
			buffers := map[uint32]*bytes.Buffer{}
			for _, item := range r.Items {
				if !item.Directory {
					b := new(bytes.Buffer)
					outputs[item.Index] = b
					buffers[item.Index] = b
				}
			}
			if err = r.Extract(ctx, outputs); err != nil {
				t.Fatal(err)
			}
			if name == "test.part01.rar" {
				var want bytes.Buffer
				for i := 0; i <= 2000; i++ {
					fmt.Fprintln(&want, i)
				}
				if len(buffers) != 1 || !bytes.Equal(buffers[0].Bytes(), want.Bytes()) {
					t.Fatal("multi-volume contents differ")
				}
			} else {
				if len(buffers) != 2 {
					t.Fatal("missing RAR entries")
				}
				for _, b := range buffers {
					if b.Len() != 2048 || crc32.ChecksumIEEE(b.Bytes()) != 0xc5b7e6a2 {
						t.Fatal("RAR contents differ")
					}
				}
			}
		})
	}
}
