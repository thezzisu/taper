package engine

import (
	"encoding/hex"
	"errors"
	"github.com/thezzisu/taper/internal/format"
	"math"
)

func ValidateManifest(m Manifest) error {
	if m.ID == (format.ID{}) || m.Files <= 0 || len(m.Contents) == 0 || m.Logical <= 0 {
		return errors.New("empty or invalid manifest")
	}
	paths := map[string]bool{}
	offset := int64(0)
	files := 0
	for _, c := range m.Contents {
		h, e := hex.DecodeString(c.Hash)
		if e != nil || len(h) != 32 {
			return errors.New("invalid SHA-256")
		}
		if c.Size < 0 || len(c.Files) == 0 {
			return errors.New("invalid content")
		}
		ps := make([]string, 0, len(c.Files))
		for _, f := range c.Files {
			if e = format.ValidPath(f.Path); e != nil {
				return e
			}
			if paths[f.Path] {
				return errors.New("duplicate archive path")
			}
			paths[f.Path] = true
			ps = append(ps, f.Path)
			files++
		}
		meta, e := format.EncodeGroup(format.Group{Size: c.Size, Paths: ps})
		if e != nil {
			return e
		}
		if c.Meta != offset || c.Data != offset+int64(len(meta)) || c.Size > math.MaxInt64-c.Data {
			return errors.New("invalid content layout")
		}
		offset = c.Data + c.Size
	}
	if files != m.Files || offset != m.Logical || Digest(m.Contents) != m.Digest {
		return errors.New("manifest digest or extent mismatch")
	}
	return nil
}
