//go:build linux

package vmruntime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	aclXattr    = "system.posix_acl_access"
	aclVersion  = 2
	aclUserObj  = 0x01
	aclUser     = 0x02
	aclGroupObj = 0x04
	aclGroup    = 0x08
	aclMask     = 0x10
	aclOther    = 0x20
	aclNoID     = 0xffffffff
)

type aclEntry struct {
	tag  uint16
	perm uint16
	id   uint32
}

type Grant struct {
	Path string `json:"path"`
	UID  int    `json:"uid"`
	Perm uint16 `json:"perm"`
}

func readACL(path string, mode os.FileMode) ([]aclEntry, error) {
	buf := make([]byte, 4096)
	n, err := unix.Getxattr(path, aclXattr, buf)
	if errors.Is(err, unix.ENODATA) {
		m := uint16(mode.Perm())
		return []aclEntry{
			{aclUserObj, (m >> 6) & 7, aclNoID},
			{aclGroupObj, (m >> 3) & 7, aclNoID},
			{aclOther, m & 7, aclNoID},
		}, nil
	}
	if err != nil {
		return nil, err
	}
	if n < 4 || binary.LittleEndian.Uint32(buf) != aclVersion || (n-4)%8 != 0 {
		return nil, fmt.Errorf("vmruntime: %s carries an ACL this runtime cannot read", path)
	}
	var out []aclEntry
	for off := 4; off < n; off += 8 {
		out = append(out, aclEntry{
			tag:  binary.LittleEndian.Uint16(buf[off:]),
			perm: binary.LittleEndian.Uint16(buf[off+2:]),
			id:   binary.LittleEndian.Uint32(buf[off+4:]),
		})
	}
	return out, nil
}

func writeACL(path string, entries []aclEntry) error {
	var mask uint16
	named := false
	var rest []aclEntry
	for _, e := range entries {
		switch e.tag {
		case aclMask:
			continue
		case aclUser, aclGroup:
			named = true
			mask |= e.perm
		case aclGroupObj:
			mask |= e.perm
		}
		rest = append(rest, e)
	}
	if named {
		rest = append(rest, aclEntry{aclMask, mask & 7, aclNoID})
	}
	sort.SliceStable(rest, func(i, j int) bool {
		if rest[i].tag != rest[j].tag {
			return rest[i].tag < rest[j].tag
		}
		return rest[i].id < rest[j].id
	})
	buf := make([]byte, 4+8*len(rest))
	binary.LittleEndian.PutUint32(buf, aclVersion)
	for i, e := range rest {
		off := 4 + 8*i
		binary.LittleEndian.PutUint16(buf[off:], e.tag)
		binary.LittleEndian.PutUint16(buf[off+2:], e.perm)
		binary.LittleEndian.PutUint32(buf[off+4:], e.id)
	}
	return unix.Setxattr(path, aclXattr, buf, 0)
}

func grantUser(path string, uid int, perm uint16) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	entries, err := readACL(path, st.Mode())
	if err != nil {
		return err
	}
	found := false
	for i, e := range entries {
		if e.tag == aclUser && e.id == uint32(uid) {
			entries[i].perm = perm
			found = true
		}
	}
	if !found {
		entries = append(entries, aclEntry{aclUser, perm, uint32(uid)})
	}
	if err := writeACL(path, entries); err != nil {
		return fmt.Errorf("vmruntime: granting uid %d %o on %s: %w", uid, perm, path, err)
	}
	return nil
}

func revokeUser(path string, uid int) error {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	entries, err := readACL(path, st.Mode())
	if err != nil {
		return err
	}
	var keep []aclEntry
	for _, e := range entries {
		if e.tag == aclUser && e.id == uint32(uid) {
			continue
		}
		keep = append(keep, e)
	}
	if len(keep) == len(entries) {
		return nil
	}
	return writeACL(path, keep)
}

func ownedBy(st os.FileInfo, uid int) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(s.Uid) == uid
}

type granter struct {
	uid    int
	self   int
	grants []Grant
	seen   map[string]uint16
}

func newGranter(uid int) *granter {
	return &granter{uid: uid, self: os.Getuid(), seen: map[string]uint16{}}
}

func (g *granter) need(path string, perm uint16, record bool) error {
	if have, ok := g.seen[path]; ok && have&perm == perm {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	other := uint16(st.Mode().Perm()) & 7
	if other&perm == perm {
		g.seen[path] = perm
		return nil
	}
	if !ownedBy(st, g.self) {
		return fmt.Errorf("vmruntime: the VMM's uid %d needs %o on %s, which uid %d does not own and which does not grant it to others; move the state, runtime or asset directory under this account", g.uid, perm, path, g.self)
	}
	if err := grantUser(path, g.uid, perm|g.seen[path]); err != nil {
		return err
	}
	g.seen[path] = perm | g.seen[path]
	if record {
		g.grants = append(g.grants, Grant{Path: path, UID: g.uid, Perm: g.seen[path]})
	}
	return nil
}

func (g *granter) traverse(path string, stopAt string) error {
	dir := filepath.Dir(path)
	for {
		if dir == stopAt {
			return nil
		}
		if err := g.need(dir, 1, true); err != nil {
			return err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

func revokeAll(grants []Grant) error {
	var errs []error
	for _, g := range grants {
		if err := revokeUser(g.Path, g.UID); err != nil {
			errs = append(errs, fmt.Errorf("revoking uid %d on %s: %w", g.UID, g.Path, err))
		}
	}
	return errors.Join(errs...)
}

func (g *granter) tree(root string) error {
	if err := g.traverse(root, ""); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return g.need(path, 1, true)
		case d.Type().IsRegular():
			return g.need(path, 4, true)
		}
		return nil
	})
}
