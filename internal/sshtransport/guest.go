package sshtransport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/irohdial"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

const (
	TerminatedSessionExitCode = 255

	GuestPollInterval = 5 * time.Second

	DefaultGuestConnectTimeout = 30 * time.Second

	DefaultGuestPollConnectTimeout = 10 * time.Second
)

type NonIrohGuestAddressError struct {
	Target string
}

func (e *NonIrohGuestAddressError) Error() string {
	return "guest address is not an iroh ticket (iroh://...): " + e.Target
}

type MissingGuestHostKeysError struct {
	Target string
}

func (e *MissingGuestHostKeysError) Error() string {
	return "no reported ssh host key for " + e.Target + "; refusing to connect without one"
}

type GuestHostKeyMismatchError struct {
	Alias string
}

func (e *GuestHostKeyMismatchError) Error() string {
	return "the guest's ssh host key is not the one it reported (" + e.Alias + "); refusing to authenticate"
}

type GuestHostKeys struct {
	Alias string

	Keys []string
}

type Dialer interface {
	Dial(ctx context.Context, target string) (net.Conn, error)
}

type UnixSocketDialer struct {
	Socket string

	Timeout time.Duration

	Log *slog.Logger
}

func (d UnixSocketDialer) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

func (d UnixSocketDialer) Dial(ctx context.Context, target string) (net.Conn, error) {
	if err := irohdial.CheckTicket(target); err != nil {
		return nil, &NonIrohGuestAddressError{Target: target}
	}
	timeout := d.Timeout
	timeout = opt.Positive(timeout, DefaultGuestConnectTimeout)
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "unix", d.Socket)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write([]byte(target + "\n")); err != nil {
		conn.Close()
		return nil, err
	}
	d.log().Info("guest_iroh_dial_started", "socket", d.Socket)
	return conn, nil
}

type DumbpipeDialer struct {
	Path string

	Log *slog.Logger
}

func (d DumbpipeDialer) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

func (d DumbpipeDialer) Dial(ctx context.Context, target string) (net.Conn, error) {
	if err := irohdial.CheckTicket(target); err != nil {
		return nil, &NonIrohGuestAddressError{Target: target}
	}
	cmd := exec.CommandContext(ctx, d.Path, "connect", target)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	tail := &tailWriter{limit: 2048}
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	d.log().Info("guest_iroh_dial_started", "dumbpipe", d.Path, "pid", cmd.Process.Pid)
	stream := &dumbpipeConn{stdin: stdin, stdout: stdout, cmd: cmd, tail: tail, log: d.log()}
	return &streamConn{ReadWriteCloser: stream, local: dumbpipeAddr{target: "dumbpipe"}, remote: dumbpipeAddr{target: target}}, nil
}

type dumbpipeConn struct {
	stdin io.WriteCloser

	stdout io.ReadCloser

	cmd *exec.Cmd

	tail *tailWriter

	log *slog.Logger

	once sync.Once
}

func (c *dumbpipeConn) Read(p []byte) (int, error) {
	return c.stdout.Read(p)
}

func (c *dumbpipeConn) Write(p []byte) (int, error) {
	return c.stdin.Write(p)
}

func (c *dumbpipeConn) Close() error {
	c.once.Do(func() {
		c.stdin.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		err := c.cmd.Wait()
		c.log.Info("guest_iroh_dial_ended", "exit", fmt.Sprint(err), "stderr", strings.TrimSpace(c.tail.String()))
	})
	return nil
}

type streamConn struct {
	io.ReadWriteCloser

	local net.Addr

	remote net.Addr
}

func (c *streamConn) LocalAddr() net.Addr { return c.local }

func (c *streamConn) RemoteAddr() net.Addr { return c.remote }

func (c *streamConn) SetDeadline(t time.Time) error {
	if conn, ok := c.ReadWriteCloser.(net.Conn); ok {
		return conn.SetDeadline(t)
	}
	return nil
}

func (c *streamConn) SetReadDeadline(t time.Time) error {
	if conn, ok := c.ReadWriteCloser.(net.Conn); ok {
		return conn.SetReadDeadline(t)
	}
	return nil
}

func (c *streamConn) SetWriteDeadline(t time.Time) error {
	if conn, ok := c.ReadWriteCloser.(net.Conn); ok {
		return conn.SetWriteDeadline(t)
	}
	return nil
}

type dumbpipeAddr struct {
	target string
}

func (a dumbpipeAddr) Network() string { return "dumbpipe" }

func (a dumbpipeAddr) String() string { return a.target }

type tailWriter struct {
	mu sync.Mutex

	buf []byte

	limit int
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if t.limit > 0 && len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

type SessionProvider interface {
	GenerateKeypair(dir, vmName string) (string, string, error)

	PollReady(ctx context.Context, privateKeyPath, target string, timeout time.Duration, hostKeys GuestHostKeys) bool

	RunSession(ctx context.Context, privateKeyPath, target, program string, commandIO CommandIO, hostKeys GuestHostKeys) int
}

type GuestSessionProvider struct {
	Dialer Dialer

	Log *slog.Logger

	ConnectTimeout time.Duration
}

func (p *GuestSessionProvider) GenerateKeypair(dir, vmName string) (string, string, error) {
	return GenerateGuestKeypair(dir, vmName)
}

func NewGuestSessionProvider(dialer Dialer, log *slog.Logger) (*GuestSessionProvider, error) {
	if dialer == nil {
		return nil, errors.New("sshtransport: guest session provider has no dialer")
	}
	if log == nil {
		log = slog.Default()
	}
	return &GuestSessionProvider{Dialer: dialer, Log: log}, nil
}

func (p *GuestSessionProvider) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

func (p *GuestSessionProvider) timeout() time.Duration {
	if p.ConnectTimeout > 0 {
		return p.ConnectTimeout
	}
	return DefaultGuestConnectTimeout
}

func GenerateGuestKeypair(dir, vmName string) (string, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "root@"+vmName)
	if err != nil {
		return "", "", err
	}
	tempDir, err := os.MkdirTemp(dir, "ssh-"+keyDirName(vmName)+"-")
	if err != nil {
		return "", "", err
	}
	path := filepath.Join(tempDir, "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		os.RemoveAll(tempDir)
		return "", "", err
	}
	sshPublic, err := ssh.NewPublicKey(pub)
	if err != nil {
		os.RemoveAll(tempDir)
		return "", "", err
	}
	publicKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPublic))) + " root@" + vmName
	return publicKey, path, nil
}

func RemoveGuestKeypair(privateKeyPath string) error {
	if privateKeyPath == "" {
		return nil
	}
	return os.RemoveAll(filepath.Dir(privateKeyPath))
}

func keyDirName(vmName string) string {
	var b strings.Builder
	for _, r := range vmName {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		if b.Len() >= 40 {
			break
		}
	}
	if b.Len() == 0 {
		return "guest"
	}
	return b.String()
}

func reportedKeySet(hostKeys GuestHostKeys) map[string]bool {
	accepted := make(map[string]bool, len(hostKeys.Keys))
	for _, key := range hostKeys.Keys {
		if parts := strings.Fields(key); len(parts) >= 2 {
			accepted[parts[1]] = true
			continue
		}
		accepted[key] = true
	}
	return accepted
}

func reportedKeyAlgorithms(hostKeys GuestHostKeys) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(hostKeys.Keys))
	add := func(algo string) {
		if algo == "" || seen[algo] {
			return
		}
		seen[algo] = true
		out = append(out, algo)
	}
	for _, line := range hostKeys.Keys {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case ssh.KeyAlgoRSA:
			add(ssh.KeyAlgoRSASHA256)
			add(ssh.KeyAlgoRSASHA512)
			add(ssh.KeyAlgoRSA)
		default:
			add(fields[0])
		}
	}
	return out
}

func (p *GuestSessionProvider) connect(ctx context.Context, target, privateKeyPath string, hostKeys GuestHostKeys) (*ssh.Client, net.Conn, error) {
	if err := irohdial.CheckTicket(target); err != nil {
		return nil, nil, &NonIrohGuestAddressError{Target: target}
	}
	if len(hostKeys.Keys) == 0 {
		return nil, nil, &MissingGuestHostKeysError{Target: target}
	}
	privateKey, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, nil, err
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, nil, err
	}
	conn, err := p.Dialer.Dial(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	accepted := reportedKeySet(hostKeys)
	mismatch := false
	config := &ssh.ClientConfig{
		User:              "root",
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		Timeout:           p.timeout(),
		HostKeyAlgorithms: reportedKeyAlgorithms(hostKeys),
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			got := base64.StdEncoding.EncodeToString(key.Marshal())
			if accepted[got] {
				return nil
			}
			mismatch = true
			p.log().Warn("guest_host_key_mismatch", "target", target, "alias", hostKeys.Alias)
			return fmt.Errorf("sshtransport: guest host key does not match %s", hostKeys.Alias)
		},
	}
	_ = conn.SetDeadline(time.Now().Add(p.timeout()))
	clientConn, channels, requests, err := ssh.NewClientConn(conn, target, config)
	if err != nil {
		conn.Close()
		if mismatch {
			return nil, nil, &GuestHostKeyMismatchError{Alias: hostKeys.Alias}
		}
		return nil, nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(clientConn, channels, requests), conn, nil
}

func (p *GuestSessionProvider) PollReady(ctx context.Context, privateKeyPath, target string, timeout time.Duration, hostKeys GuestHostKeys) bool {
	if err := irohdial.CheckTicket(target); err != nil {
		p.log().Warn("guest_ssh_refused", "target", target, "err", err.Error())
		return false
	}
	if len(hostKeys.Keys) == 0 {
		p.log().Warn("guest_ssh_refused", "target", target, "err", "no reported ssh host key")
		return false
	}
	if _, err := os.Stat(privateKeyPath); err != nil {
		return false
	}
	deadline := time.Now().Add(timeout)
	attempt := 0
	for time.Now().Before(deadline) && ctx.Err() == nil {
		attempt++
		ready := p.pollOnce(ctx, privateKeyPath, target, hostKeys)
		if ready {
			p.log().Info("guest_ssh_ready", "target", target, "attempt", attempt)
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(GuestPollInterval):
		}
	}
	if ctx.Err() != nil {
		p.log().Info("guest_ssh_poll_terminated", "target", target, "attempt", attempt)
		return false
	}
	p.log().Warn("guest_ssh_timeout", "target", target, "timeoutMs", timeout.Milliseconds())
	return false
}

func (p *GuestSessionProvider) pollOnce(ctx context.Context, privateKeyPath, target string, hostKeys GuestHostKeys) bool {
	cctx, cancel := context.WithTimeout(ctx, DefaultGuestPollConnectTimeout)
	defer cancel()
	client, conn, err := p.connect(cctx, target, privateKeyPath, hostKeys)
	if err != nil {
		var mismatch *GuestHostKeyMismatchError
		if errors.As(err, &mismatch) {
			return false
		}
		p.log().Info("guest_ssh_poll", "target", target, "err", err)
		return false
	}
	defer client.Close()
	defer conn.Close()
	session, err := client.NewSession()
	if err != nil {
		return false
	}
	defer session.Close()
	if err := session.Run("true"); err != nil {
		return false
	}
	return true
}

func (p *GuestSessionProvider) RunSession(ctx context.Context, privateKeyPath, target, program string, commandIO CommandIO, hostKeys GuestHostKeys) int {
	if err := irohdial.CheckTicket(target); err != nil {
		p.log().Warn("guest_session_refused", "target", target, "err", err.Error())
		_ = commandIO.WriteErr([]byte("guest session failed: " + err.Error() + "\n"))
		return 1
	}
	if len(hostKeys.Keys) == 0 {
		err := &MissingGuestHostKeysError{Target: target}
		p.log().Warn("guest_session_refused", "target", target, "err", err.Error())
		_ = commandIO.WriteErr([]byte("guest session failed: " + err.Error() + "\n"))
		return 1
	}
	if ctx.Err() != nil {
		p.log().Info("guest_session_terminated", "target", target, "phase", "before_connect")
		return TerminatedSessionExitCode
	}
	client, conn, err := p.connect(ctx, target, privateKeyPath, hostKeys)
	if err != nil {
		if ctx.Err() != nil {
			p.log().Info("guest_session_terminated", "target", target, "phase", "connect")
			return TerminatedSessionExitCode
		}
		p.log().Warn("guest_session_failed", "target", target, "err", err)
		_ = commandIO.WriteErr([]byte("guest session failed: " + err.Error() + "\n"))
		return 1
	}
	defer client.Close()
	defer conn.Close()
	session, err := client.NewSession()
	if err != nil {
		p.log().Warn("guest_session_failed", "target", target, "err", err)
		_ = commandIO.WriteErr([]byte("guest session failed: " + err.Error() + "\n"))
		return 1
	}
	defer session.Close()
	if pty := commandIO.PTY(); pty != nil {
		modes := ssh.TerminalModes{
			ssh.ECHO:          1,
			ssh.TTY_OP_ISPEED: 14400,
			ssh.TTY_OP_OSPEED: 14400,
		}
		if err := session.RequestPty("xterm", pty.Rows, pty.Cols, modes); err != nil {
			p.log().Warn("guest_pty_failed", "target", target, "err", err)
			_ = commandIO.WriteErr([]byte("guest session failed: " + err.Error() + "\n"))
			return 1
		}
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		return 1
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return 1
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return 1
	}
	if err := session.Start(program); err != nil {
		p.log().Warn("guest_exec_failed", "target", target, "err", err)
		_ = commandIO.WriteErr([]byte("guest session failed: " + err.Error() + "\n"))
		return 1
	}
	p.log().Info("guest_session_started", "target", target)
	commandIO.OnData(func(chunk []byte) {
		if _, err := stdin.Write(chunk); err != nil {
			_ = stdin.Close()
		}
	})
	commandIO.OnClose(func() {
		_ = stdin.Close()
	})
	var copying sync.WaitGroup
	copying.Go(func() {
		_, _ = io.Copy(writeFunc(commandIO.Write), stdout)
	})
	copying.Go(func() {
		_, _ = io.Copy(writeFunc(commandIO.WriteErr), stderr)
	})
	done := make(chan struct{})
	var watching sync.WaitGroup
	watching.Go(func() {
		select {
		case <-ctx.Done():
			client.Close()
		case <-done:
		}
	})
	err = session.Wait()
	close(done)
	watching.Wait()
	copying.Wait()
	if ctx.Err() != nil {
		p.log().Info("guest_session_terminated", "target", target, "phase", "session")
		return TerminatedSessionExitCode
	}
	if err == nil {
		p.log().Info("guest_session_exit", "target", target, "code", 0)
		return 0
	}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		p.log().Info("guest_session_exit", "target", target, "code", exitErr.ExitStatus())
		return exitErr.ExitStatus()
	}
	p.log().Warn("guest_session_failed", "target", target, "err", err)
	_ = commandIO.WriteErr([]byte("guest session failed: " + err.Error() + "\n"))
	return 1
}

type writeFunc func([]byte) error

func (f writeFunc) Write(p []byte) (int, error) {
	if err := f(p); err != nil {
		return 0, err
	}
	return len(p), nil
}
