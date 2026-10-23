package sshtransport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	NoKeyCouldMatchExtension = "sshtransport.socialweb.computer/no-key-could-match"

	PermissionDID = "sshtransport.socialweb.computer/did"

	PermissionHandle = "sshtransport.socialweb.computer/handle"

	PermissionPDS = "sshtransport.socialweb.computer/pds"

	DefaultMaxConnections = 10000

	DefaultMaxSessions = 1000

	DefaultSessionsPerAccount = 1000

	DefaultMaxAuthAttempts = 6

	DefaultAuthTimeout = 120 * time.Second

	DefaultAuthorizationLookupTimeout = 30 * time.Second

	DefaultShutdownGrace = 5 * time.Second
)

var (
	allowedKeyAlgorithms = []string{
		"ssh-ed25519",
		"ecdsa-sha2-nistp256",
		"ecdsa-sha2-nistp384",
		"ecdsa-sha2-nistp521",
		"sk-ssh-ed25519@openssh.com",
		"sk-ecdsa-sha2-nistp256@openssh.com",
		"rsa-sha2-256",
		"rsa-sha2-512",
		"ssh-rsa",
	}

	allowedSignatureFormats = map[string]bool{
		"ssh-ed25519":                        true,
		"ecdsa-sha2-nistp256":                true,
		"ecdsa-sha2-nistp384":                true,
		"ecdsa-sha2-nistp521":                true,
		"sk-ssh-ed25519@openssh.com":         true,
		"sk-ecdsa-sha2-nistp256@openssh.com": true,
		"rsa-sha2-256":                       true,
		"rsa-sha2-512":                       true,
	}

	newlinePattern = regexp.MustCompile(`\r?\n`)
)

type PTYSize struct {
	Cols int
	Rows int
}

type CommandIO interface {
	Write(chunk []byte) error

	WriteErr(chunk []byte) error

	OnData(handler func(chunk []byte))

	OnClose(handler func())

	Exit(code int)

	PTY() *PTYSize
}

type ComputeCommandRunner interface {
	Run(ctx context.Context, account AuthorizedAccount, command string, env map[string]string, io CommandIO) error
}

type SessionRunnerShutdowner interface {
	Shutdown(ctx context.Context) error
}

type Config struct {
	Port int

	Hostname string

	HostKeyPath string

	Banner string

	NoKeyCouldMatchMessage string

	MaxConnections int

	MaxSessions int

	SessionsPerAccount int

	MaxAuthAttempts int

	AuthTimeout time.Duration

	SessionMax time.Duration
}

type ServerOptions struct {
	Config Config

	Authorizer KeyAuthorizer

	Runner ComputeCommandRunner

	DefaultCommand string

	Log *slog.Logger
}

type Server struct {
	opts Options

	sshConfig *ssh.ServerConfig

	log *slog.Logger

	mu sync.Mutex

	conns map[net.Conn]struct{}

	connWG sync.WaitGroup

	running int

	perAccount map[string]int
}

type Options = ServerOptions

func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Authorizer == nil {
		return nil, errors.New("sshtransport: SSH server has no key authorizer")
	}
	if opts.Runner == nil {
		return nil, errors.New("sshtransport: SSH server has no command runner")
	}
	if opts.Config.HostKeyPath == "" {
		return nil, errors.New("sshtransport: SSH server has no host key path")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	opts.Config = configWithDefaults(opts.Config)
	signer, err := loadOrCreateHostKey(opts.Config.HostKeyPath, opts.Log)
	if err != nil {
		return nil, err
	}
	s := &Server{
		opts:       opts,
		log:        opts.Log,
		conns:      map[net.Conn]struct{}{},
		perAccount: map[string]int{},
	}
	cfg := &ssh.ServerConfig{
		MaxAuthTries:            opts.Config.MaxAuthAttempts,
		PublicKeyAuthAlgorithms: allowedKeyAlgorithms,
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
		VerifiedPublicKeyCallback: s.verifiedPublicKey,
	}
	if opts.Config.Banner != "" {
		banner := opts.Config.Banner
		cfg.BannerCallback = func(ssh.ConnMetadata) string { return banner }
	}
	cfg.AddHostKey(signer)
	s.sshConfig = cfg
	return s, nil
}

func configWithDefaults(c Config) Config {
	if c.MaxConnections <= 0 {
		c.MaxConnections = DefaultMaxConnections
	}
	if c.MaxSessions <= 0 {
		c.MaxSessions = DefaultMaxSessions
	}
	if c.SessionsPerAccount <= 0 {
		c.SessionsPerAccount = DefaultSessionsPerAccount
	}
	if c.MaxAuthAttempts <= 0 {
		c.MaxAuthAttempts = DefaultMaxAuthAttempts
	}
	if c.AuthTimeout <= 0 {
		c.AuthTimeout = DefaultAuthTimeout
	}
	return c
}

func loadOrCreateHostKey(path string, log *slog.Logger) (ssh.Signer, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		signer, err := ssh.ParsePrivateKey(raw)
		if err != nil {
			return nil, fmt.Errorf("%s exists but is not a usable private key; refusing to start with a different identity: %w", path, err)
		}
		if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(path, 0o600); err != nil {
				log.Warn("host_key_permissions_unrepaired", "path", path, "err", err)
			} else {
				log.Info("host_key_permissions_repaired", "path", path)
			}
		}
		return signer, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(pem.EncodeToMemory(block)); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	log.Info("host_key_generated", "path", path)
	return ssh.NewSignerFromKey(priv)
}

func (s *Server) verifiedPublicKey(conn ssh.ConnMetadata, key ssh.PublicKey, permissions *ssh.Permissions, signatureAlgorithm string) (*ssh.Permissions, error) {
	if !allowedSignatureFormats[signatureAlgorithm] {
		s.log.Warn("auth_rejected_algo", "username", conn.User(), "algo", signatureAlgorithm)
		return nil, fmt.Errorf("sshtransport: signature algorithm %q is not accepted", signatureAlgorithm)
	}
	presented := PresentedKey{Algo: key.Type(), Key: base64.StdEncoding.EncodeToString(key.Marshal())}
	ctx, cancel := context.WithTimeout(context.Background(), DefaultAuthorizationLookupTimeout)
	defer cancel()
	account, err := s.opts.Authorizer.Authorize(ctx, conn.User(), presented)
	if err != nil {
		var noKey *NoKeyCouldMatchError
		if errors.As(err, &noKey) {
			s.log.Info("auth_accepted_no_key_could_match", "username", conn.User(), "reason", noKey.Reason)
			return &ssh.Permissions{Extensions: map[string]string{NoKeyCouldMatchExtension: noKey.Reason}}, nil
		}
		s.log.Warn("auth_error", "username", conn.User(), "err", err)
		return nil, err
	}
	if account == nil {
		s.log.Info("auth_rejected", "username", conn.User())
		return nil, errors.New("sshtransport: the offered key is not associated with the account")
	}
	s.log.Info("auth_accepted", "username", conn.User(), "did", account.DID)
	return &ssh.Permissions{Extensions: map[string]string{
		PermissionDID:    account.DID,
		PermissionHandle: account.Handle,
		PermissionPDS:    account.PDS,
	}}, nil
}

func (s *Server) ServeConn(ctx context.Context, conn net.Conn) error {
	if conn == nil {
		return errors.New("sshtransport: no connection")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if len(s.conns) >= s.opts.Config.MaxConnections {
		s.mu.Unlock()
		conn.Close()
		return errors.New("sshtransport: at the concurrent connection limit")
	}
	s.conns[conn] = struct{}{}
	s.connWG.Add(1)
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	s.handleConnection(ctx, conn)
	close(done)
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
	s.connWG.Done()
	return nil
}

func (s *Server) handleConnection(ctx context.Context, raw net.Conn) {
	if s.opts.Config.AuthTimeout > 0 {
		raw.SetDeadline(time.Now().Add(s.opts.Config.AuthTimeout))
	}
	conn, channels, requests, err := ssh.NewServerConn(raw, s.sshConfig)
	if err != nil {
		raw.Close()
		s.log.Warn("connection_error", "err", err)
		return
	}
	raw.SetDeadline(time.Time{})
	defer conn.Close()
	account := accountFromPermissions(conn.Permissions)
	noKeyUsername := ""
	if conn.Permissions != nil {
		if _, ok := conn.Permissions.Extensions[NoKeyCouldMatchExtension]; ok {
			noKeyUsername = conn.User()
		}
	}
	go ssh.DiscardRequests(requests)
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(ssh.UnknownChannelType, "only session channels are served")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			s.log.Warn("channel_accept_failed", "err", err)
			continue
		}
		go s.serveSession(ctx, channel, channelRequests, account, conn.User(), noKeyUsername)
	}
}

func accountFromPermissions(permissions *ssh.Permissions) *AuthorizedAccount {
	if permissions == nil || permissions.Extensions == nil {
		return nil
	}
	did := permissions.Extensions[PermissionDID]
	if did == "" {
		return nil
	}
	return &AuthorizedAccount{
		DID:    did,
		Handle: permissions.Extensions[PermissionHandle],
		PDS:    permissions.Extensions[PermissionPDS],
	}
}

type ptyRequest struct {
	Term          string
	Cols, Rows    uint32
	Width, Height uint32
	Modes         string
}

type windowChangeRequest struct {
	Cols, Rows    uint32
	Width, Height uint32
}

type envRequest struct {
	Name  string
	Value string
}

type execRequest struct {
	Command string
}

func (s *Server) serveSession(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request, account *AuthorizedAccount, username, noKeyUsername string) {
	defer channel.Close()
	io := newChannelIO(channel)
	env := map[string]string{}
	envBytes := 0
	started := false
	for req := range requests {
		switch req.Type {
		case "pty-req":
			var request ptyRequest
			if err := ssh.Unmarshal(req.Payload, &request); err != nil {
				req.Reply(false, nil)
				continue
			}
			io.setPTY(&PTYSize{Cols: int(request.Cols), Rows: int(request.Rows)})
			req.Reply(true, nil)
		case "window-change":
			var request windowChangeRequest
			if err := ssh.Unmarshal(req.Payload, &request); err == nil && io.PTY() != nil {
				io.setSize(int(request.Cols), int(request.Rows))
			}
			req.Reply(false, nil)
		case "env":
			var request envRequest
			if err := ssh.Unmarshal(req.Payload, &request); err != nil {
				req.Reply(false, nil)
				continue
			}
			if len(env) < 64 && envBytes+len(request.Name)+len(request.Value) <= 8192 {
				env[request.Name] = request.Value
				envBytes += len(request.Name) + len(request.Value)
			}
			req.Reply(true, nil)
		case "exec":
			if started {
				req.Reply(false, nil)
				continue
			}
			var request execRequest
			if err := ssh.Unmarshal(req.Payload, &request); err != nil {
				req.Reply(false, nil)
				continue
			}
			started = true
			req.Reply(true, nil)
			go s.startSession(ctx, io, copyEnv(env), account, username, noKeyUsername, request.Command)
		case "shell":
			if started {
				req.Reply(false, nil)
				continue
			}
			started = true
			req.Reply(true, nil)
			go s.startSession(ctx, io, copyEnv(env), account, username, noKeyUsername, s.opts.DefaultCommand)
		default:
			req.Reply(false, nil)
		}
	}
	io.markClosed()
}

func copyEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	maps.Copy(out, env)
	return out
}

func (s *Server) startSession(ctx context.Context, io *channelIO, env map[string]string, account *AuthorizedAccount, username, noKeyUsername, command string) {
	if account == nil {
		explanation := "authentication required\n"
		if noKeyUsername != "" {
			explanation = s.opts.Config.NoKeyCouldMatchMessage
			if explanation == "" {
				explanation = defaultNoKeyMessage(noKeyUsername)
			}
		}
		s.log.Info("session_refused_no_key_could_match", "username", username)
		_ = io.WriteErr([]byte(explanation))
		io.Exit(1)
		return
	}
	if refusal := s.acquire(account.DID); refusal != "" {
		s.log.Info("session_refused", "did", account.DID, "reason", refusal)
		_ = io.WriteErr([]byte(refusal + "\n"))
		io.Exit(1)
		return
	}
	defer s.release(account.DID)
	sessionID := randomHex(16)
	remaining := s.opts.Config.SessionMax
	s.log.Info("session_started", "sessionId", sessionID, "did", account.DID,
		"remainingSec", int(remaining/time.Second), "commandChars", len(command))
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var timer *time.Timer
	if remaining > 0 {
		timer = time.AfterFunc(remaining, func() {
			s.log.Info("session_capped", "sessionId", sessionID, "did", account.DID,
				"remainingSec", int(remaining/time.Second))
			cancel()
			io.Exit(1)
		})
		defer timer.Stop()
	}
	err := s.opts.Runner.Run(sctx, *account, command, env, io)
	if err != nil {
		if io.exited() {
			s.log.Warn("post_exit_error", "did", account.DID, "err", err)
			return
		}
		s.log.Warn("provisioning_failed", "did", account.DID, "err", err)
		_ = io.WriteErr([]byte("provisioning failed: " + err.Error() + "\n"))
		io.Exit(1)
	}
}

func (s *Server) acquire(did string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running >= s.opts.Config.MaxSessions {
		return fmt.Sprintf("server is at its %d concurrent session limit", s.opts.Config.MaxSessions)
	}
	held := s.perAccount[did]
	if held >= s.opts.Config.SessionsPerAccount {
		return fmt.Sprintf("account already has %d sessions running", held)
	}
	s.running++
	s.perAccount[did] = held + 1
	return ""
}

func (s *Server) release(did string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running > 0 {
		s.running--
	}
	held := s.perAccount[did] - 1
	if held <= 0 {
		delete(s.perAccount, did)
	} else {
		s.perAccount[did] = held
	}
}

func (s *Server) closeConnections() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.mu.Unlock()
	for _, conn := range conns {
		conn.Close()
	}
}

func (s *Server) Shutdown(ctx context.Context) error {
	if runner, ok := s.opts.Runner.(SessionRunnerShutdowner); ok {
		grace, stop := context.WithTimeout(ctx, DefaultShutdownGrace)
		_ = runner.Shutdown(grace)
		stop()
	}
	s.closeConnections()
	done := make(chan struct{})
	go func() {
		s.connWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func defaultNoKeyMessage(username string) string {
	return "\n" +
		"socialweb-computer-ssh\n" +
		"\n" +
		"  No SSH key registered for \"" + username + "\" can sign in here.\n" +
		"\n" +
		"  If that is your handle, register an SSH key against the account first.\n" +
		"  If it is not, use your handle as the username -- the account, not just the\n" +
		"  key, is what this door authenticates against.\n" +
		"\n"
}

func randomHex(n int) string {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(raw)
}

type channelIO struct {
	channel ssh.Channel

	mu sync.Mutex

	deliverMu sync.Mutex

	pty *PTYSize

	dataHandler func([]byte)

	closeHandlers []func()

	pending [][]byte

	pendingBytes int

	readerStarted bool

	closed bool

	exitOnce sync.Once

	exitedFlag bool
}

func newChannelIO(channel ssh.Channel) *channelIO {
	return &channelIO{channel: channel}
}

func (c *channelIO) Write(chunk []byte) error {
	_, err := c.channel.Write(forClientTerminal(c.PTY(), chunk))
	return err
}

func (c *channelIO) WriteErr(chunk []byte) error {
	_, err := c.channel.Stderr().Write(forClientTerminal(c.PTY(), chunk))
	return err
}

func (c *channelIO) OnData(handler func(chunk []byte)) {
	c.deliverMu.Lock()
	c.mu.Lock()
	c.dataHandler = handler
	pending := c.pending
	c.pending = nil
	c.pendingBytes = 0
	if !c.readerStarted {
		c.readerStarted = true
		go c.readLoop()
	}
	c.mu.Unlock()
	for _, chunk := range pending {
		handler(chunk)
	}
	c.deliverMu.Unlock()
}

func (c *channelIO) readLoop() {
	buf := make([]byte, 32*1024)
	for {
		n, err := c.channel.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			c.mu.Lock()
			handler := c.dataHandler
			if handler == nil {
				if c.pendingBytes+len(chunk) <= 1<<20 {
					c.pending = append(c.pending, chunk)
					c.pendingBytes += len(chunk)
				}
			}
			c.mu.Unlock()
			if handler != nil {
				c.deliverMu.Lock()
				handler(chunk)
				c.deliverMu.Unlock()
			}
		}
		if err != nil {
			c.markClosed()
			return
		}
	}
}

func (c *channelIO) OnClose(handler func()) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		handler()
		return
	}
	c.closeHandlers = append(c.closeHandlers, handler)
	c.mu.Unlock()
}

func (c *channelIO) markClosed() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	handlers := c.closeHandlers
	c.closeHandlers = nil
	c.mu.Unlock()
	for _, handler := range handlers {
		handler()
	}
}

func (c *channelIO) Exit(code int) {
	c.exitOnce.Do(func() {
		c.mu.Lock()
		c.exitedFlag = true
		c.mu.Unlock()
		_, _ = c.channel.SendRequest("exit-status", false, ssh.Marshal(struct {
			Status uint32
		}{uint32(code)}))
		_ = c.channel.Close()
	})
}

func (c *channelIO) exited() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exitedFlag
}

func (c *channelIO) setPTY(size *PTYSize) {
	c.mu.Lock()
	c.pty = size
	c.mu.Unlock()
}

func (c *channelIO) setSize(cols, rows int) {
	c.mu.Lock()
	if c.pty == nil {
		c.pty = &PTYSize{}
	}
	c.pty.Cols = cols
	c.pty.Rows = rows
	c.mu.Unlock()
}

func (c *channelIO) PTY() *PTYSize {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pty == nil {
		return nil
	}
	copied := *c.pty
	return &copied
}

func forClientTerminal(pty *PTYSize, chunk []byte) []byte {
	if pty == nil || !bytes.Contains(chunk, []byte("\n")) {
		return chunk
	}
	return newlinePattern.ReplaceAll(chunk, []byte("\r\n"))
}
