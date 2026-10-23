//go:build linux

package hostsvc

import (
	"context"
	"fmt"
	"maps"
	"time"

	sd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
)

type ProbeReading struct {
	Version  string
	Sent     map[string]any
	ReadBack map[string]any
	Result   string
}

func ManagerVersion(ctx context.Context, socket string) (string, error) {
	ctx, cancel := withDeadline(ctx, DefaultCallTimeout)
	defer cancel()
	b, err := dialPrivate(socket)(ctx)
	if err != nil {
		return "", err
	}
	defer b.Close()
	return b.Version(ctx)
}

func Probe(ctx context.Context, socket string, spec UnitSpec, extra map[string]any) (ProbeReading, error) {
	r := ProbeReading{Sent: map[string]any{}, ReadBack: map[string]any{}}
	if err := ValidateSpec(spec); err != nil {
		return r, err
	}
	ctx, cancel := withDeadline(ctx, DefaultCallTimeout)
	defer cancel()
	b, err := dialPrivate(socket)(ctx)
	if err != nil {
		return r, err
	}
	defer b.Close()
	if r.Version, err = b.Version(ctx); err != nil {
		return r, fmt.Errorf("hostsvc: reading the manager version: %w", err)
	}
	n, err := strs.LeadingInt(r.Version)
	if err != nil || n < MinSystemdVersion {
		return r, fmt.Errorf("%w: the manager reports %q", ErrSystemdTooOld, r.Version)
	}
	props := unitProperties(spec)
	for k, v := range extra {
		props = append(props, sd.Property{Name: k, Value: dbus.MakeVariant(v)})
	}
	for _, p := range props {
		r.Sent[p.Name] = p.Value.Value()
	}
	ch := make(chan string, 1)
	if err := b.StartTransientUnit(ctx, spec.Name, props, ch); err != nil {
		return r, fmt.Errorf("hostsvc: starting the probe %s: %w", spec.Name, err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer stopCancel()
		done := make(chan string, 1)
		if b.StopUnit(stopCtx, spec.Name, done) == nil {
			_, _ = waitJob(stopCtx, done)
		}
		_ = b.ResetFailedUnit(stopCtx, spec.Name)
	}()
	if r.Result, err = waitJob(ctx, ch); err != nil {
		return r, err
	}
	up, err := b.UnitProperties(ctx, spec.Name)
	if err != nil {
		return r, fmt.Errorf("hostsvc: reading the probe's unit properties: %w", err)
	}
	sp, err := b.ServiceProperties(ctx, spec.Name)
	if err != nil {
		return r, fmt.Errorf("hostsvc: reading the probe's service properties: %w", err)
	}
	maps.Copy(r.ReadBack, up)
	maps.Copy(r.ReadBack, sp)
	return r, nil
}
