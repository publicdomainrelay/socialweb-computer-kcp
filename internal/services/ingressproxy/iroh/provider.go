package iroh

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterlink"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/irohdial"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/ingressproxy/loopback"
)

const (
	URLScheme = "iroh"

	DefaultListenHost = "127.0.0.1"

	TicketTimeout = 20 * time.Second

	MinRestartBackoff = time.Second

	MaxRestartBackoff = 30 * time.Second
)

type Config struct {
	DumbpipePath string

	RelayURL string

	ListenHost string
}

func (c Config) Check() error {
	path := strings.TrimSpace(c.DumbpipePath)
	switch {
	case path == "":
		return errors.New("ingressproxy iroh: spec.iroh.dumbpipePath is required")
	case !filepath.IsAbs(path):
		return fmt.Errorf("ingressproxy iroh: dumbpipe %q is not an absolute path", path)
	}
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("ingressproxy iroh: dumbpipe: %w", err)
	}
	if st.IsDir() || st.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("ingressproxy iroh: dumbpipe %s is not an executable file", path)
	}
	if relay := strings.TrimSpace(c.RelayURL); relay != "" {
		parsed, err := url.Parse(relay)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("ingressproxy iroh: relayURL %q is not an absolute URL", relay)
		}
	}
	host := strings.TrimSpace(c.ListenHost)
	if host != "" && strings.ContainsAny(host, ":/[]") {
		return fmt.Errorf("ingressproxy iroh: listenHost %q is a host without a port", host)
	}
	return nil
}

func (c Config) listenHost() string {
	host := strings.TrimSpace(c.ListenHost)
	if host == "" {
		return DefaultListenHost
	}
	return host
}

type Provider struct {
	cfg Config

	mu sync.Mutex

	log *slog.Logger

	deps component.Deps

	ctx context.Context

	cancel context.CancelFunc

	stopCh chan struct{}

	stopOnce sync.Once

	ready bool

	registrations map[*registration]struct{}
}

func New(cfg Config) *Provider {
	return &Provider{cfg: cfg}
}

func (p *Provider) Start(ctx context.Context, deps component.Deps) error {
	if err := p.cfg.Check(); err != nil {
		return err
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	runCtx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	if p.ctx != nil {
		p.mu.Unlock()
		cancel()
		return errors.New("ingressproxy iroh: the provider already started")
	}
	p.deps = deps
	p.log = log
	p.ctx = runCtx
	p.cancel = cancel
	p.stopCh = make(chan struct{})
	p.stopOnce = sync.Once{}
	p.ready = true
	p.registrations = map[*registration]struct{}{}
	stopCh := p.stopCh
	p.mu.Unlock()

	log.Info("ingressproxy_iroh_started", "provider", "iroh", "dumbpipe", p.cfg.DumbpipePath,
		"listenHost", p.cfg.listenHost(), "relay", p.cfg.RelayURL)

	select {
	case <-ctx.Done():
	case <-stopCh:
	}
	cancel()
	p.mu.Lock()
	p.ready = false
	p.ctx = nil
	p.cancel = nil
	p.mu.Unlock()
	p.closeAll()
	return nil
}

func (p *Provider) Ready(ctx context.Context) error {
	p.mu.Lock()
	ready := p.ready
	p.mu.Unlock()
	if !ready {
		return errors.New("ingressproxy iroh: the provider is not started")
	}
	return nil
}

func (p *Provider) Stop(ctx context.Context) error {
	p.mu.Lock()
	stopCh := p.stopCh
	p.mu.Unlock()
	if stopCh == nil {
		return nil
	}
	p.stopOnce.Do(func() { close(stopCh) })
	p.closeAll()
	return nil
}

func (p *Provider) Endpoints() []component.Endpoint {
	return nil
}

func (p *Provider) IngressRegistrar() exposure.IngressRegistrar {
	return p
}

func (p *Provider) Register(ctx context.Context, spec exposure.IngressSpec) (exposure.IngressRegistration, error) {
	p.mu.Lock()
	ready := p.ready
	root := p.ctx
	deps := p.deps
	log := p.log
	p.mu.Unlock()
	if !ready || root == nil {
		return nil, errors.New("ingressproxy iroh: the provider is not started")
	}
	if deps.Handlers == nil {
		return nil, errors.New("ingressproxy iroh: the handler registry is not configured")
	}
	bridge, err := loopback.Start(loopback.Config{
		Host:     p.cfg.listenHost(),
		Protocol: string(spec.Protocol),
		Target:   spec.Target,
		Handler:  spec.Handler,
		Handlers: deps.Handlers,
		Caller:   spec.Caller(),
	})
	if err != nil {
		return nil, err
	}
	secret, err := p.secret(spec.Workspace, spec.Exposure)
	if err != nil {
		_ = bridge.Close()
		return nil, err
	}
	runCtx, cancel := context.WithCancel(root)
	reg := &registration{
		provider: p,
		spec:     spec,
		bridge:   bridge,
		secret:   secret,
		log:      log.With("workspace", spec.Workspace, "exposure", spec.Exposure),
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	child, err := reg.startChild(runCtx)
	if err != nil {
		cancel()
		_ = bridge.Close()
		return nil, err
	}
	reg.mu.Lock()
	reg.cmd = child.cmd
	reg.url = URLScheme + "://" + child.ticket
	reg.mu.Unlock()
	p.mu.Lock()
	if p.registrations == nil {
		p.registrations = map[*registration]struct{}{}
	}
	p.registrations[reg] = struct{}{}
	running := p.ctx == root
	p.mu.Unlock()
	go reg.supervise(runCtx, child)
	if !running {
		cancel()
		_ = reg.Close()
		return nil, errors.New("ingressproxy iroh: the provider stopped")
	}
	return reg, nil
}

func (p *Provider) closeAll() {
	p.mu.Lock()
	registrations := p.registrations
	p.registrations = nil
	p.mu.Unlock()
	for registration := range registrations {
		_ = registration.Close()
	}
}

func (p *Provider) forget(registration *registration) {
	p.mu.Lock()
	if p.registrations != nil {
		delete(p.registrations, registration)
	}
	p.mu.Unlock()
}

func (p *Provider) secret(workspace, exposure string) (string, error) {
	dir := filepath.Join(p.deps.StateDir, "iroh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("ingressproxy iroh: the secret dir: %w", err)
	}
	sum := sha256.Sum256([]byte(workspace + "/" + exposure))
	path := filepath.Join(dir, hex.EncodeToString(sum[:8])+".secret")
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		secret := strings.TrimSpace(string(data))
		if !validSecret(secret) {
			return "", fmt.Errorf("ingressproxy iroh: %s holds no 32-byte hex iroh secret", path)
		}
		return secret, nil
	case !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("ingressproxy iroh: reading %s: %w", path, err)
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return "", fmt.Errorf("ingressproxy iroh: the secret: %w", err)
	}
	secret := hex.EncodeToString(seed)
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		return "", fmt.Errorf("ingressproxy iroh: writing %s: %w", path, err)
	}
	return secret, nil
}

func validSecret(secret string) bool {
	if len(secret) != 64 {
		return false
	}
	_, err := hex.DecodeString(secret)
	return err == nil
}

type registration struct {
	provider *Provider

	spec exposure.IngressSpec

	bridge *loopback.Bridge

	secret string

	log *slog.Logger

	cancel context.CancelFunc

	done chan struct{}

	closeOnce sync.Once

	mu sync.Mutex

	cmd *exec.Cmd

	url string
}

func (r *registration) URL() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.url
}

func (r *registration) Provider() string {
	return "iroh (" + URLScheme + "://<ticket>)"
}

func (r *registration) Close() error {
	r.closeOnce.Do(func() {
		r.cancel()
		r.mu.Lock()
		cmd := r.cmd
		r.mu.Unlock()
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = r.bridge.Close()
		<-r.done
		r.provider.forget(r)
	})
	return nil
}

type child struct {
	cmd *exec.Cmd

	ticket string

	tail *tailBuffer

	started time.Time
}

func (r *registration) startChild(ctx context.Context) (*child, error) {
	cmd := exec.CommandContext(ctx, r.provider.cfg.DumbpipePath, "listen-tcp", "--host", r.bridge.Address())
	cmd.Env = childEnv(r.secret, r.provider.cfg.RelayURL)
	cmd.Stdout = io.Discard
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("ingressproxy iroh: dumbpipe stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ingressproxy iroh: starting dumbpipe: %w", err)
	}
	tail := &tailBuffer{}
	found := make(chan string, 1)
	go pump(stderr, tail, found)
	timer := time.NewTimer(TicketTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			killChild(cmd)
			return nil, ctx.Err()
		case <-timer.C:
			killChild(cmd)
			return nil, fmt.Errorf("ingressproxy iroh: dumbpipe printed no ticket within %s: %s", TicketTimeout, strings.TrimSpace(tail.String()))
		case ticket, ok := <-found:
			if !ok {
				killChild(cmd)
				return nil, fmt.Errorf("ingressproxy iroh: dumbpipe ended before printing a ticket: %s", strings.TrimSpace(tail.String()))
			}
			if err := irohdial.CheckTicket(ticket); err != nil {
				killChild(cmd)
				return nil, fmt.Errorf("ingressproxy iroh: dumbpipe printed %q: %w", ticket, err)
			}
			if _, err := clusterlink.ParseTicket(ticket); err != nil {
				killChild(cmd)
				return nil, fmt.Errorf("ingressproxy iroh: dumbpipe printed %q: %w", ticket, err)
			}
			r.log.Info("ingressproxy_iroh_dumbpipe_listening", "address", r.bridge.Address(), "ticket", ticket)
			return &child{cmd: cmd, ticket: ticket, tail: tail, started: time.Now()}, nil
		}
	}
}

func (r *registration) supervise(ctx context.Context, first *child) {
	defer close(r.done)
	current := first
	backoff := MinRestartBackoff
	for {
		if current == nil {
			next, err := r.startChild(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				r.log.Warn("ingressproxy_iroh_dumbpipe_restart_failed", "err", err.Error())
				backoff = nextBackoff(backoff)
				if !sleepCtx(ctx, backoff) {
					return
				}
				continue
			}
			current = next
			r.mu.Lock()
			r.cmd = next.cmd
			r.url = URLScheme + "://" + next.ticket
			r.mu.Unlock()
		}
		started := current.started
		err := current.cmd.Wait()
		stderr := strings.TrimSpace(current.tail.String())
		r.mu.Lock()
		if r.cmd == current.cmd {
			r.cmd = nil
		}
		r.mu.Unlock()
		current = nil
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= time.Minute {
			backoff = MinRestartBackoff
		}
		r.log.Warn("ingressproxy_iroh_dumbpipe_exited", "err", fmt.Sprint(err), "stderr", stderr)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

func pump(reader io.Reader, tail *tailBuffer, found chan<- string) {
	defer close(found)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 64*1024)
	for scanner.Scan() {
		line := scanner.Text()
		tail.Write([]byte(line + "\n"))
		if ticket, ok := ticketIn(line); ok {
			select {
			case found <- ticket:
			default:
			}
		}
	}
}

func ticketIn(line string) (string, bool) {
	_, rest, ok := strings.Cut(line, "dumbpipe connect-tcp ")
	if !ok {
		return "", false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", false
	}
	ticket := fields[len(fields)-1]
	if !strings.HasPrefix(ticket, clusterlink.TicketPrefix) {
		return "", false
	}
	return ticket, true
}

func childEnv(secret, relay string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == clusterlink.EnvSecret || key == "IROH_RELAY_URL" {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, clusterlink.EnvSecret+"="+secret)
	if relay = strings.TrimSpace(relay); relay != "" {
		env = append(env, "IROH_RELAY_URL="+relay)
	}
	return env
}

func killChild(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		return MinRestartBackoff
	}
	next := current * 2
	if next > MaxRestartBackoff {
		return MaxRestartBackoff
	}
	return next
}

type tailBuffer struct {
	mu sync.Mutex

	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2048 {
		t.buf = t.buf[len(t.buf)-2048:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
