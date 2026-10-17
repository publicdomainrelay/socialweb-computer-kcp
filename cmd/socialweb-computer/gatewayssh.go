package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/irohdial"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/gatewayssh"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/request"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tsbundle"
)

var gatewaySshOptions = slices.Concat([]string{backendOpt, "profile", "network", "name", "instance", "state-dir", "fg", "ready-timeout",
	"network-timeout", "kcp-path", "kine-path", "deno-path", "bao-path", "dumbpipe-path", "asset-cache"}, devOnly(tsbundle.SourceRootOption))

type gatewaySshRun struct {
	a              *app
	vals           clicfg.Values
	profile        string
	network        string
	options        []v1alpha1.ApplicationOption
	value          *v1alpha1.MarketNetworkValue
	bidderDids     []string
	ownerSessions  string
	networkTimeout time.Duration
}

type gatewaySshSummary struct {
	Profile    string                          `json:"profile"`
	Network    string                          `json:"network,omitempty"`
	Name       string                          `json:"name,omitempty"`
	Instance   string                          `json:"instance,omitempty"`
	KcpURL     string                          `json:"kcpUrl,omitempty"`
	Phase      v1alpha1.ApplicationPhase       `json:"phase,omitempty"`
	Message    string                          `json:"message,omitempty"`
	SSH        string                          `json:"ssh,omitempty"`
	Listeners  []v1alpha1.ApplicationListener  `json:"listeners,omitempty"`
	Components []v1alpha1.ApplicationComponent `json:"components,omitempty"`
	Seconds    float64                         `json:"seconds"`
}

func (r *gatewaySshRun) log(msg string, args ...any) {
	r.a.log.Info(msg, append([]any{"verb", "run gateway ssh", "profile", r.profile, "utc", time.Now().UTC().Format(time.RFC3339)}, args...)...)
}

func (r *gatewaySshRun) passthrough(args []string) error {
	parsed, err := appctl.ParseAppArgs(appctl.SshGatewayApp, args)
	if err != nil {
		return err
	}
	var nv v1alpha1.MarketNetworkValue
	nvSet := false
	for _, p := range parsed {
		if r.network != "" && strings.HasPrefix(p.To, appctl.MapToNetwork) {
			return fmt.Errorf("--%s maps to %s, which the market network sets for its child applications", p.Name, p.To)
		}
		switch {
		case p.To == appctl.MapToOptions:
			r.options = append(r.options, v1alpha1.ApplicationOption{Name: p.Name, Value: p.Value})
		case p.To == "spec.ownerSessionsFile":
			r.ownerSessions = p.Value
		case p.To == "spec.bidderDids":
			r.bidderDids = append(r.bidderDids, splitList(p.Value)...)
		case strings.HasPrefix(p.To, appctl.MapToNetwork):
			nvSet = true
			switch p.To {
			case "network.plcDirectoryUrl":
				nv.PlcDirectoryUrl = p.Value
			case "network.ingressProxyHost":
				nv.IngressProxyHost = p.Value
			case "network.relayUrls":
				nv.RelayUrls = splitList(p.Value)
			case "network.hosts":
				for _, h := range splitList(p.Value) {
					name, addr, ok := strings.Cut(h, "=")
					if !ok {
						return fmt.Errorf("--network-hosts %q: want name=address,...", p.Value)
					}
					nv.Hosts = append(nv.Hosts, v1alpha1.NetworkHost{Name: name, Address: addr})
				}
			}
		default:
			return fmt.Errorf("--%s maps to %s, which run gateway ssh does not take", p.Name, p.To)
		}
	}
	if nvSet {
		r.value = &nv
	}
	return nil
}

func runGatewaySsh(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	pos, pass := splitWords(rest)
	if len(pos) > 0 {
		return a.fail(exitRefused, "run gateway ssh: unexpected %q; hono-socialweb-computer-ssh options go after --", strings.Join(pos, " "))
	}
	r := &gatewaySshRun{a: a, vals: vals, profile: vals.String("profile"), network: vals.String("network")}
	var err error
	if r.networkTimeout, err = time.ParseDuration(vals.String("network-timeout")); err != nil {
		return a.fail(exitRefused, "run gateway ssh: network-timeout: %v", err)
	}
	if r.network != "" && (r.profile == "" || r.profile == appctl.SshGatewayProfileDefault) {
		r.profile = appctl.SshGatewayProfileNetwork
	}
	if r.profile == "" {
		r.profile = appctl.SshGatewayProfileDefault
	}
	if !slices.Contains(appctl.SshGatewayProfiles, r.profile) {
		return a.fail(exitRefused, "run gateway ssh: --profile %q: want one of %s", r.profile, strings.Join(appctl.SshGatewayProfiles, ", "))
	}
	if err := r.passthrough(pass); err != nil {
		return a.fail(exitRefused, "run gateway ssh: %v", err)
	}
	backend, err := hostsvc.ParseBackend(vals.String(backendOpt))
	if err != nil {
		return a.fail(exitRefused, "run gateway ssh: %v", err)
	}
	switch {
	case r.network != "" && backend != hostsvc.BackendProc:
		return a.fail(exitRefused, "run gateway ssh: --network: the market network places its children under its own devsup; pass --host-backend=proc")
	case r.network != "":
		return r.inNetwork(ctx)
	case r.profile == appctl.SshGatewayProfileNetwork:
		return a.fail(exitRefused, "run gateway ssh: --profile %s needs --network <market network instance> (dev up --profile market-network)", r.profile)
	}
	return r.standalone(ctx)
}

func (r *gatewaySshRun) name() (string, error) {
	name := r.vals.String("name")
	if name == "" {
		name = gatewayssh.DefaultName
	}
	return name, request.CheckName(name)
}

func (r *gatewaySshRun) report(sum gatewaySshSummary, st v1alpha1.ApplicationStatus) gatewaySshSummary {
	sum.Phase, sum.Message, sum.Listeners, sum.Components = st.Phase, st.Message, st.Listeners, st.Components
	sum.SSH = gatewayssh.SshListener(st.Listeners)
	return sum
}

func (r *gatewaySshRun) inNetwork(ctx context.Context) int {
	started := time.Now()
	l, err := r.a.layoutOf(r.network, r.vals)
	if err != nil {
		return r.a.fail(exitRefused, "run gateway ssh: %v", err)
	}
	var netRec devinstance.Record
	if err := devinstance.ReadJSON(l.Record, &netRec); err != nil {
		return r.a.fail(exitRefused, "run gateway ssh: market network %s has no instance record (dev up --profile market-network first): %v", r.network, err)
	}
	if netRec.Profile != bootstrap.ProfileMarketNetwork || netRec.AdminKubeconfig == "" {
		return r.a.fail(exitRefused, "run gateway ssh: instance %s is not a market network (profile %s)", r.network, netRec.Profile)
	}
	if netRec.Executables[appctl.DumbpipeExecutable].Path == "" {
		return r.a.fail(exitRefused, "run gateway ssh: market network %s was brought up without dumbpipe; its children reach guests over iroh only (dev up --dumbpipe-path in a dev build)", r.network)
	}
	name, err := r.name()
	if err != nil {
		return r.a.fail(exitRefused, "run gateway ssh: --name: %v", err)
	}
	app := v1alpha1.NetworkApp{Name: name, Kind: v1alpha1.ApplicationKindSshGateway, Profile: r.profile, Options: r.options,
		OwnerSessionsFile: r.ownerSessions}
	cur, found, err := request.NetworkApp(ctx, netRec.AdminKubeconfig, name)
	switch {
	case err != nil:
		return r.a.fail(exitFailed, "run gateway ssh: %v", err)
	case !found:
		if err := request.AddToNetwork(ctx, netRec.AdminKubeconfig, app); err != nil {
			return r.a.fail(exitFailed, "run gateway ssh: %v", err)
		}
		r.log("the market network places the ssh gateway in its workspace of the network's kcp", "network", r.network, "name", name)
	case cur.Message == appctl.StoppedMessage:
		if err := request.SetStopped(ctx, netRec.AdminKubeconfig, name, false); err != nil {
			return r.a.fail(exitFailed, "run gateway ssh: %v", err)
		}
		r.log("the market network starts the ssh gateway's kcp again from its kept state", "name", name)
	default:
		r.log("the market network already runs this ssh gateway; waiting for it to be Ready", "name", name, "phase", string(cur.Phase))
	}
	st, err := request.WaitNetworkApp(ctx, netRec.AdminKubeconfig, name, r.networkTimeout, func(a v1alpha1.NetworkAppStatus) bool {
		return a.Phase == v1alpha1.ApplicationPhaseReady && gatewayssh.SshListener(a.Listeners) != ""
	}, func(a v1alpha1.NetworkAppStatus) {
		r.log("network child", "name", name, "phase", string(a.Phase), "message", a.Message)
	})
	sum := r.report(gatewaySshSummary{Profile: r.profile, Network: r.network, Name: name, Instance: st.Instance, KcpURL: st.KcpUrl},
		v1alpha1.ApplicationStatus{Phase: st.Phase, Message: st.Message, Listeners: st.Listeners, Components: st.Components})
	sum.Seconds = time.Since(started).Seconds()
	r.a.printJSON(sum)
	if err != nil {
		return r.a.fail(exitFailed, "run gateway ssh: %v", err)
	}
	if !r.vals.Bool("fg") {
		return exitOK
	}
	r.log("run gateway ssh --fg: the ssh gateway lives as long as this process; Ctrl-C removes it from the market network", "name", name)
	<-ctx.Done()
	if err := request.RemoveFromNetwork(context.Background(), netRec.AdminKubeconfig, name); err != nil {
		return r.a.fail(exitFailed, "run gateway ssh: removing %s from the market network: %v", name, err)
	}
	return exitOK
}

func (r *gatewaySshRun) standalone(ctx context.Context) int {
	started := time.Now()
	name, err := r.name()
	if err != nil {
		return r.a.fail(exitRefused, "run gateway ssh: --name: %v", err)
	}
	spec, err := gatewayssh.Spec(gatewayssh.SpecInput{Profile: r.profile, Network: r.value, BidderDids: r.bidderDids, Options: r.options})
	if err != nil {
		return r.a.fail(exitRefused, "run gateway ssh: %v", err)
	}
	instance := r.vals.String("instance")
	if instance == "" {
		instance = gatewayssh.InstanceID(name)
	}
	vals := r.vals.With("instance", instance).With("profile", bootstrap.ProfileQuick)
	up, code := bringUp(ctx, r.a, vals, false)
	if code != exitOK {
		return code
	}
	if up.Executables[appctl.DumbpipeExecutable].Path == "" {
		return r.a.fail(exitRefused, "run gateway ssh: the instance has no dumbpipe; the ssh gateway reaches guests over iroh only (--dumbpipe-path in a dev build)")
	}
	k, err := gatewayssh.OpenKcp(up.AdminKubeconfig)
	if err != nil {
		return r.a.fail(exitFailed, "run gateway ssh: %v", err)
	}
	if err := k.ApplyRoot(ctx, spec); err != nil {
		return r.a.fail(exitFailed, "run gateway ssh: applying the SshGateway root object: %v", err)
	}
	r.log("the SshGateway root object is applied into its own kcp", "instance", up.Instance, "kcp", up.KcpURL)
	o, err := k.WaitReady(ctx, r.networkTimeout, func(st v1alpha1.ApplicationStatus) {
		r.log("ssh gateway", "phase", string(st.Phase), "message", st.Message)
	})
	sum := gatewaySshSummary{Profile: r.profile, Name: name, Instance: up.Instance, KcpURL: up.KcpURL}
	if o != nil {
		sum = r.report(sum, o.Status)
	}
	sum.Seconds = time.Since(started).Seconds()
	r.a.printJSON(sum)
	if err != nil {
		return r.a.fail(exitFailed, "run gateway ssh: %v", err)
	}
	if !r.vals.Bool("fg") {
		return exitOK
	}
	r.log("run gateway ssh --fg: the ssh gateway lives as long as this process; Ctrl-C stops its instance and keeps its state", "instance", up.Instance)
	<-ctx.Done()
	d := downInstance(context.Background(), r.a, up.layout, true)
	r.a.printJSON(d)
	if len(d.Remaining) > 0 {
		return exitFailed
	}
	return exitOK
}

func runIrohDial(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	if len(rest) > 0 {
		return a.fail(exitRefused, "internal iroh-dial: unexpected %q", strings.Join(rest, " "))
	}
	err := irohdial.Run(ctx, irohdial.Config{Socket: vals.String("socket"), Dumbpipe: vals.String("dumbpipe-path"), Log: a.log})
	if err != nil {
		return a.fail(exitFailed, "internal iroh-dial: %v", err)
	}
	return exitOK
}
