package main

import (
	"context"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atprotorelaymain"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
)

var atprotoRelayOptions = []string{
	"state-dir", "atproto-relay-listen", "atproto-relay-public-url", "atproto-relay-plc-url", "atproto-relay-allowed-hosts",
	"atproto-relay-seed-hosts", "atproto-relay-recrawl-window", "atproto-relay-idle-after", "atproto-relay-max-backlog",
	"atproto-relay-start-seq", "atproto-relay-insecure-http",
}

func runAtprotoRelay(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	const verb = "internal atproto-relay"
	if len(rest) > 0 {
		return a.fail(exitRefused, "%s: unexpected %q", verb, strings.Join(rest, " "))
	}
	durations := map[string]time.Duration{}
	for _, name := range []string{"atproto-relay-recrawl-window", "atproto-relay-idle-after"} {
		d, err := time.ParseDuration(vals.String(name))
		if err != nil || d < 0 {
			return a.fail(exitRefused, "%s: --%s wants a Go duration of 0 or more, got %q", verb, name, vals.String(name))
		}
		durations[name] = d
	}
	if vals.Int("atproto-relay-start-seq") < 0 || vals.Int("atproto-relay-max-backlog") < 0 {
		return a.fail(exitRefused, "%s: --atproto-relay-start-seq and --atproto-relay-max-backlog want 0 or more", verb)
	}
	v := buildVersion(a.goos, a.goarch)
	version := v.Version
	if v.Revision != "" && !strings.Contains(version, v.Revision) {
		version += " " + v.Revision
	}
	err := atprotorelaymain.Run(ctx, atprotorelaymain.Config{
		Listen:        vals.String("atproto-relay-listen"),
		StateDir:      vals.String("state-dir"),
		PublicURL:     vals.String("atproto-relay-public-url"),
		PLCURL:        vals.String("atproto-relay-plc-url"),
		AllowedHosts:  atprotorelaymain.SplitList(vals.String("atproto-relay-allowed-hosts")),
		SeedHostsFile: vals.String("atproto-relay-seed-hosts"),
		RecrawlWindow: durations["atproto-relay-recrawl-window"],
		IdleAfter:     durations["atproto-relay-idle-after"],
		MaxBacklog:    vals.Int("atproto-relay-max-backlog"),
		StartSeq:      int64(vals.Int("atproto-relay-start-seq")),
		InsecureHTTP:  vals.Bool("atproto-relay-insecure-http"),
		Version:       version,
		Log:           a.log.With("component", "atproto-relay"),
	})
	if err != nil {
		return a.fail(exitFailed, "%s: %v", verb, err)
	}
	return exitOK
}
