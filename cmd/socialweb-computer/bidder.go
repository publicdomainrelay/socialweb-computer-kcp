package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appcli"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/buildstamp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devsup"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestimage"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestimagerelease"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/progress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/bidder"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/denoruntime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/request"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tsbundle"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vmruntime"
)

const bidderIdentityWait = 5 * time.Minute

var bidderOptions = slices.Concat([]string{backendOpt, "profile", "network", "name", "instance", "state-dir", "fg", "ready-timeout",
	"network-timeout", "market-network-file", "bidder-policy", "bidder-policy-args", "kcp-path", "kine-path", "deno-path", "bao-path", "openbao-plugin-dir", "asset-cache", "unit-prefix", "kcp-front-address", "guest-image-release-base"},
	devVMOptions, devOnly(tsbundle.SourceRootOption))

type bidderRun struct {
	a              *app
	vals           clicfg.Values
	backend        hostsvc.Backend
	profile        string
	handle         string
	network        string
	policy         string
	options        []v1alpha1.ApplicationOption
	value          *v1alpha1.MarketNetworkValue
	pdsURL         string
	readyTimeout   time.Duration
	networkTimeout time.Duration
}

type bidderSummary struct {
	Profile   string                          `json:"profile"`
	Handle    string                          `json:"handle,omitempty"`
	Network   string                          `json:"network,omitempty"`
	Name      string                          `json:"name,omitempty"`
	Instance  string                          `json:"instance,omitempty"`
	KcpURL    string                          `json:"kcpUrl,omitempty"`
	Did       string                          `json:"did,omitempty"`
	Phase     v1alpha1.ApplicationPhase       `json:"phase,omitempty"`
	Message   string                          `json:"message,omitempty"`
	Listeners []v1alpha1.ApplicationListener  `json:"listeners,omitempty"`
	Comps     []v1alpha1.ApplicationComponent `json:"components,omitempty"`
	Seconds   float64                         `json:"seconds"`
	Identity  *v1alpha1.MarketIdentityStatus  `json:"identity,omitempty"`
}

func (r *bidderRun) log(msg string, args ...any) {
	r.a.log.Info(msg, append([]any{"verb", "run bidder", "profile", r.profile, "utc", time.Now().UTC().Format(time.RFC3339)}, args...)...)
}

func (r *bidderRun) passthrough(args []string) error {
	parsed, err := appctl.ParseAppArgs(appctl.BidderApp, args)
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
		case p.To == "spec.policy":
			r.policy = p.Value
		case p.To == "spec.identity.handle":
			if r.handle != "" && r.handle != p.Value {
				return fmt.Errorf("--atproto-handle %s and --handle %s name two accounts", p.Value, r.handle)
			}
			r.handle = p.Value
		case strings.HasPrefix(p.To, "spec.firecracker"):
			return appctl.ErrFirecrackerInPod
		case strings.HasPrefix(p.To, appctl.MapToNetwork):
			nvSet = true
			switch p.To {
			case "network.plcDirectoryUrl":
				nv.PlcDirectoryUrl = p.Value
			case "network.ingressProxyHost":
				nv.IngressProxyHost = p.Value
			case "network.relayUrls":
				nv.RelayUrls = splitList(p.Value)
			case "network.firehoseUrls":
				nv.FirehoseUrls = splitList(p.Value)
			case "network.qrOrigin":
				nv.QrOrigin = p.Value
			case "network.guestTlsUrl":
				nv.GuestTlsUrl = p.Value
			case "network.caBundleRef":
				nv.CaBundleRef = p.Value
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
			return fmt.Errorf("--%s maps to %s, which run bidder does not take", p.Name, p.To)
		}
	}
	if nvSet {
		r.value = &nv
	}
	return nil
}

func runBidder(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	pos, pass := splitWords(rest)
	if len(pos) > 0 {
		return a.fail(exitRefused, "run bidder: unexpected %q; hono-bidder options go after --", strings.Join(pos, " "))
	}
	backend, err := hostsvc.ParseBackend(vals.String(backendOpt))
	if err != nil {
		return a.fail(exitRefused, "run bidder: %v", err)
	}
	r := &bidderRun{a: a, vals: vals, backend: backend, profile: vals.String("profile"), handle: strings.ToLower(vals.String("handle")),
		network: vals.String("network")}
	if !slices.Contains(appctl.BidderProfiles, r.profile) {
		return a.fail(exitRefused, "run bidder: --profile %q: want one of %s", r.profile, strings.Join(appctl.BidderProfiles, ", "))
	}
	if r.readyTimeout, err = time.ParseDuration(vals.String("ready-timeout")); err != nil {
		return a.fail(exitRefused, "run bidder: ready-timeout: %v", err)
	}
	if r.readyTimeout < 0 {
		return a.fail(exitRefused, "run bidder: ready-timeout: %s is negative", r.readyTimeout)
	}
	if r.networkTimeout, err = time.ParseDuration(vals.String("network-timeout")); err != nil {
		return a.fail(exitRefused, "run bidder: network-timeout: %v", err)
	}
	if err := r.passthrough(pass); err != nil {
		return a.fail(exitRefused, "run bidder: %v", err)
	}
	if r.policy == "" {
		r.policy = vals.String("bidder-policy")
	}
	if args := vals.String("bidder-policy-args"); args != "" && !slices.ContainsFunc(r.options, func(o v1alpha1.ApplicationOption) bool { return o.Name == "policy-args" }) {
		r.options = append(r.options, v1alpha1.ApplicationOption{Name: "policy-args", Value: args})
	}
	switch {
	case r.network != "" && r.profile != appctl.BidderProfileNetwork:
		return a.fail(exitRefused, "run bidder: --network places the bidder in a market network, whose profile is %s, not %s", appctl.BidderProfileNetwork, r.profile)
	case r.network == "" && r.profile == appctl.BidderProfileNetwork && r.vals.String("market-network-file") == "":
		return a.fail(exitRefused, "run bidder: --profile %s needs --network <market network instance> (dev up --profile market-network), or --market-network-file for an installed bidder account", r.profile)
	case r.network != "" && r.vals.String("market-network-file") != "":
		return a.fail(exitRefused, "run bidder: --network places the bidder in the market network, which hands it the network value; --market-network-file is for a bidder outside it")
	case r.network != "" && backend != hostsvc.BackendProc:
		return a.fail(exitRefused, "run bidder: --network: the market network places its children under its own devsup; pass --host-backend=proc")
	case r.network == "" && r.handle == "":
		return a.fail(exitRefused, "run bidder: --handle is required: it names the bidder's account and owns its ComputeVMs")
	}
	if r.vals.String("guest-image-source") == "" {
		if backend == hostsvc.BackendSystemdUser && cachedGuestImage(r.vals.String("state-dir")) {
			r.a.log.Info("the pinned guest image is already in the store, so no source is asked of the published artifacts")
		} else {
			src, err := publishedGuestImageSource(ctx, r.vals.String("guest-image-release-base"))
			if err != nil {
				return a.fail(exitRefused, "run bidder: %v", err)
			}
			r.vals = r.vals.With("guest-image-source", src)
		}
	}
	if r.handle != "" {
		if err := bidder.CheckHandle(r.handle); err != nil {
			return a.fail(exitRefused, "run bidder: %v", err)
		}
	}
	if p := r.vals.String("market-network-file"); p != "" {
		if err := r.readMarketNetwork(p); err != nil {
			return a.fail(exitRefused, "run bidder: --market-network-file: %v", err)
		}
	}
	switch {
	case r.network != "":
		return r.inNetwork(ctx)
	case backend == hostsvc.BackendSystemdUser:
		return r.installedCore(ctx)
	}
	return r.standalone(ctx)
}

func (r *bidderRun) readMarketNetwork(path string) error {
	if r.profile != appctl.BidderProfileNetwork {
		return fmt.Errorf("it names a market network, whose profile is %s, not %s", appctl.BidderProfileNetwork, r.profile)
	}
	if r.value != nil {
		return errors.New("it and the --network-* options both name the network value")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var st v1alpha1.MarketNetworkStatus
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("%s is not a MarketNetwork status: %w", path, err)
	}
	if st.Network == nil || st.BidderPdsUrl == "" {
		return fmt.Errorf("%s names no network value or bidderPdsUrl (dev status --instance <n> | jq .network.status, once the network is Ready)", path)
	}
	if _, err := os.Stat(st.Network.CaBundleRef); err != nil {
		return fmt.Errorf("the network CA bundle: %w", err)
	}
	r.value, r.pdsURL = st.Network, st.BidderPdsUrl
	return nil
}

func cachedGuestImage(stateDir string) bool {
	if stateDir == "" {
		return false
	}
	pin, err := guestimage.PinFor(vmruntime.ImageTarget())
	if err != nil || pin.Valid() != nil {
		return false
	}
	dir := guestimage.Store{Root: filepath.Join(stateDir, "images")}.Dir(pin)
	for _, name := range []string{guestimage.ManifestName, guestimage.KernelName, guestimage.RootfsName} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !st.Mode().IsRegular() || st.Size() != pin.Files[name].Bytes {
			return false
		}
	}
	return true
}

func publishedGuestImageSource(ctx context.Context, base string) (string, error) {
	version, commit := buildstamp.Identity()
	target := vmruntime.ImageTarget()
	pin, err := guestimage.PinFor(target)
	if err != nil {
		return "", err
	}
	src, err := guestimagerelease.Source(ctx, guestimagerelease.Options{
		Base:           base,
		Target:         target,
		Version:        version,
		Commit:         commit,
		ManifestSHA256: pin.Files[guestimage.ManifestName].SHA256,
	})
	if err != nil {
		return "", fmt.Errorf("%w (pass --guest-image-source file:///<dir>, or point --guest-image-release-base at the published artifacts)", err)
	}
	return src, nil
}

func (r *bidderRun) kcpProvider() *v1alpha1.KcpProvider {
	return &v1alpha1.KcpProvider{Handle: r.handle, GuestImageSource: r.vals.String("guest-image-source"),
		VmHostBackend: r.vals.String("vm-host-backend")}
}

func (r *bidderRun) spec() (v1alpha1.MarketBidderSpec, error) {
	return bidder.Spec(bidder.SpecInput{Profile: r.profile, Handle: r.handle, Policy: r.policy, Network: r.value, PdsUrl: r.pdsURL, Kcp: r.kcpProvider(),
		Options: r.options})
}

func (r *bidderRun) marketNetwork() (devinstance.Layout, devinstance.Record, error) {
	l, err := r.a.layoutOf(r.network, r.vals)
	if err != nil {
		return l, devinstance.Record{}, err
	}
	var rec devinstance.Record
	if err := devinstance.ReadJSON(l.Record, &rec); err != nil {
		return l, rec, fmt.Errorf("market network %s has no instance record (dev up --profile market-network first): %w", r.network, err)
	}
	if rec.Profile != bootstrap.ProfileMarketNetwork || rec.AdminKubeconfig == "" {
		return l, rec, fmt.Errorf("instance %s is not a market network (profile %s)", r.network, rec.Profile)
	}
	return l, rec, nil
}

func (r *bidderRun) inNetwork(ctx context.Context) int {
	started := time.Now()
	_, netRec, err := r.marketNetwork()
	if err != nil {
		return r.a.fail(exitRefused, "run bidder: %v", err)
	}
	name := r.vals.String("name")
	if name == "" && r.handle != "" {
		name = bidder.AppName(r.handle)
	}
	if name == "" {
		name = "bidder-" + strings.ToLower(time.Now().UTC().Format("150405"))
	}
	if err := request.CheckName(name); err != nil {
		return r.a.fail(exitRefused, "run bidder: --name: %v", err)
	}
	app, err := appctl.NetworkBidderApp(name, nil, appctl.BidderVMDefaults{GuestImageSource: r.vals.String("guest-image-source"),
		VmHostBackend: r.vals.String("vm-host-backend"), Handle: r.handle})
	if err != nil {
		return r.a.fail(exitRefused, "run bidder: %v", err)
	}
	app.Profile, app.Options = r.profile, r.options
	if r.policy != "" {
		app.Policy = r.policy
	}
	cur, found, err := request.NetworkApp(ctx, netRec.AdminKubeconfig, name)
	switch {
	case err != nil:
		return r.a.fail(exitFailed, "run bidder: %v", err)
	case !found:
		if err := request.AddToNetwork(ctx, netRec.AdminKubeconfig, app); err != nil {
			return r.a.fail(exitFailed, "run bidder: %v", err)
		}
		r.log("the market network places the bidder in its workspace of the network's kcp", "network", r.network, "name", name, "computeVMHandle", appctl.KcpHandle(app))
	case cur.Message == appctl.StoppedMessage:
		if err := request.SetStopped(ctx, netRec.AdminKubeconfig, name, false); err != nil {
			return r.a.fail(exitFailed, "run bidder: %v", err)
		}
		r.log("the market network starts the bidder's kcp again from its kept state", "name", name)
	default:
		r.log("the market network already runs this bidder; waiting for it to be Ready", "name", name, "phase", string(cur.Phase))
	}
	st, err := request.WaitNetworkApp(ctx, netRec.AdminKubeconfig, name, r.networkTimeout, func(a v1alpha1.NetworkAppStatus) bool {
		return a.Phase == v1alpha1.ApplicationPhaseReady && a.Did != ""
	}, func(a v1alpha1.NetworkAppStatus) {
		r.log("network child", "name", name, "phase", string(a.Phase), "message", a.Message)
	})
	sum := bidderSummary{Profile: r.profile, Handle: name + "." + appctl.BidderAccountDomain, Network: r.network, Name: name, Instance: st.Instance,
		KcpURL: st.KcpUrl, Did: st.Did, Phase: st.Phase, Message: st.Message, Listeners: st.Listeners, Seconds: time.Since(started).Seconds()}
	r.a.printJSON(sum)
	if err != nil {
		return r.a.fail(exitFailed, "run bidder: %v", err)
	}
	if !r.vals.Bool("fg") {
		return exitOK
	}
	r.log("run bidder --fg: the bidder lives as long as this process; Ctrl-C removes it from the market network", "name", name)
	<-ctx.Done()
	if err := request.RemoveFromNetwork(context.Background(), netRec.AdminKubeconfig, name); err != nil {
		return r.a.fail(exitFailed, "run bidder: removing %s from the market network: %v", name, err)
	}
	return exitOK
}

func (r *bidderRun) standalone(ctx context.Context) int {
	started := time.Now()
	spec, err := r.spec()
	if err != nil {
		return r.a.fail(exitRefused, "run bidder: %v", err)
	}
	instance := r.vals.String("instance")
	if instance == "" {
		instance = bidder.InstanceID(r.handle)
	}
	vals := r.vals.With("instance", instance).With("profile", bootstrap.ProfileQuick).With("handle", r.handle)
	up, code := bringUp(ctx, r.a, vals, false)
	if code != exitOK {
		return code
	}
	k, err := bidder.OpenKcp(up.AdminKubeconfig)
	if err != nil {
		return r.a.fail(exitFailed, "run bidder: %v", err)
	}
	if err := k.ApplyRoot(ctx, spec); err != nil {
		return r.a.fail(exitFailed, "run bidder: applying the MarketBidder root object: %v", err)
	}
	r.log("the MarketBidder root object is applied into the handle's own kcp", "instance", up.Instance, "kcp", up.KcpURL)
	sum := bidderSummary{Profile: r.profile, Handle: r.handle, Instance: up.Instance, KcpURL: up.KcpURL}
	id, err := r.signIn(ctx, k, spec)
	if id != nil {
		s := id.Status
		sum.Identity = &s
	}
	if err != nil {
		r.a.printJSON(sum)
		return r.a.fail(exitFailed, "run bidder: %v", err)
	}
	o, err := k.WaitReady(ctx, r.networkTimeout, func(st v1alpha1.ApplicationStatus) {
		r.log("bidder", "phase", string(st.Phase), "message", st.Message)
	})
	if o != nil {
		sum.Did, sum.Phase, sum.Message, sum.Listeners, sum.Comps = o.Status.Did, o.Status.Phase, o.Status.Message, o.Status.Listeners, o.Status.Components
	}
	sum.Seconds = time.Since(started).Seconds()
	r.a.printJSON(sum)
	if err != nil {
		return r.a.fail(exitFailed, "run bidder: %v", err)
	}
	if !r.vals.Bool("fg") {
		return exitOK
	}
	r.log("run bidder --fg: the bidder lives as long as this process; Ctrl-C stops its instance and keeps its state", "instance", up.Instance)
	<-ctx.Done()
	d := downInstance(context.Background(), r.a, up.layout, true)
	r.a.printJSON(d)
	if len(d.Remaining) > 0 {
		return exitFailed
	}
	return exitOK
}

func (r *bidderRun) signIn(ctx context.Context, k *bidder.Kcp, spec v1alpha1.MarketBidderSpec) (*v1alpha1.MarketIdentity, error) {
	if err := k.WaitIdentityObject(ctx, r.networkTimeout); err != nil {
		return nil, err
	}
	ids, err := k.Identities()
	if err != nil {
		return nil, err
	}
	var in io.Reader
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		in = os.Stdin
	}
	wctx, cancel := context.WithTimeout(ctx, bidderIdentityWait)
	defer cancel()
	id, err := appcli.IdentityRenderer{Client: ids, Out: r.a.stderr, In: in}.WaitReady(wctx, appctl.BidderIdentity)
	if err != nil {
		return id, err
	}
	if err := bidder.CheckSignIn(spec, id); err != nil {
		return id, fmt.Errorf("the sign-in is refused; the core removes the session and shows a new QR: %w", err)
	}
	return id, nil
}

func (r *bidderRun) installedCore(ctx context.Context) int {
	const verb = "run bidder"
	state := r.vals.String("state-dir")
	if !filepath.IsAbs(state) {
		return r.a.fail(exitRefused, "%s: host-backend=systemd-user: state-dir %q is not absolute (install bidder writes it into config.json)", verb, state)
	}
	if err := bidder.CheckAccount(r.backend, r.handle, "/"); err != nil {
		return r.a.fail(exitRefused, "%s: %v", verb, err)
	}
	spec, err := r.spec()
	if err != nil {
		return r.a.fail(exitRefused, "%s: %v", verb, err)
	}
	prefix := r.vals.String("unit-prefix")
	exes := map[string]devinstance.Executable{}
	for _, x := range []struct{ name, option string }{{bidder.ExeKcp, "kcp-path"}, {bidder.ExeKine, "kine-path"}, {bidder.ExeBao, "bao-path"}} {
		e, err := r.a.optionalExe(ctx, r.vals, x.name, x.option)
		if err != nil {
			return r.a.fail(exitRefused, "%s: %v", verb, err)
		}
		exes[x.name] = e
	}
	deno, err := r.a.denoExe(ctx, r.vals)
	if err != nil {
		return r.a.fail(exitRefused, "%s: %v", verb, err)
	}
	exes[bidder.ExeDeno] = deno
	plugins, err := r.a.openBaoPluginExes(r.vals)
	if err != nil {
		return r.a.fail(exitRefused, "%s: %v", verb, err)
	}
	for name, e := range plugins {
		exes[name] = e
	}
	exe, err := selfExe()
	if err != nil {
		return r.a.fail(exitFailed, "%s: %v", verb, err)
	}
	exes[bidder.ExeCore] = devinstance.Executable{Path: exe}
	src, err := tsbundle.Select(r.vals)
	if err != nil {
		return r.a.fail(exitRefused, "%s: the TypeScript closure: %v", verb, err)
	}
	closure, err := src.Prepare(state)
	if err != nil {
		return r.a.fail(exitFailed, "%s: %v", verb, err)
	}
	grace, err := vmStopGrace(r.vals)
	if err != nil {
		return r.a.fail(exitRefused, "%s: %v", verb, err)
	}
	runtimeDir := r.vals.String("vm-runtime-dir")
	if !filepath.IsAbs(runtimeDir) {
		return r.a.fail(exitRefused, "%s: vm-runtime-dir %q must be absolute (the account's XDG_RUNTIME_DIR)", verb, runtimeDir)
	}
	vm := devinstance.VMRuntime{Handle: r.handle, Backend: hostsvc.BackendSystemdUser, ImageSource: r.vals.String("guest-image-source"),
		StopGrace: grace.String(), RuntimeDir: runtimeDir, Executables: map[string]string{}}
	var vexe vmruntime.Executables
	for _, x := range []struct {
		dst          *string
		name, option string
	}{{&vexe.Stage, "swc-userns-stage", "userns-stage-path"}, {&vexe.Pasta, "pasta", "pasta-path"},
		{&vexe.VMExec, "socialweb-vmexec", "vmexec-path"}, {&vexe.Firecracker, "firecracker", "firecracker-path"}} {
		p, err := r.a.vmExe(r.vals, x.name, x.option)
		if err != nil {
			return r.a.fail(exitRefused, "%s: %v", verb, err)
		}
		*x.dst, vm.Executables[x.name] = p, p
	}
	m, err := hostsvc.Open(ctx, hostsvc.Config{Backend: hostsvc.BackendSystemdUser, UnitPrefix: prefix})
	if err != nil {
		return r.a.fail(exitFailed, "%s: the systemd-user backend: %v", verb, err)
	}
	defer m.Close()
	rt, err := vmruntime.ForHost(vmruntime.Config{Manager: m, StateDir: state, RuntimeDir: runtimeDir, Exec: vexe,
		ImageSource: vm.ImageSource, StopGrace: grace, Binaries: installedBinaries(state),
		ImageOut: r.a.stderr, ImageTTY: progress.IsTTYWriter(r.a.stderr)})
	if err != nil {
		return r.a.fail(exitFailed, "%s: the VM runtime: %v", verb, err)
	}
	unit, _ := r.a.env.LookupEnv(devsup.EnvUnit)
	os.Setenv(denoruntime.WatchListEnv, "false")
	cfg := bidder.CoreConfig{StateDir: state, Prefix: prefix, Handle: r.handle, Executables: exes, Closure: closure, Spec: spec, VM: vm,
		ComputeVM: computevm.Config{Handle: r.handle, Runtime: rt, Manager: m, Log: r.a.log}, FrontAddress: r.vals.String("kcp-front-address"),
		ReadyTimeout: r.readyTimeout, Unit: unit, Log: r.a.log}
	if err := bidder.RunCore(ctx, cfg, m); err != nil {
		if errors.Is(err, context.Canceled) {
			return exitOK
		}
		return r.a.fail(exitFailed, "%s: %v", verb, err)
	}
	return exitOK
}
