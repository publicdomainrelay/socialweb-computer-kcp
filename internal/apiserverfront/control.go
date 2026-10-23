package apiserverfront

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const MaxSocketPath = 107

const (
	OpStatus = "status"
	OpSet    = "set"
	OpDrain  = "drain"
	OpHold   = "hold"
)

const controlTimeout = 10 * time.Second

type Request struct {
	Op      string `json:"op"`
	Backend string `json:"backend,omitempty"`
	Name    string `json:"name,omitempty"`
}

type Response struct {
	Err     string  `json:"err,omitempty"`
	Status  *Status `json:"status,omitempty"`
	Drained int     `json:"drained,omitempty"`
	Port    int     `json:"port,omitempty"`
}

func CheckSocketPath(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("apiserverfront: control socket %q must be absolute", path)
	}
	if len(path) > MaxSocketPath {
		return fmt.Errorf("apiserverfront: control socket %s is %d bytes, over the %d-byte sun_path limit", path, len(path), MaxSocketPath)
	}
	return nil
}

func ServeControl(ctx context.Context, path string, f *Front, ready func()) error {
	if err := CheckSocketPath(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("apiserverfront: control socket %s: %w", path, err)
	}
	os.Chmod(path, 0o600)
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	if ready != nil {
		ready()
	}
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Go(func() { serveOne(c, f) })
	}
}

func serveOne(c net.Conn, f *Front) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(controlTimeout))
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return
	}
	var req Request
	var resp Response
	if err := json.Unmarshal(line, &req); err != nil {
		resp.Err = err.Error()
		json.NewEncoder(c).Encode(resp)
		return
	}
	switch req.Op {
	case OpStatus:
		st := f.Status()
		resp.Status = &st
	case OpSet:
		st, err := f.SetBackend(req.Backend)
		resp.Status = &st
		if err != nil {
			resp.Err = err.Error()
		}
	case OpDrain:
		n, err := f.Drain(req.Backend)
		resp.Drained = n
		if err != nil {
			resp.Err = err.Error()
		}
		st := f.Status()
		resp.Status = &st
	case OpHold:
		port, err := f.Hold(req.Name)
		resp.Port = port
		if err != nil {
			resp.Err = err.Error()
		}
		st := f.Status()
		resp.Status = &st
	default:
		resp.Err = fmt.Sprintf("apiserverfront: unknown op %q", req.Op)
	}
	json.NewEncoder(c).Encode(resp)
}

type Client struct {
	Path string
}

func (c Client) call(ctx context.Context, req Request) (Response, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, controlTimeout)
		defer cancel()
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Path)
	if err != nil {
		return Response{}, fmt.Errorf("apiserverfront: dialing the front's control socket %s: %w", c.Path, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return Response{}, fmt.Errorf("apiserverfront: reading the %s answer: %w", req.Op, err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, err
	}
	if resp.Err != "" {
		return resp, errors.New(resp.Err)
	}
	return resp, nil
}

func statusOf(resp Response) Status {
	if resp.Status == nil {
		return Status{}
	}
	return *resp.Status
}

func (c Client) Status(ctx context.Context) (Status, error) {
	resp, err := c.call(ctx, Request{Op: OpStatus})
	return statusOf(resp), err
}

func (c Client) Set(ctx context.Context, backend string) (Status, error) {
	resp, err := c.call(ctx, Request{Op: OpSet, Backend: backend})
	return statusOf(resp), err
}

func (c Client) Drain(ctx context.Context, backend string) (int, error) {
	resp, err := c.call(ctx, Request{Op: OpDrain, Backend: backend})
	return resp.Drained, err
}

func (c Client) Hold(ctx context.Context, name string) (int, error) {
	resp, err := c.call(ctx, Request{Op: OpHold, Name: name})
	return resp.Port, err
}
