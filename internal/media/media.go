package media

import (
	"errors"
	"io"
)

var ErrFilemark = errors.New("filemark")
var ErrEnd = io.EOF

type Info struct {
	Serial      string `json:"serial"`
	Total       int64  `json:"total"`
	Remaining   int64  `json:"remaining"`
	MaxRecord   int    `json:"max_record"`
	Compression bool   `json:"compression"`
	Encryption  bool   `json:"encryption"`
	Barcode     string `json:"barcode,omitempty"`
	Alerts      []int  `json:"tape_alerts,omitempty"`
}

// Positions count logical objects including filemarks, never compressed bytes.
type Device interface {
	Info() (Info, error)
	Position() (uint64, error)
	Locate(uint64) error
	End() error
	ReadRecord() ([]byte, error)
	WriteRecord([]byte) error
	Filemark() error
	SetKey([]byte) error
	Close() error
}

// Identity is a redundant public bootstrap in cartridge memory, not a directory index.
type Identity interface {
	ReadIdentity() ([]byte, error)
	WriteIdentity([]byte) error
}
