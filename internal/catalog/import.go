package catalog

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
)

func Import(src, dst string, pass []byte) error {
	db, e := os.ReadFile(filepath.Join(src, "catalog.db"))
	if e != nil {
		return e
	}
	descriptor, e := os.ReadFile(filepath.Join(src, "catalog.db.unlock"))
	if e != nil {
		return e
	}
	mac, e := os.ReadFile(filepath.Join(src, "catalog.db.mac"))
	if e != nil {
		return e
	}
	key, e := unwrap(pass, descriptor)
	if e != nil {
		return e
	}
	defer clear(key)
	h := hmac.New(sha256.New, key)
	h.Write([]byte("taper-export-v1"))
	h.Write(descriptor)
	h.Write(db)
	if !hmac.Equal(h.Sum(nil), mac) {
		return errors.New("export authentication failed")
	}
	snapshot, e := openDatabase(filepath.Join(src, "catalog.db"), key, "ro")
	if e != nil {
		return e
	}
	if e = errors.Join(checkVersion(snapshot), snapshot.Close()); e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(dst), 0700); e != nil {
		return e
	}
	created := []string{dst, dst + ".unlock", dst + ".mac"}
	installed := false
	defer func() {
		if !installed {
			for _, name := range created {
				_ = os.Remove(name)
			}
		}
	}()
	file, e := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = file.Write(db)
	if e == nil {
		e = file.Sync()
	}
	file.Close()
	if e != nil {
		return e
	}
	file, e = os.OpenFile(dst+".unlock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = file.Write(descriptor)
	if e == nil {
		e = file.Sync()
	}
	file.Close()
	if e != nil {
		return e
	}
	file, e = os.OpenFile(dst+".mac", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = file.Write(mac)
	if e == nil {
		e = file.Sync()
	}
	file.Close()
	if e != nil {
		return e
	}
	dir, e := os.Open(filepath.Dir(dst))
	if e != nil {
		return e
	}
	defer dir.Close()
	if e = dir.Sync(); e != nil {
		return e
	}
	installed = true
	return nil
}
