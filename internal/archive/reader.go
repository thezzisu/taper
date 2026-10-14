// Package archive reads containers with the embedded official 7-Zip engine.
// The host owns all input/output streams; the guest has no filesystem access.
package archive

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:embed 7zip.wasm
var wasm []byte

const (
	Version     = "7-Zip 26.04"
	MemoryLimit = 1 << 30
	MaxEntries  = 1_000_000
	maxName     = 64 << 10
	maxNames    = 64 << 20
)

var (
	ErrNotArchive  = errors.New("unrecognized container")
	ErrPassword    = errors.New("password required, incorrect password, or damaged encrypted data")
	ErrLimit       = errors.New("container index limit exceeded")
	ErrUnsupported = errors.New("unsupported compression or encryption method")
)

// Source may be a file, a section, or a concatenation of split volumes.
// The caller retains ownership and closes it after Reader.Close.
type Source struct {
	io.ReaderAt
	Size int64
}

type Item struct {
	Index     uint32
	Path      string // Preserves 7-Zip's POSIX filename bytes, including non-UTF-8.
	Size      int64
	Directory bool
	Encrypted bool
	Link      bool
}

type Engine struct {
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
}

var compilationCache = wazero.NewCompilationCache()

type Reader struct {
	Format    string
	Encrypted bool
	Warning   string
	Items     []Item
	module    api.Module
	sources   []Source
	volume    func(string) (Source, error)
	outputs   map[uint32]io.Writer
	completed map[uint32]bool
	err       error
	nameBytes int
}

type readerKey struct{}

func current(ctx context.Context) *Reader { return ctx.Value(readerKey{}).(*Reader) }

func New(ctx context.Context) (*Engine, error) {
	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCompilationCache(compilationCache).
		WithCoreFeatures(api.CoreFeaturesV2|experimental.CoreFeaturesExceptionHandling).
		WithMemoryLimitPages(MemoryLimit/65536).
		WithCloseOnContextDone(true))
	fail := func(err error) (*Engine, error) { r.Close(context.Background()); return nil, err }
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, r); err != nil {
		return fail(err)
	}
	h := r.NewHostModuleBuilder("taper")
	h.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, flags, p, n uint32) {
		v := current(ctx)
		v.Encrypted = flags&1 != 0
		if flags&2 != 0 {
			v.Warning = "7-Zip reported incomplete or damaged archive metadata"
			if b, ok := m.Memory().Read(p, n); ok && n > 0 && n <= maxName {
				v.Warning = string(b)
			}
		}
	}).Export("status")
	h.NewFunctionBuilder().WithFunc(readAt).Export("read")
	h.NewFunctionBuilder().WithFunc(func(ctx context.Context, id uint32) uint64 {
		v := current(ctx)
		if int(id) >= len(v.sources) {
			return math.MaxUint64
		}
		return uint64(v.sources[id].Size)
	}).Export("size")
	h.NewFunctionBuilder().WithFunc(openVolume).Export("volume")
	h.NewFunctionBuilder().WithFunc(addItem).Export("item")
	h.NewFunctionBuilder().WithFunc(func(ctx context.Context, m api.Module, p, n uint32) {
		v := current(ctx)
		b, ok := m.Memory().Read(p, n)
		if !ok || n > maxName {
			v.err = ErrLimit
			return
		}
		v.Format = string(b)
	}).Export("format")
	h.NewFunctionBuilder().WithFunc(writeData).Export("write")
	h.NewFunctionBuilder().WithFunc(func(ctx context.Context, index, result uint32) {
		v := current(ctx)
		if result != 0 {
			if v.err == nil {
				v.err = fmt.Errorf("container entry %d: 7-Zip integrity result %d", index, result)
				if result == 1 {
					v.err = errors.Join(ErrUnsupported, v.err)
				}
			}
			return
		}
		v.completed[index] = true
	}).Export("finish")
	if _, err := h.Instantiate(ctx); err != nil {
		return fail(err)
	}
	c, err := r.CompileModule(ctx, wasm)
	if err != nil {
		return fail(err)
	}
	return &Engine{runtime: r, compiled: c}, nil
}

func (e *Engine) Close() error { return e.runtime.Close(context.Background()) }

func (e *Engine) Open(ctx context.Context, name string, source Source, volumes func(string) (Source, error), password []byte) (*Reader, error) {
	if len(name) > maxName || strings.ContainsRune(name, 0) {
		return nil, errors.New("invalid container name")
	}
	if bytes.IndexByte(password, 0) >= 0 || !utf8.Valid(password) {
		return nil, errors.New("archive password must be UTF-8 without NUL")
	}
	if source.ReaderAt == nil || source.Size < 0 {
		return nil, errors.New("invalid container source")
	}
	v := &Reader{sources: []Source{source}, volume: volumes}
	ctx = context.WithValue(ctx, readerKey{}, v)
	m, err := e.runtime.InstantiateModule(ctx, e.compiled, wazero.NewModuleConfig().
		WithName("").WithStartFunctions("_initialize").WithRandSource(rand.Reader).
		WithSysWalltime().WithSysNanotime())
	if err != nil {
		return nil, err
	}
	v.module = m
	fail := func(err error) (*Reader, error) { v.Close(); return nil, err }
	namePtr, err := v.put(ctx, append([]byte(name), 0))
	if err != nil {
		return fail(err)
	}
	defer v.free(ctx, namePtr)
	passPtr, err := v.put(ctx, append(append([]byte(nil), password...), 0))
	if err != nil {
		return fail(err)
	}
	defer func() {
		if v.module != nil {
			if b, ok := m.Memory().Read(uint32(passPtr), uint32(len(password)+1)); ok {
				clear(b)
			}
		}
		v.free(ctx, passPtr)
	}()
	var provided uint64
	if password != nil {
		provided = 1
	}
	res, err := m.ExportedFunction("taper_open").Call(ctx, namePtr, passPtr, provided)
	if v.err != nil {
		return fail(v.err)
	}
	if err != nil {
		return fail(err)
	}
	if err = resultError(res[0]); err != nil {
		return fail(err)
	}
	return v, nil
}

// Extract processes requested members together in archive order. Solid streams
// are decoded once, rather than restarted for each nested archive.
func (v *Reader) Extract(ctx context.Context, outputs map[uint32]io.Writer) error {
	if len(outputs) == 0 {
		return nil
	}
	if v.module == nil {
		return errors.New("container reader closed")
	}
	ctx = context.WithValue(ctx, readerKey{}, v)
	indices := make([]byte, 0, 4*len(outputs))
	for _, item := range v.Items {
		if w, ok := outputs[item.Index]; ok {
			if w == nil || item.Directory || item.Link {
				return errors.New("invalid extraction target")
			}
			indices = binary.LittleEndian.AppendUint32(indices, item.Index)
		}
	}
	if len(indices)/4 != len(outputs) {
		return errors.New("container entry not found")
	}
	p, err := v.put(ctx, indices)
	if err != nil {
		return err
	}
	defer v.free(ctx, p)
	v.outputs, v.completed, v.err = outputs, make(map[uint32]bool), nil
	defer func() { v.outputs, v.completed = nil, nil }()
	res, err := v.module.ExportedFunction("taper_extract").Call(ctx, p, uint64(len(outputs)))
	if v.err != nil {
		if err == nil && res[0] == 2 && !errors.Is(v.err, ErrLimit) && !errors.Is(v.err, ErrUnsupported) {
			return errors.Join(ErrPassword, v.err)
		}
		return v.err
	}
	if err != nil {
		return err
	}
	if err = resultError(res[0]); err != nil {
		return err
	}
	for i := range outputs {
		if !v.completed[i] {
			return fmt.Errorf("container entry %d was not completed", i)
		}
	}
	return nil
}

func (v *Reader) Close() error {
	if v.module == nil {
		return nil
	}
	err := v.module.Close(context.Background())
	v.module, v.sources, v.outputs = nil, nil, nil
	return err
}

func (v *Reader) put(ctx context.Context, b []byte) (uint64, error) {
	res, err := v.module.ExportedFunction("malloc").Call(ctx, uint64(len(b)))
	if err != nil {
		return 0, err
	}
	if res[0] == 0 || !v.module.Memory().Write(uint32(res[0]), b) {
		return 0, ErrLimit
	}
	return res[0], nil
}
func (v *Reader) free(ctx context.Context, p uint64) {
	if v.module != nil {
		_, _ = v.module.ExportedFunction("free").Call(ctx, p)
	}
}

func resultError(result uint64) error {
	switch result {
	case 0:
		return nil
	case 1:
		return ErrNotArchive
	case 2:
		return ErrPassword
	case 4:
		return ErrLimit
	default:
		return errors.New("7-Zip could not decode this container")
	}
}

func readAt(ctx context.Context, m api.Module, id uint32, off uint64, p, n uint32) uint32 {
	v := current(ctx)
	if int(id) >= len(v.sources) || off > math.MaxInt64 {
		v.err = errors.New("invalid container read")
		return math.MaxUint32
	}
	n = min(n, 1<<20)
	b, ok := m.Memory().Read(p, n)
	if !ok {
		v.err = ErrLimit
		return math.MaxUint32
	}
	if err := ctx.Err(); err != nil {
		v.err = err
		return math.MaxUint32
	}
	count, err := v.sources[id].ReadAt(b, int64(off))
	if err != nil && err != io.EOF {
		v.err = err
		return math.MaxUint32
	}
	return uint32(count)
}

func openVolume(ctx context.Context, m api.Module, p, n uint32) uint32 {
	v := current(ctx)
	if v.volume == nil {
		return math.MaxUint32
	}
	b, ok := m.Memory().Read(p, n)
	if !ok || n > maxName || len(v.sources) >= 1024 {
		v.err = ErrLimit
		return math.MaxUint32
	}
	s, err := v.volume(string(b))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			v.err = err
		}
		return math.MaxUint32
	}
	if s.ReaderAt == nil || s.Size < 0 {
		v.err = errors.New("invalid volume")
		return math.MaxUint32
	}
	v.sources = append(v.sources, s)
	return uint32(len(v.sources) - 1)
}

func addItem(ctx context.Context, m api.Module, index, p, n uint32, size uint64, flags uint32) uint32 {
	v := current(ctx)
	if len(v.Items) >= MaxEntries || n > maxName || v.nameBytes+int(n) > maxNames {
		v.err = ErrLimit
		return 1
	}
	b, ok := m.Memory().Read(p, n)
	if !ok {
		v.err = errors.New("invalid container filename")
		return 1
	}
	v.nameBytes += int(n)
	v.Items = append(v.Items, Item{index, string(b), int64(size), flags&1 != 0, flags&2 != 0, flags&4 != 0})
	return 0
}

func writeData(ctx context.Context, m api.Module, index, p, n uint32) uint32 {
	v := current(ctx)
	w := v.outputs[index]
	if w == nil {
		v.err = errors.New("unexpected extracted entry")
		return math.MaxUint32
	}
	b, ok := m.Memory().Read(p, n)
	if !ok {
		v.err = ErrLimit
		return math.MaxUint32
	}
	if err := ctx.Err(); err != nil {
		v.err = err
		return math.MaxUint32
	}
	nw, err := w.Write(b)
	if err == nil && nw != len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		v.err = err
		return math.MaxUint32
	}
	return uint32(nw)
}
