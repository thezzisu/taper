package media

import (
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/thezzisu/taper/internal/format"
)

type request struct {
	Op   string
	Pos  uint64
	Data []byte
}
type response struct {
	Info  Info
	Pos   uint64
	Data  []byte
	Error string
	Code  int
}

// Serve confines a privileged process to a typed tape protocol, not arbitrary CDBs.
func Serve(name string, write bool, in io.Reader, out io.Writer) error {
	d, e := OpenSG(name, write)
	if e != nil {
		return e
	}
	defer d.Close()
	dec := gob.NewDecoder(in)
	enc := gob.NewEncoder(out)
	readBuffer := make([]byte, format.BlockSize)
	for {
		var q request
		if e = dec.Decode(&q); e == io.EOF {
			return nil
		} else if e != nil {
			return e
		}
		r := response{}
		var err error
		switch q.Op {
		case "info":
			r.Info, err = d.Info()
		case "position":
			r.Pos, err = d.Position()
		case "locate":
			err = d.Locate(q.Pos)
		case "end":
			err = d.End()
		case "read":
			var n int
			n, err = d.ReadRecordInto(readBuffer)
			r.Data = readBuffer[:n]
		case "write":
			err = d.WriteRecord(q.Data)
		case "filemark":
			err = d.Filemark()
		case "flush":
			err = d.Flush()
		case "key":
			err = d.SetKey(q.Data)
			clear(q.Data)
		case "close":
			err = d.Close()
			if err != nil {
				r.Error = err.Error()
			}
			enc.Encode(r)
			return nil
		case "identity-read":
			r.Data, err = d.ReadIdentity()
		case "identity-write":
			err = d.WriteIdentity(q.Data)
		default:
			err = errors.New("unknown tape operation")
		}
		if err == ErrEnd {
			r.Code = 1
		} else if err == ErrFilemark {
			r.Code = 2
		} else if err != nil {
			r.Error = err.Error()
		}
		if e = enc.Encode(r); e != nil {
			return e
		}
	}
}

type Worker struct {
	cmd   *exec.Cmd
	input io.WriteCloser
	enc   *gob.Encoder
	dec   *gob.Decoder
	mu    sync.Mutex
}

func OpenWorker(name string, write bool) (*Worker, error) {
	binary, e := os.Executable()
	if e != nil {
		return nil, e
	}
	args := []string{binary, "device-worker", "--device", name}
	if write {
		args = append(args, "--write")
	}
	cmd := exec.Command("pkexec", args...)
	cmd.Stderr = os.Stderr
	in, e := cmd.StdinPipe()
	if e != nil {
		return nil, e
	}
	out, e := cmd.StdoutPipe()
	if e != nil {
		return nil, e
	}
	if e = cmd.Start(); e != nil {
		return nil, e
	}
	w := &Worker{cmd: cmd, input: in, enc: gob.NewEncoder(in), dec: gob.NewDecoder(out)}
	if _, e = w.Info(); e != nil {
		in.Close()
		cmd.Wait()
		return nil, e
	}
	return w, nil
}
func (w *Worker) call(q request) (r response, e error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e = w.enc.Encode(q); e != nil {
		return
	}
	if e = w.dec.Decode(&r); e != nil {
		return
	}
	switch r.Code {
	case 1:
		e = ErrEnd
	case 2:
		e = ErrFilemark
	}
	if r.Error != "" {
		e = fmt.Errorf("tape %s: %s", q.Op, r.Error)
	}
	return
}
func (w *Worker) Info() (Info, error)         { r, e := w.call(request{Op: "info"}); return r.Info, e }
func (w *Worker) Position() (uint64, error)   { r, e := w.call(request{Op: "position"}); return r.Pos, e }
func (w *Worker) Locate(p uint64) error       { _, e := w.call(request{Op: "locate", Pos: p}); return e }
func (w *Worker) End() error                  { _, e := w.call(request{Op: "end"}); return e }
func (w *Worker) ReadRecord() ([]byte, error) { r, e := w.call(request{Op: "read"}); return r.Data, e }
func (w *Worker) ReadRecordInto(b []byte) (int, error) {
	if len(b) != format.BlockSize {
		return 0, errors.New("record buffer must be 1 MiB")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if e := w.enc.Encode(request{Op: "read"}); e != nil {
		return 0, e
	}
	r := response{Data: b[:0]}
	if e := w.dec.Decode(&r); e != nil {
		return 0, e
	}
	if r.Error != "" {
		return 0, errors.New(r.Error)
	}
	if r.Code == 1 {
		return 0, ErrEnd
	}
	if r.Code == 2 {
		return 0, ErrFilemark
	}
	if len(r.Data) > len(b) {
		return 0, errors.New("oversize tape record")
	}
	copy(b, r.Data)
	return len(r.Data), nil
}
func (w *Worker) WriteRecord(b []byte) error { _, e := w.call(request{Op: "write", Data: b}); return e }
func (w *Worker) Filemark() error            { _, e := w.call(request{Op: "filemark"}); return e }
func (w *Worker) Flush() error               { _, e := w.call(request{Op: "flush"}); return e }
func (w *Worker) SetKey(b []byte) error      { _, e := w.call(request{Op: "key", Data: b}); return e }
func (w *Worker) Close() error {
	_, e := w.call(request{Op: "close"})
	w.input.Close()
	ce := w.cmd.Wait()
	if e != nil {
		return e
	}
	return ce
}
