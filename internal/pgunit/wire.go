package pgunit

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"time"
)

const (
	protocolVersion = 196608
	DialTimeout     = 10 * time.Second
)

type Error struct {
	Severity string
	Code     string
	Message  string
}

func (e *Error) Error() string {
	return fmt.Sprintf("postgres %s %s: %s", e.Severity, e.Code, e.Message)
}

type conn struct {
	c net.Conn
	r *bufio.Reader
}

func cstring(b []byte) (string, []byte) {
	for i, c := range b {
		if c == 0 {
			return string(b[:i]), b[i+1:]
		}
	}
	return string(b), nil
}

func parseError(p []byte) *Error {
	e := &Error{}
	for len(p) > 0 && p[0] != 0 {
		t := p[0]
		var v string
		v, p = cstring(p[1:])
		switch t {
		case 'S':
			e.Severity = v
		case 'C':
			e.Code = v
		case 'M':
			e.Message = v
		}
	}
	return e
}

func (c *conn) send(t byte, payload []byte) error {
	var head []byte
	if t != 0 {
		head = append(head, t)
	}
	head = binary.BigEndian.AppendUint32(head, uint32(len(payload)+4))
	_, err := c.c.Write(append(head, payload...))
	return err
}

func (c *conn) recv() (byte, []byte, error) {
	var head [5]byte
	if _, err := io.ReadFull(c.r, head[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(head[1:])
	if n < 4 || n > 1<<26 {
		return 0, nil, fmt.Errorf("pgunit: a %d-byte message is not postgres", n)
	}
	p := make([]byte, n-4)
	_, err := io.ReadFull(c.r, p)
	return head[0], p, err
}

func (c *conn) untilReady() ([][]string, error) {
	var rows [][]string
	var failed error
	for {
		t, p, err := c.recv()
		if err != nil {
			return nil, err
		}
		switch t {
		case 'E':
			e := parseError(p)
			if e.Severity == "FATAL" || e.Severity == "PANIC" {
				return nil, e
			}
			failed = e
		case 'R':
			if len(p) < 4 || binary.BigEndian.Uint32(p) != 0 {
				return nil, errors.New("pgunit: postgres asked for a password; the socket is trust-only to its account")
			}
		case 'D':
			if len(p) < 2 {
				return nil, errors.New("pgunit: a short data row")
			}
			cols := int(binary.BigEndian.Uint16(p))
			p = p[2:]
			row := make([]string, 0, cols)
			for range cols {
				if len(p) < 4 {
					return nil, errors.New("pgunit: a short data row")
				}
				l := int32(binary.BigEndian.Uint32(p))
				p = p[4:]
				if l < 0 {
					row = append(row, "")
					continue
				}
				if int(l) > len(p) {
					return nil, errors.New("pgunit: a short data row")
				}
				row = append(row, string(p[:l]))
				p = p[l:]
			}
			rows = append(rows, row)
		case 'Z':
			return rows, failed
		}
	}
}

func dial(ctx context.Context, socketDir, user, db string) (*conn, error) {
	if err := CheckSocketDir(socketDir); err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: DialTimeout}
	nc, err := d.DialContext(ctx, "unix", filepath.Join(socketDir, SocketName))
	if err != nil {
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(DialTimeout)
	}
	nc.SetDeadline(deadline)
	c := &conn{c: nc, r: bufio.NewReader(nc)}
	var p []byte
	p = binary.BigEndian.AppendUint32(p, protocolVersion)
	for _, kv := range [][2]string{{"user", user}, {"database", db}} {
		p = append(append(append(append(p, kv[0]...), 0), kv[1]...), 0)
	}
	p = append(p, 0)
	if err := c.send(0, p); err != nil {
		nc.Close()
		return nil, err
	}
	if _, err := c.untilReady(); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

func (c *conn) close() {
	c.send('X', nil)
	c.c.Close()
}

func Query(ctx context.Context, socketDir, user, db, sql string) ([][]string, error) {
	c, err := dial(ctx, socketDir, user, db)
	if err != nil {
		return nil, err
	}
	defer c.close()
	if err := c.send('Q', append([]byte(sql), 0)); err != nil {
		return nil, err
	}
	return c.untilReady()
}

func Ready(ctx context.Context, socketDir, user string) error {
	rows, err := Query(ctx, socketDir, user, "postgres", "SELECT 1")
	if err != nil {
		return err
	}
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0] != "1" {
		return fmt.Errorf("pgunit: SELECT 1 answered %v", rows)
	}
	return nil
}

func ServerVersion(ctx context.Context, socketDir, user string) (string, error) {
	rows, err := Query(ctx, socketDir, user, "postgres", "SHOW server_version")
	if err != nil {
		return "", err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return "", fmt.Errorf("pgunit: SHOW server_version answered %v", rows)
	}
	return rows[0][0], nil
}

func CreateDatabase(ctx context.Context, socketDir, user, name string) (bool, error) {
	if len(name) > 63 || !nameRE.MatchString(name) {
		return false, fmt.Errorf("pgunit: database %q: want lower-case letters, digits and underscores", name)
	}
	rows, err := Query(ctx, socketDir, user, "postgres", "SELECT 1 FROM pg_database WHERE datname = '"+name+"'")
	if err != nil {
		return false, err
	}
	if len(rows) > 0 {
		return false, nil
	}
	_, err = Query(ctx, socketDir, user, "postgres", "CREATE DATABASE "+name)
	var pe *Error
	if errors.As(err, &pe) && (pe.Code == "42P04" || pe.Code == "23505") {
		return false, nil
	}
	return err == nil, err
}
