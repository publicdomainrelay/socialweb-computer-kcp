//go:build linux

package devsup

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type storedFD struct {
	name string
	f    *os.File
}

type fdStore struct {
	path string
	conn *net.UnixConn
	fds  []storedFD
}

func (st *fdStore) socket() string { return st.path }

func (s *server) notifyPath(name string) string {
	sum := sha256.Sum256([]byte(s.id + "/" + name))
	return filepath.Join(filepath.Dir(s.o.Socket), "n-"+hex.EncodeToString(sum[:])[:12]+".sock")
}

func (s *server) openFDStore(u *unit) error {
	if u.spec.FDStore <= 0 || u.store != nil {
		return nil
	}
	path := s.notifyPath(u.spec.Name)
	if len(path) > 107 {
		return fmt.Errorf("devsup: the notify socket %s of %s is over the 107-byte sun_path limit", path, u.spec.Name)
	}
	os.Remove(path)
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("devsup: the notify socket of %s: %w", u.spec.Name, err)
	}
	os.Chmod(path, 0o600)
	raw, err := conn.SyscallConn()
	if err == nil {
		var serr error
		err = raw.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PASSCRED, 1) })
		if err == nil {
			err = serr
		}
	}
	if err != nil {
		conn.Close()
		os.Remove(path)
		return fmt.Errorf("devsup: SO_PASSCRED on the notify socket of %s: %w", u.spec.Name, err)
	}
	st := &fdStore{path: path, conn: conn}
	u.store = st
	s.wg.Go(func() { s.readNotify(u, st) })
	return nil
}

func (s *server) readNotify(u *unit, st *fdStore) {
	buf := make([]byte, 4096)
	oob := make([]byte, unix.CmsgSpace(253*4)+unix.CmsgSpace(unix.SizeofUcred))
	for {
		n, oobn, _, _, err := st.conn.ReadMsgUnix(buf, oob)
		if err != nil {
			return
		}
		var fds []int
		pid := 0
		if msgs, err := unix.ParseSocketControlMessage(oob[:oobn]); err == nil {
			for i := range msgs {
				switch {
				case msgs[i].Header.Level == unix.SOL_SOCKET && msgs[i].Header.Type == unix.SCM_RIGHTS:
					if got, err := unix.ParseUnixRights(&msgs[i]); err == nil {
						fds = append(fds, got...)
					}
				case msgs[i].Header.Level == unix.SOL_SOCKET && msgs[i].Header.Type == unix.SCM_CREDENTIALS:
					if cred, err := unix.ParseUnixCredentials(&msgs[i]); err == nil {
						pid = int(cred.Pid)
					}
				}
			}
		}
		s.notified(u, st, pid, buf[:n], fds)
	}
}

func notifyFields(msg []byte) map[string]string {
	out := map[string]string{}
	for line := range bytes.SplitSeq(msg, []byte{'\n'}) {
		k, v, ok := strings.Cut(string(line), "=")
		if ok {
			out[k] = v
		}
	}
	return out
}

func (s *server) notified(u *unit, st *fdStore, pid int, msg []byte, fds []int) {
	closeAll := func() {
		for _, fd := range fds {
			unix.Close(fd)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if u.store != st || pid <= 0 || !(pid == u.st.MainPID || s.belongs(u.spec.Name, u.st.InvocationID, u.pgid, pid)) {
		s.log.Warn("a notify message from outside the unit was dropped", "unit", u.spec.Name, "pid", pid)
		closeAll()
		return
	}
	f := notifyFields(msg)
	name := f["FDNAME"]
	name = cmp.Or(name, "stored")
	if f["FDSTOREREMOVE"] == "1" {
		keep := st.fds[:0]
		for _, sfd := range st.fds {
			if sfd.name == name {
				sfd.f.Close()
				continue
			}
			keep = append(keep, sfd)
		}
		st.fds = keep
		closeAll()
		return
	}
	if f["FDSTORE"] != "1" {
		closeAll()
		return
	}
	for _, fd := range fds {
		if len(st.fds) >= u.spec.FDStore {
			s.log.Warn("the fd store is full; an fd was dropped", "unit", u.spec.Name, "max", u.spec.FDStore)
			unix.Close(fd)
			continue
		}
		st.fds = append(st.fds, storedFD{name: name, f: os.NewFile(uintptr(fd), name)})
		s.log.Info("fd stored", "unit", u.spec.Name, "name", name, "pid", pid, "held", len(st.fds))
	}
}

func (st *fdStore) launchFiles() ([]*os.File, []string) {
	if st == nil || len(st.fds) == 0 {
		return nil, nil
	}
	var files []*os.File
	var names []string
	for _, sfd := range st.fds {
		files = append(files, sfd.f)
		names = append(names, sfd.name)
	}
	return files, []string{EnvListenFDs + "=" + strconv.Itoa(len(files)), EnvListenNames + "=" + strings.Join(names, ":")}
}

func (st *fdStore) release() {
	if st == nil {
		return
	}
	st.conn.Close()
	os.Remove(st.path)
	for _, sfd := range st.fds {
		sfd.f.Close()
	}
	st.fds = nil
}
