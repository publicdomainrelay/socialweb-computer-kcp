package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/assets"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devsup"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/gatewaykcp"
)

var serveGatewayOptions = []string{backendOpt, "state-dir", "unit-prefix", "kcp-path", "kine-path", "k3s-path", "postgres-root", "bao-path",
	"openbao-plugin-dir", "asset-cache", "ready-timeout", "kcp-front-address", "tls-front-address", "cert-front-address", "tls-cert-file", "tls-key-file",
	"gateway-spec-file", "gateway-profile", "bootstrap-cluster", "bootstrap-account-did"}

func (a *app) optionalExe(ctx context.Context, vals clicfg.Values, name, option string) (devinstance.Executable, error) {
	if !assets.Release() && vals.String(option) == "" {
		return devinstance.Executable{}, nil
	}
	return a.devExe(ctx, vals, name, option)
}

func (a *app) gatewayExecutables(ctx context.Context, vals clicfg.Values) (map[string]devinstance.Executable, error) {
	out := map[string]devinstance.Executable{}
	for _, x := range []struct{ name, option string }{{gatewaykcp.ExeKcp, "kcp-path"}, {gatewaykcp.ExeKine, "kine-path"},
		{gatewaykcp.ExeBao, "bao-path"}, {gatewaykcp.ExeK3s, "k3s-path"}} {
		e, err := a.optionalExe(ctx, vals, x.name, x.option)
		if err != nil {
			return nil, err
		}
		out[x.name] = e
	}
	plugins, err := a.openBaoPluginExes(vals)
	if err != nil {
		return nil, err
	}
	for name, e := range plugins {
		out[name] = e
	}
	if !assets.Release() && vals.String("postgres-root") == "" {
		out[gatewaykcp.ExePostgres] = devinstance.Executable{}
	} else {
		pg, err := a.postgresRoot(vals, "")
		if err != nil {
			return nil, err
		}
		out[gatewaykcp.ExePostgres] = pg
	}
	if assets.Release() {
		cache, err := a.assetCache(vals)
		if err != nil {
			return nil, err
		}
		for _, name := range []string{gatewaykcp.ExeExternalSecrets, gatewaykcp.ExeESOBundle, gatewaykcp.ExeDumbpipe, gatewaykcp.ExeWebDownloads} {
			e, err := assets.Resolve(assets.Options{Cache: cache, Log: a.log}, name)
			if err != nil {
				return nil, err
			}
			out[name] = devinstance.Executable{Path: e.Path, Version: e.Version}
		}
	}
	exe, err := selfExe()
	if err != nil {
		return nil, err
	}
	out[gatewaykcp.ExeCore] = devinstance.Executable{Path: exe}
	return out, nil
}

func runServeGatewayKcp(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	const verb = "serve gateway kcp"
	if len(rest) > 0 {
		return a.fail(exitRefused, "%s: unexpected arguments %q", verb, rest)
	}
	if err := a.spec.Require(vals, "state-dir"); err != nil {
		return a.fail(exitRefused, "%s: %v", verb, err)
	}
	state := vals.String("state-dir")
	if !filepath.IsAbs(state) {
		return a.fail(exitRefused, "%s: state-dir %q is not absolute", verb, state)
	}
	if envFile := filepath.Join(state, "gateway.env"); func() bool { _, err := os.Lstat(envFile); return err == nil }() {
		f, err := os.Open(envFile)
		if err != nil {
			return a.fail(exitRefused, "%s: %s exists but this account cannot read it (%v): systemd skipped it, so the gateway "+
				"would run on built-in defaults (audience did:web:socialweb.local) and refuse every real token; "+
				"chown it to the account and restart", verb, envFile, err)
		}
		f.Close()
	}
	backend, err := hostsvc.ParseBackend(vals.String(backendOpt))
	if err != nil {
		return a.fail(exitRefused, "%s: %v", verb, err)
	}
	prefix := vals.String("unit-prefix")
	if prefix != "" {
		if err := hostsvc.ValidatePrefix(prefix); err != nil {
			return a.fail(exitRefused, "%s: unit-prefix: %v", verb, err)
		}
	}
	readyTimeout, err := time.ParseDuration(vals.String("ready-timeout"))
	if err != nil {
		return a.fail(exitRefused, "%s: ready-timeout: %v", verb, err)
	}
	exes, err := a.gatewayExecutables(ctx, vals)
	if err != nil {
		return a.fail(exitRefused, "%s: %v", verb, err)
	}
	base, err := gatewaykcp.ReadSpec(vals.String("gateway-spec-file"))
	if err != nil {
		return a.fail(exitRefused, "%s: %v", verb, err)
	}
	environ := os.Environ()
	spec := gatewaykcp.Spec(base, vals.String("gateway-profile"), environ)
	unit, _ := a.env.LookupEnv(devsup.EnvUnit)
	cfg := gatewaykcp.Config{StateDir: state, Prefix: prefix, Backend: backend, Executables: exes,
		Spec: spec, Unit: unit, FrontAddress: vals.String("kcp-front-address"), TLSAddress: vals.String("tls-front-address"),
		CertFrontAddress: vals.String("cert-front-address"),
		TLSCert:          vals.String("tls-cert-file"), TLSKey: vals.String("tls-key-file"),
		BootstrapCluster: vals.String("bootstrap-cluster"), BootstrapAccountDID: vals.String("bootstrap-account-did"),
		ReadyTimeout: readyTimeout, Environ: environ, Exe: exes[gatewaykcp.ExeCore].Path, Stderr: a.stderr, Log: a.log}
	var m hostsvc.Manager
	switch backend {
	case hostsvc.BackendSystemdUser:
		m, err = hostsvc.Open(ctx, hostsvc.Config{Backend: backend, UnitPrefix: prefix})
	case hostsvc.BackendProc:
		m, _, err = startOrAdoptDevsup(ctx, a, gatewaykcp.NewLayout(state, prefix), cfg.Exe, 0, false)
	}
	if err != nil {
		return a.fail(exitFailed, "%s: the %s backend: %v", verb, backend, err)
	}
	defer m.Close()
	if err := gatewaykcp.Run(ctx, cfg, m); err != nil {
		if errors.Is(err, context.Canceled) {
			return exitOK
		}
		return a.fail(exitFailed, "%s: %v", verb, err)
	}
	return exitOK
}

var gatewayChildAssets = []string{gatewaykcp.ExeK3s, gatewaykcp.ExeExternalSecrets, gatewaykcp.ExeESOBundle, gatewaykcp.ExeWebDownloads}

func (a *app) gatewayChildExecutables(vals clicfg.Values) (map[string]devinstance.Executable, error) {
	if !assets.Release() {
		return nil, errors.New("--with-gateway runs the gateway's embedded k3s and external-secrets: build with scripts/build-socialweb-computer.sh (--dev for a dev build)")
	}
	cache, err := a.assetCache(vals)
	if err != nil {
		return nil, err
	}
	out := map[string]devinstance.Executable{}
	for _, name := range gatewayChildAssets {
		e, err := assets.Resolve(assets.Options{Cache: cache, Log: a.log}, name)
		if err != nil {
			return nil, err
		}
		out[name] = devinstance.Executable{Path: e.Path, Version: e.Version}
	}
	return out, nil
}

func (a *app) runGatewayChild(ctx context.Context, rec devinstance.Record, m hostsvc.Manager, timeout time.Duration) error {
	base, err := gatewaykcp.ReadSpec(rec.Gateway.SpecFile)
	if err != nil {
		return err
	}
	environ := os.Environ()
	exes := map[string]devinstance.Executable{}
	for k, v := range rec.Executables {
		exes[k] = v
	}
	unit, _ := a.env.LookupEnv(devsup.EnvUnit)
	cfg := gatewaykcp.Config{Backend: hostsvc.BackendProc, Executables: exes,
		Spec: gatewaykcp.Spec(base, base.Profile, environ), Unit: unit, TLSAddress: gatewaykcp.DefaultAddress,
		CertFrontAddress: gatewaykcp.DefaultAddress,
		BootstrapCluster: rec.Gateway.BootstrapCluster, BootstrapAccountDID: rec.Gateway.BootstrapAccountDID,
		ReadyTimeout: timeout, Environ: environ, Exe: exes[gatewaykcp.ExeCore].Path, Stderr: a.stderr, Log: a.log}
	return gatewaykcp.RunChild(ctx, cfg, rec.Layout, m)
}
