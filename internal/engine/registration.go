package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/thezzisu/taper/internal/catalog"
	"github.com/thezzisu/taper/internal/format"
	"github.com/thezzisu/taper/internal/media"
)

// RegisterMedia only queries the drive and MAM. It does not locate, read tape
// records, set a drive key, write a label, or erase anything.
func RegisterMedia(s *catalog.Store, d media.Device, name string, key []byte, saveKey, hardwareCompression bool) (l Label, err error) {
	explicitName := name != ""
	info, err := d.Info()
	if err != nil {
		return
	}
	if name == "" {
		name = info.Barcode
	}
	if name == "" {
		name = info.Serial
	}
	if len(name) > 64 {
		return l, errors.New("media label exceeds 64 UTF-8 bytes")
	}
	var state, oldKeyID string
	err = s.DB.QueryRow("SELECT id,label,state,key_id FROM media WHERE serial=?", info.Serial).Scan(&l.ID, &l.Name, &state, &oldKeyID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		l = Label{ID: format.NewID().String(), Name: name, Serial: info.Serial, Version: 1}
		state = "registered"
		if b, e := d.ReadIdentity(); e == nil && len(b) > 7 && string(b[:7]) == "taper1:" {
			var fields []string
			if json.Unmarshal(b[7:], &fields) == nil && len(fields) == 3 {
				if _, e = format.ParseID(fields[0]); e != nil {
					return l, e
				}
				l.ID = fields[0]
				oldKeyID = fields[2]
				state = "open"
			}
		}
	}
	if !explicitName && l.Name != "" {
		name = l.Name
	}
	l.Serial = info.Serial
	l.Name = name
	l.Version = 1
	l.KeyID = oldKeyID
	if len(key) > 0 {
		if len(key) != 32 {
			return l, errors.New("key must be 32 bytes")
		}
		id := media.KeyID(key)
		if state != "registered" && oldKeyID != id {
			return l, errors.New("key does not match the tape identity")
		}
		l.KeyID = id
	}
	var stored []byte
	if saveKey {
		stored = key
	}
	_, err = s.DB.Exec(`INSERT INTO media(id,label,serial,total,remaining,sampled,state,key_id,key,hardware_compression) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET
label=excluded.label,
hardware_compression=excluded.hardware_compression,
key_id=excluded.key_id,key=CASE WHEN ? THEN NULL WHEN excluded.key IS NOT NULL THEN excluded.key ELSE media.key END`, l.ID, l.Name, l.Serial, info.Total, info.Remaining, info.Sampled, state, l.KeyID, stored, hardwareCompression, !saveKey)
	if err == nil {
		err = recordMediaInfo(s, l.ID, info)
	}
	return
}

// Called only by the writer after source prefill succeeds. New media get their
// public label at this point; occupied unformatted media are always refused.
func prepareMedia(s *catalog.Store, d media.Device, provided []byte) (l Label, err error) {
	info, err := d.Info()
	if err != nil {
		return
	}
	var state string
	var stored []byte
	var compression *bool
	err = s.DB.QueryRow("SELECT id,label,state,key_id,key,hardware_compression FROM media WHERE serial=?", info.Serial).Scan(&l.ID, &l.Name, &state, &l.KeyID, &stored, &compression)
	if errors.Is(err, sql.ErrNoRows) {
		return l, errors.New("register the loaded tape with tape register first")
	}
	if err != nil {
		return
	}
	defer clear(stored)
	var incomplete int
	if err = s.DB.QueryRow("SELECT COUNT(*) FROM replicas WHERE media_id=? AND state IN ('writing','failed')", l.ID).Scan(&incomplete); err != nil {
		return
	}
	if incomplete != 0 {
		return l, errors.New("tape has an incomplete write; inspect and resolve its tail before appending")
	}
	if compression == nil {
		return l, errors.New("hardware compression preference is unknown; register it before writing")
	}
	if state == "open" {
		l, err = UnlockMedia(s, d, provided)
		if err == nil {
			err = d.SetCompression(*compression)
		}
		return
	}
	if state != "registered" {
		return l, fmt.Errorf("media state is %s", state)
	}
	if err = checkMediaInfo(info); err != nil {
		return
	}
	key := stored
	if len(provided) > 0 {
		key = provided
	}
	if l.KeyID != "" && media.KeyID(key) != l.KeyID {
		return l, errors.New("matching tape key required")
	}
	if l.KeyID == "" && len(key) > 0 {
		return l, errors.New("register the intended encryption key before writing")
	}
	if err = d.Locate(0); err != nil {
		return
	}
	if _, e := d.ReadRecord(); e != io.EOF {
		return l, errors.New("uninitialized tape is not empty; explicitly approve and clear it before writing")
	}
	if err = d.SetCompression(*compression); err != nil {
		return
	}
	if err = d.SetKey(nil); err != nil {
		return
	}
	if pos, e := d.Position(); e != nil {
		return l, e
	} else if pos != 0 {
		if err = d.Locate(0); err != nil {
			return
		}
	}
	l.Serial = info.Serial
	l.Version = 1
	id, _ := format.ParseID(l.ID)
	if err = control(d, format.Media, id, l); err != nil {
		return
	}
	if err = d.Filemark(); err != nil {
		return
	}
	b, _ := json.Marshal([]string{l.ID, l.Name, l.KeyID})
	if err = d.WriteIdentity(append([]byte("taper1:"), b...)); err != nil {
		return
	}
	if err = d.SetKey(key); err != nil {
		return
	}
	_, err = s.DB.Exec("UPDATE media SET state='open' WHERE id=?", l.ID)
	return
}
