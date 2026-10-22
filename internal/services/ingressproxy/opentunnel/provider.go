package opentunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/ingressproxy/loopback"
)

const (
	DefaultListenHost = "127.0.0.1"

	URLTimeout = 30 * time.Second

	MinRestartBackoff = time.Second

	MaxRestartBackoff = 30 * time.Second
)

var urlPattern = regexp.MustCompile(`https://[^\s"'<>]+`)

type Config struct {
	Command []string

	RouteName string

	ProxyProtocol string

	LocalHost string
}

func (c Config) Check() error {
	command := c.command()
	path := strings.TrimSpace(command[0])
	switch {
	case path == "":
		return errors.New("ingressproxy opentunnel: spec.opentunnel.command[0] is empty")
	case filepath.IsAbs(path):
		st, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("ingressproxy opentunnel: command: %w", err)
		}
		if st.IsDir() || st.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("ingressproxy opentunnel: command %s is not an executable file", path)
		}
	default:
		if _, err := exec.LookPath(path); err != nil {
			return fmt.Errorf("ingressproxy opentunnel: command %q: %w", path, err)
		}
	}
	switch strings.ToLower(strings.TrimSpace(c.ProxyProtocol)) {
	case "", "v1", "v2":
	default:
		return fmt.Errorf("ingressproxy opentunnel: proxyProtocol %q is not v1 or v2", c.ProxyProtocol)
	}
	host := strings.TrimSpace(c.LocalHost)
	if host != "" && strings.ContainsAny(host, ":/[]") {
		return fmt.Errorf("ingressproxy opentunnel: localHost %q is a host without a port", host)
	}
	return nil
}

func (c Config) command() []string {
	if len(c.Command) == 0 {
		return []string{"opentunnel"}
	}
	return append([]string(nil), c.Command...)
}

func (c Config) localHost() string {
	host := strings.TrimSpace(c.LocalHost)
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

	args []string

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
	if ctx == nil {
		ctx = context.Background()
	}
	if err := p.cfg.Check(); err != nil {
		return err
	}
	args := p.cfg.command()
	path := args[0]
	if !filepath.IsAbs(path) {
		resolved, err := exec.LookPath(path)
		if err != nil {
			return fmt.Errorf("ingressproxy opentunnel: command %q: %w", path, err)
		}
		path = resolved
	}
	args[0] = path
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	runCtx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	if p.ctx != nil {
		p.mu.Unlock()
		cancel()
		return errors.New("ingressproxy opentunnel: the provider already started")
	}
	p.deps = deps
	p.log = log
	p.args = args
	p.ctx = runCtx
	p.cancel = cancel
	p.stopCh = make(chan struct{})
	p.stopOnce = sync.Once{}
	p.ready = true
	p.registrations = map[*registration]struct{}{}
	stopCh := p.stopCh
	p.mu.Unlock()

	log.Info("ingressproxy_opentunnel_started", "provider", "opentunnel", "command", strings.Join(args, " "),
		"localHost", p.cfg.localHost(), "route", p.cfg.RouteName, "proxyProtocol", p.cfg.ProxyProtocol)

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
		return errors.New("ingressproxy opentunnel: the provider is not started")
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
	args := append([]string(nil), p.args...)
	log := p.log
	p.mu.Unlock()
	if !ready || root == nil || len(args) == 0 {
		return nil, errors.New("ingressproxy opentunnel: the provider is not started")
	}
	if deps.Handlers == nil {
		return nil, errors.New("ingressproxy opentunnel: the handler registry is not configured")
	}
	bridge, err := loopback.Start(loopback.Config{
		Host:     p.cfg.localHost(),
		Protocol: string(spec.Protocol),
		Target:   spec.Target,
		Handler:  spec.Handler,
		Handlers: deps.Handlers,
	})
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(root)
	reg := &registration{
		provider: p,
		spec:     spec,
		bridge:   bridge,
		args:     args,
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
	reg.url = child.url + spec.Path
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
		return nil, errors.New("ingressproxy opentunnel: the provider stopped")
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

type registration struct {
	provider *Provider

	spec exposure.IngressSpec

	bridge *loopback.Bridge

	args []string

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
	return "opentunnel (https URL)"
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

	url string

	tail *tailBuffer

	started time.Time
}

func (r *registration) startChild(ctx context.Context) (*child, error) {
	args := append([]string(nil), r.args...)
	_, port, err := net.SplitHostPort(r.bridge.Address())
	if err != nil {
		return nil, fmt.Errorf("ingressproxy opentunnel: the loopback address %q: %w", r.bridge.Address(), err)
	}
	args = append(args, "route", "add", port)
	if name := strings.TrimSpace(r.provider.cfg.RouteName); name != "" {
		args = append(args, "--name", name)
	}
	switch strings.ToLower(strings.TrimSpace(r.provider.cfg.ProxyProtocol)) {
	case "v1", "v2":
		args = append(args, "--proxy-protocol", strings.ToLower(strings.TrimSpace(r.provider.cfg.ProxyProtocol)))
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = os.Environ()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ingressproxy opentunnel: stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("ingressproxy opentunnel: stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ingressproxy opentunnel: starting %s: %w", args[0], err)
	}
	tail := &tailBuffer{}
	found := make(chan string, 1)
	var pumps sync.WaitGroup
	pumps.Add(2)
	go pump(stdout, tail, found, pumps.Done)
	go pump(stderr, tail, found, pumps.Done)
	go func() {
		pumps.Wait()
		close(found)
	}()
	timer := time.NewTimer(URLTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			killChild(cmd)
			return nil, ctx.Err()
		case <-timer.C:
			killChild(cmd)
			return nil, fmt.Errorf("ingressproxy opentunnel: %s printed no URL within %s: %s", args[0], URLTimeout, strings.TrimSpace(tail.String()))
		case raw, ok := <-found:
			if !ok {
				killChild(cmd)
				return nil, fmt.Errorf("ingressproxy opentunnel: %s ended before printing a URL: %s", args[0], strings.TrimSpace(tail.String()))
			}
			r.log.Info("ingressproxy_opentunnel_route_added", "address", r.bridge.Address(), "url", raw)
			return &child{cmd: cmd, url: raw, tail: tail, started: time.Now()}, nil
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
				r.log.Warn("ingressproxy_opentunnel_restart_failed", "err", err.Error())
				backoff = nextBackoff(backoff)
				if !sleepCtx(ctx, backoff) {
					return
				}
				continue
			}
			current = next
			r.mu.Lock()
			r.cmd = next.cmd
			r.url = next.url + r.spec.Path
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
		r.log.Warn("ingressproxy_opentunnel_exited", "err", fmt.Sprint(err), "output", stderr)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

func pump(reader io.Reader, tail *tailBuffer, found chan<- string, done func()) {
	defer done()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 64*1024)
	for scanner.Scan() {
		line := scanner.Text()
		tail.Write([]byte(line + "\n"))
		if raw, ok := urlIn(line); ok {
			select {
			case found <- raw:
			default:
			}
		}
	}
}

func urlIn(line string) (string, bool) {
	for _, match := range urlPattern.FindAllString(line, -1) {
		candidate := strings.TrimRight(match, ".,;:)]}")
		parsed, err := url.Parse(candidate)
		if err != nil || parsed.Host == "" {
			continue
		}
		host := strings.ToLower(parsed.Hostname())
		if host == "opentunnel.xyz" || strings.HasSuffix(host, ".opentunnel.xyz") {
			return candidate, true
		}
	}
	return "", false
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
