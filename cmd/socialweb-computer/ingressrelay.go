package main

import (
	"context"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/ingressrelaymain"
)

var ingressRelayOptions = []string{
	"hostname", "unix-socket", "listen", "tls-cert-file", "tls-key-file",
	"ingress-relay-service-id", "ingress-relay-timeout", "ingress-relay-keepalive", "ingress-relay-keepalive-timeout",
	"ingress-relay-nonce-ttl", "ingress-relay-additional-hosts", "ingress-relay-allowed-dids", "ingress-relay-plc-url",
	"ingress-relay-max-body-mib", "ingress-relay-expire-sessions",
}

func runIngressRelay(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	const verb = "internal ingress-relay"
	if len(rest) > 0 {
		return a.fail(exitRefused, "%s: unexpected %q", verb, strings.Join(rest, " "))
	}
	durations := map[string]time.Duration{}
	for _, name := range []string{"ingress-relay-timeout", "ingress-relay-keepalive", "ingress-relay-keepalive-timeout", "ingress-relay-nonce-ttl"} {
		d, err := time.ParseDuration(vals.String(name))
		if err != nil || d <= 0 {
			return a.fail(exitRefused, "%s: --%s wants a positive Go duration, got %q", verb, name, vals.String(name))
		}
		durations[name] = d
	}
	v := buildVersion(a.goos, a.goarch)
	version := v.Version
	if v.Revision != "" && !strings.Contains(version, v.Revision) {
		version += " " + v.Revision
	}
	err := ingressrelaymain.Run(ctx, ingressrelaymain.Config{
		Hostname:          vals.String("hostname"),
		ServiceID:         vals.String("ingress-relay-service-id"),
		Listen:            vals.String("listen"),
		UnixSocket:        vals.String("unix-socket"),
		TLSCertFile:       vals.String("tls-cert-file"),
		TLSKeyFile:        vals.String("tls-key-file"),
		PLCURL:            vals.String("ingress-relay-plc-url"),
		AdditionalHosts:   ingressrelaymain.SplitList(vals.String("ingress-relay-additional-hosts")),
		AllowedDIDs:       ingressrelaymain.SplitList(vals.String("ingress-relay-allowed-dids")),
		RelayTimeout:      durations["ingress-relay-timeout"],
		KeepaliveInterval: durations["ingress-relay-keepalive"],
		KeepaliveTimeout:  durations["ingress-relay-keepalive-timeout"],
		NonceTTL:          durations["ingress-relay-nonce-ttl"],
		MaxBodyBytes:      int64(vals.Int("ingress-relay-max-body-mib")) << 20,
		ExpireSessions:    vals.Bool("ingress-relay-expire-sessions"),
		Version:           version,
		Log:               a.log.With("component", "ingress-relay"),
	})
	if err != nil {
		return a.fail(exitFailed, "%s: %v", verb, err)
	}
	return exitOK
}
