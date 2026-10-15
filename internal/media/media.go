package media

import (
	"errors"
	"fmt"
	"io"
)

var ErrIdentityAbsent = errors.New("MAM identity absent")

var ErrFilemark = errors.New("filemark")
var ErrEnd = io.EOF

type Info struct {
	Serial       string  `json:"serial"`
	Total        int64   `json:"reported_total_bytes"`
	Remaining    int64   `json:"reported_remaining_bytes"`
	NativeLimit  int64   `json:"native_limit_bytes"`
	Density      byte    `json:"density,omitempty"`
	Position     *uint64 `json:"logical_position"`
	Sampled      string  `json:"sampled"`
	MAM          []byte  `json:"mam,omitempty"`
	PositionData []byte  `json:"position_response,omitempty"`
	EarlyWarning bool    `json:"early_warning,omitempty"`
	MaxRecord    int     `json:"max_record"`
	Compression  bool    `json:"compression"`
	Encryption   bool    `json:"encryption"`
	Barcode      string  `json:"barcode,omitempty"`
	Alerts       []int   `json:"tape_alerts,omitempty"`
}

// Total and Remaining are device facts. Budget applies the documented native
// limit to consumed bytes without altering those measurements.
func (i Info) Budget() (total, remaining int64, err error) {
	if i.Total <= 0 || i.Remaining < 0 || i.Remaining > i.Total || i.NativeLimit <= 0 {
		return 0, 0, fmt.Errorf("capacity budget unavailable: missing native limit or inconsistent device counters")
	}
	total = min(i.Total, i.NativeLimit)
	remaining = max(0, total-(i.Total-i.Remaining))
	if i.EarlyWarning {
		remaining = 0
	}
	return
}

// Positions count logical objects including filemarks, never compressed bytes.
type Device interface {
	Info() (Info, error)
	Position() (uint64, error)
	Locate(uint64) error
	End() error
	ReadRecord() ([]byte, error)
	ReadRecordInto([]byte) (int, error)
	WriteRecord([]byte) error
	Filemark() error
	Flush() error
	SetKey([]byte) error
	SetCompression(bool) error
	ReadIdentity() ([]byte, error)
	WriteIdentity([]byte) error
	Eject() error
	Close() error
}
