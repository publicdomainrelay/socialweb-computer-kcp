//go:build linux

package hostsvc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/host"
)

var _ host.Units = (*proc)(nil)

type proc struct {
	socket    string
	prefix    string
	installed string
}

func (p *proc) StartInstalled(ctx context.Context, name string) (UnitStatus, error) {
	spec, err := ReadInstalledSpec(p.installed, name)
	if err != nil {
		return UnitStatus{}, err
	}
	return p.Start(ctx, spec)
}

func openProc(ctx context.Context, cfg Config) (Manager, error) {
	p := &proc{socket: cfg.ProcSocket, prefix: cfg.Prefix(), installed: cfg.InstalledDir}
	if _, err := p.Hello(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *proc) dial(ctx context.Context, timeout time.Duration) (net.Conn, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", p.socket)
	if err != nil {
		return nil, fmt.Errorf("%w: dialing devsup at %s: %w", ErrUnreachable, p.socket, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	return c, nil
}

func (p *proc) call(ctx context.Context, req ProcRequest) (ProcResponse, error) {
	timeout := DefaultCallTimeout
	if req.Op == ProcOpStop || req.Op == ProcOpStopSlice || req.Op == ProcOpShutdown || req.Op == ProcOpCheck {
		timeout = ProcStopCallTimeout
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	c, err := p.dial(ctx, timeout)
	if err != nil {
		return ProcResponse{}, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return ProcResponse{}, fmt.Errorf("%w: writing %s to devsup: %w", ErrUnreachable, req.Op, err)
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return ProcResponse{}, fmt.Errorf("%w: reading the %s answer from devsup: %w", ErrUnreachable, req.Op, err)
	}
	var resp ProcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return ProcResponse{}, fmt.Errorf("hostsvc: devsup answered %s with %q: %w", req.Op, line, err)
	}
	return resp, resp.Error()
}

func (p *proc) owns(name string) error {
	if !strings.HasPrefix(name, p.prefix) {
		return fmt.Errorf("hostsvc: unit %s is outside this manager's prefix %s", name, p.prefix)
	}
	return nil
}

func (p *proc) Hello(ctx context.Context) (ProcHello, error) {
	resp, err := p.call(ctx, ProcRequest{Op: ProcOpHello})
	if err != nil {
		return ProcHello{}, err
	}
	if resp.Hello == nil {
		return ProcHello{}, fmt.Errorf("hostsvc: devsup at %s answered hello without its identity", p.socket)
	}
	return *resp.Hello, nil
}

func (p *proc) Capabilities() Capabilities { return ProcCapabilities() }

func (p *proc) Start(ctx context.Context, spec UnitSpec) (UnitStatus, error) {
	if err := ValidateSpec(spec); err != nil {
		return UnitStatus{}, err
	}
	if err := p.owns(spec.Name); err != nil {
		return UnitStatus{}, err
	}
	if err := RefuseUnsupported(p.Capabilities(), spec); err != nil {
		return UnitStatus{}, err
	}
	resp, err := p.call(ctx, ProcRequest{Op: ProcOpStart, Spec: &spec})
	if err != nil {
		return UnitStatus{}, fmt.Errorf("hostsvc: starting %s: %w", spec.Name, err)
	}
	return *resp.Status, nil
}

func (p *proc) Inspect(ctx context.Context, name string) (UnitStatus, error) {
	if err := ValidateName(name); err != nil {
		return UnitStatus{}, err
	}
	resp, err := p.call(ctx, ProcRequest{Op: ProcOpInspect, Name: name})
	if err != nil {
		return UnitStatus{}, fmt.Errorf("hostsvc: inspecting %s: %w", name, err)
	}
	return *resp.Status, nil
}

func (p *proc) Stop(ctx context.Context, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if _, err := p.call(ctx, ProcRequest{Op: ProcOpStop, Name: name}); err != nil {
		return fmt.Errorf("hostsvc: stopping %s: %w", name, err)
	}
	return nil
}

func (p *proc) StopSlice(ctx context.Context, slice string) error {
	if err := ValidateSlice(slice); err != nil {
		return err
	}
	if _, err := p.call(ctx, ProcRequest{Op: ProcOpStopSlice, Slice: slice}); err != nil {
		return fmt.Errorf("hostsvc: stopping %s: %w", slice, err)
	}
	return nil
}

func (p *proc) ResetFailed(ctx context.Context, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if _, err := p.call(ctx, ProcRequest{Op: ProcOpResetFailed, Name: name}); err != nil {
		return fmt.Errorf("hostsvc: resetting %s: %w", name, err)
	}
	return nil
}

func (p *proc) List(ctx context.Context, prefix string) ([]UnitStatus, error) {
	if err := ValidatePrefix(prefix); err != nil {
		return nil, err
	}
	resp, err := p.call(ctx, ProcRequest{Op: ProcOpList, Prefix: prefix})
	if err != nil {
		return nil, fmt.Errorf("hostsvc: listing %s*: %w", prefix, err)
	}
	return resp.Statuses, nil
}

func (p *proc) Identify(ctx context.Context, name string, pid int) (Identity, error) {
	if err := ValidateName(name); err != nil {
		return Identity{}, err
	}
	resp, err := p.call(ctx, ProcRequest{Op: ProcOpIdentify, Name: name, PID: pid})
	if err != nil {
		return Identity{}, err
	}
	return *resp.Identity, nil
}

func (p *proc) Check(ctx context.Context, want Identity) (Observation, error) {
	if err := ValidateName(want.Unit); err != nil {
		return Observation{}, err
	}
	if obs, foreign := ForeignIdentity(p.Capabilities(), want); foreign {
		return obs, nil
	}
	resp, err := p.call(ctx, ProcRequest{Op: ProcOpCheck, Identity: &want})
	if errors.Is(err, ErrUnreachable) {
		return Observation{Verdict: VerdictUnknown, Detail: err.Error()}, nil
	}
	if err != nil {
		return Observation{Detail: err.Error()}, err
	}
	return *resp.Observation, nil
}

func (p *proc) Watch(ctx context.Context, prefix string) (<-chan UnitEvent, error) {
	if err := ValidatePrefix(prefix); err != nil {
		return nil, err
	}
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, DefaultCallTimeout)
	c, err := d.DialContext(dctx, "unix", p.socket)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("%w: dialing devsup at %s: %w", ErrUnreachable, p.socket, err)
	}
	if err := json.NewEncoder(c).Encode(ProcRequest{Op: ProcOpWatch, Prefix: prefix}); err != nil {
		c.Close()
		return nil, fmt.Errorf("%w: writing watch to devsup: %w", ErrUnreachable, err)
	}
	r := bufio.NewReader(c)
	line, err := r.ReadBytes('\n')
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("%w: reading the watch answer from devsup: %w", ErrUnreachable, err)
	}
	var first ProcResponse
	if err := json.Unmarshal(line, &first); err != nil || first.Error() != nil {
		c.Close()
		return nil, fmt.Errorf("hostsvc: devsup refused watch: %v %v", err, first.Error())
	}
	ch := make(chan UnitEvent, 64)
	stop := context.AfterFunc(ctx, func() { c.Close() })
	go func() {
		defer close(ch)
		defer stop()
		defer c.Close()
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var resp ProcResponse
			if json.Unmarshal(line, &resp) != nil || resp.Event == nil {
				continue
			}
			select {
			case ch <- *resp.Event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func (p *proc) Shutdown(ctx context.Context) error {
	_, err := p.call(ctx, ProcRequest{Op: ProcOpShutdown})
	return err
}

func (p *proc) Close() error { return nil }

func OpenProcClient(ctx context.Context, cfg Config) (ProcClient, error) {
	cfg.Backend = BackendProc
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	m, err := openProc(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return m.(*proc), nil
}
