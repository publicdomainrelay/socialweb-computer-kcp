package irohdial

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	MaxSocketPath  = 107
	MaxTicketBytes = 4096
	TicketTimeout  = 10 * time.Second
	stderrTail     = 2048
)

var ticketPattern = regexp.MustCompile(`^[a-z0-9]+$`)

type Config struct {
	Socket   string
	Dumbpipe string
	Log      *slog.Logger
}

func (c Config) Check() error {
	switch {
	case !filepath.IsAbs(c.Socket):
		return fmt.Errorf("irohdial: socket %q is not an absolute path", c.Socket)
	case len(c.Socket) > MaxSocketPath:
		return fmt.Errorf("irohdial: socket %s is %d bytes, over the %d a unix socket path takes", c.Socket, len(c.Socket), MaxSocketPath)
	case !filepath.IsAbs(c.Dumbpipe):
		return fmt.Errorf("irohdial: dumbpipe %q is not an absolute path", c.Dumbpipe)
	}
	st, err := os.Stat(c.Dumbpipe)
	if err != nil {
		return fmt.Errorf("irohdial: dumbpipe: %w", err)
	}
	if st.IsDir() || st.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("irohdial: dumbpipe %s is not an executable file", c.Dumbpipe)
	}
	return nil
}

func CheckTicket(ticket string) error {
	if len(ticket) == 0 || len(ticket) > MaxTicketBytes || !ticketPattern.MatchString(ticket) {
		return errors.New("irohdial: not an iroh ticket (lowercase base32, at most 4096 bytes)")
	}
	return nil
}

func Run(ctx context.Context, c Config) error {
	if err := c.Check(); err != nil {
		return err
	}
	log := c.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(filepath.Dir(c.Socket), 0o700); err != nil {
		return err
	}
	if err := os.Remove(c.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", c.Socket)
	if err != nil {
		return err
	}
	if err := os.Chmod(c.Socket, 0o600); err != nil {
		ln.Close()
		return err
	}
	log.Info("the iroh dialer listens; each connection names one guest's iroh ticket", "socket", c.Socket, "dumbpipe", c.Dumbpipe)
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Go(func() {
			serve(ctx, c, log, conn)
		})
	}
}

func serve(ctx context.Context, c Config, log *slog.Logger, conn net.Conn) {
	defer conn.Close()
	started := time.Now()
	conn.SetReadDeadline(started.Add(TicketTimeout))
	br := bufio.NewReaderSize(conn, MaxTicketBytes+2)
	line, err := br.ReadSlice('\n')
	if err != nil {
		log.Warn("a connection named no ticket", "err", err.Error())
		return
	}
	conn.SetReadDeadline(time.Time{})
	ticket := strings.TrimSpace(string(line))
	if err := CheckTicket(ticket); err != nil {
		log.Warn("refused a connection", "err", err.Error())
		return
	}
	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(dctx, c.Dumbpipe, "connect", ticket)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Warn("dumbpipe stdin", "err", err.Error())
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Warn("dumbpipe stdout", "err", err.Error())
		return
	}
	var tail tailBuffer
	cmd.Stderr = &tail
	if err := cmd.Start(); err != nil {
		log.Warn("starting dumbpipe connect", "err", err.Error())
		return
	}
	id := ticket[:min(16, len(ticket))]
	log.Info("dumbpipe connect started", "ticket", id, "pid", cmd.Process.Pid)
	go func() {
		io.Copy(stdin, br)
		stdin.Close()
	}()
	io.Copy(conn, stdout)
	cancel()
	err = cmd.Wait()
	log.Info("dumbpipe connect ended", "ticket", id, "seconds", time.Since(started).Seconds(), "exit", fmt.Sprint(err),
		"stderr", strings.TrimSpace(tail.String()))
}

type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > stderrTail {
		t.buf = t.buf[len(t.buf)-stderrTail:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
