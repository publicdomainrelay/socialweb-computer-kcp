package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketnetwork"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketrequester"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appcli"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/logx"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/request"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/webrtcproxy"
)

const (
	requestSSHReady     = time.Minute
	requestIdentityWait = 5 * time.Minute
	requestSummaryRead  = 10 * time.Second
	requestAttachProbe  = 10 * time.Second
	requestSSHRetryRun  = 20 * time.Second
	transportIroh       = "iroh"
	transportWebRTC     = "webrtc"
	sshPort             = 22
	sshConnectionFailed = 255
)

var requestOptions = []string{"network", "name", "secrets", "tunnel", "hold", "exec", "skip-ssh", "contract-timeout", "state-dir",
	"ready-timeout", "network-timeout", "kcp-path", "kine-path", "bao-path", "dumbpipe-path", "asset-cache", "transport", "tag"}

type requestRun struct {
	a               *app
	vals            clicfg.Values
	stateRoot       string
	inv             request.Invocation
	hold            bool
	skipSSH         bool
	exec            string
	secrets         string
	transport       string
	tag             string
	target          string
	root            v1alpha1.MarketRequesterSpec
	contract        v1alpha1.ComputeContractSpec
	appOptions      []v1alpha1.ApplicationOption
	contractTimeout time.Duration
	networkTimeout  time.Duration
	summary         request.Summary
	kcp             *request.Kcp
	standalone      devinstance.Layout
	instanceDir     string
}

func splitWords(rest []string) ([]string, []string) {
	i := 0
	for i < len(rest) && !strings.HasPrefix(rest[i], "--") {
		i++
	}
	pass := rest[i:]
	if len(pass) > 0 && pass[0] == "--" {
		pass = pass[1:]
	}
	return rest[:i], pass
}

func (r *requestRun) log(msg string, args ...any) {
	r.a.log.Info(msg, append([]any{"request", r.inv.Name, "utc", time.Now().UTC().Format(time.RFC3339)}, args...)...)
}

func newRequestRun(a *app, vals clicfg.Values) (*requestRun, error) {
	root, err := devinstance.StateRoot(vals.String("state-dir"), a.env)
	if err != nil {
		return nil, err
	}
	r := &requestRun{a: a, vals: vals, stateRoot: root, hold: vals.Bool("hold"), skipSSH: vals.Bool("skip-ssh"),
		exec: vals.String("exec"), secrets: vals.String("secrets"), tag: strings.TrimSpace(vals.String("tag")),
		target: strings.TrimSpace(vals.String("contract"))}
	switch r.transport = strings.ToLower(strings.TrimSpace(vals.String("transport"))); r.transport {
	case "", transportIroh:
		r.transport = transportIroh
	case transportWebRTC:
		if r.tag == "" {
			return nil, errors.New("transport webrtc: --tag names the webrtc tunnel")
		}
	default:
		return nil, fmt.Errorf("transport %q: want iroh or webrtc", r.transport)
	}
	if r.contract.Tunnels, err = parseTunnels(vals.String("tunnel")); err != nil {
		return nil, fmt.Errorf("tunnel: %w", err)
	}
	if r.contractTimeout, err = time.ParseDuration(vals.String("contract-timeout")); err != nil {
		return nil, fmt.Errorf("contract-timeout: %w", err)
	}
	if r.networkTimeout, err = time.ParseDuration(vals.String("network-timeout")); err != nil {
		return nil, fmt.Errorf("network-timeout: %w", err)
	}
	return r, nil
}

func splitList(v string) []string {
	var out []string
	for s := range strings.SplitSeq(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (r *requestRun) passthrough(args []string, network bool) error {
	parsed, err := compose.ParseAppArgs(marketrequester.RequesterApp, args)
	if err != nil {
		return err
	}
	r.root = v1alpha1.MarketRequesterSpec{Profile: marketrequester.RequesterProfileDefault,
		Identity: v1alpha1.ApplicationIdentity{Mode: v1alpha1.MarketIdentityModeLocalRepo}}
	var nv v1alpha1.MarketNetworkValue
	nvSet := false
	for _, p := range parsed {
		perApp := p.To == "spec.secretsFallback" || p.To == "spec.comms.mode" || p.To == "spec.comms.eventSourceRefs"
		if network && !perApp && (strings.HasPrefix(p.To, compose.MapToSpec) || strings.HasPrefix(p.To, compose.MapToNetwork)) {
			return fmt.Errorf("--%s maps to %s, which the market network sets for its child applications", p.Name, p.To)
		}
		switch {
		case p.To == compose.MapToOptions:
			r.appOptions = append(r.appOptions, v1alpha1.ApplicationOption{Name: p.Name, Value: p.Value})
		case p.To == compose.MapToCLI:
			switch p.Name {
			case "exec":
				r.exec = p.Value
			case "hold", "keep-vm":
				r.hold = p.Value != "false"
			case "skip-ssh":
				r.skipSSH = p.Value != "false"
			}
		case p.To == "spec.identity.mode":
			switch p.Name {
			case "atproto-oauth":
				r.root.Identity.Mode = v1alpha1.MarketIdentityModeOAuthBrowser
			case "atproto-oauth-qr":
				r.root.Identity.Mode = v1alpha1.MarketIdentityModeOAuthQr
			default:
				r.root.Identity.Mode = v1alpha1.MarketIdentityMode(p.Value)
			}
		case p.To == "spec.identity.handle":
			r.root.Identity.Handle = p.Value
		case p.To == "spec.identity.pdsUrl":
			r.root.Identity.PdsURL = p.Value
		case p.To == "spec.identity.association":
			if p.Value != "false" {
				r.root.Identity.Association = v1alpha1.AssociationSkip
			}
		case p.To == "spec.bidderDids":
			r.root.BidderDids = append(r.root.BidderDids, splitList(p.Value)...)
		case strings.HasPrefix(p.To, "spec.ingress"):
			var applied bool
			var err error
			if r.root.Ingress, applied, err = applyIngressOption(r.root.Ingress, p.Name, p.Value); err != nil {
				return err
			}
			if !applied {
				return fmt.Errorf("--%s maps to %s, which run request does not take", p.Name, p.To)
			}
		case p.To == "spec.secretsFallback":
			switch fallback := v1alpha1.SecretsFallback(strings.TrimSpace(p.Value)); fallback {
			case v1alpha1.SecretsFallbackNone, v1alpha1.SecretsFallbackSealed:
				r.root.SecretsFallback = fallback
			default:
				return fmt.Errorf("--secrets-fallback %q: want None or Sealed", p.Value)
			}
		case strings.HasPrefix(p.To, "spec.comms"):
			var applied bool
			var err error
			if r.root.Comms, applied, err = applyCommsOption(r.root.Comms, p.Name, p.Value); err != nil {
				return err
			}
			if !applied {
				return fmt.Errorf("--%s maps to %s, which run request does not take", p.Name, p.To)
			}
		case strings.HasPrefix(p.To, compose.MapToNetwork):
			nvSet = true
			switch p.To {
			case "network.plcDirectoryUrl":
				nv.PLCDirectoryURL = p.Value
			case "network.ingressProxyHost":
				nv.IngressProxyHost = p.Value
			case "network.qrOrigin":
				nv.QrOrigin = p.Value
			case "network.hosts":
				for _, h := range splitList(p.Value) {
					name, addr, ok := strings.Cut(h, "=")
					if !ok {
						return fmt.Errorf("--network-hosts %q: want name=address,...", p.Value)
					}
					nv.Hosts = append(nv.Hosts, v1alpha1.NetworkHost{Name: name, Address: addr})
				}
			}
		case p.To == "contract.bidderDids":
			r.contract.BidderDids = append(r.contract.BidderDids, splitList(p.Value)...)
		case p.To == "contract.denyBidderDids":
			r.contract.DenyBidderDids = append(r.contract.DenyBidderDids, splitList(p.Value)...)
		case p.To == "contract.vmName":
			r.contract.VMName = p.Value
		case p.To == "contract.userData":
			b, err := os.ReadFile(p.Value)
			if err != nil {
				return fmt.Errorf("--user-data: %w", err)
			}
			r.contract.UserData = string(b)
		case p.To == "contract.sshAuthorizedKeys":
			r.contract.SshAuthorizedKeys = append(r.contract.SshAuthorizedKeys, splitList(p.Value)...)
		case p.To == "contract.policy":
			r.contract.Policy = p.Value
		case p.To == "contract.policyArgs":
			r.contract.PolicyArgs = p.Value
		case p.To == "contract.vmReadyTimeoutSec":
			n, err := strconv.Atoi(p.Value)
			if err != nil {
				return fmt.Errorf("--vm-ready-timeout-sec %q: %w", p.Value, err)
			}
			r.contract.VMReadyTimeoutSec = n
		case p.To == "contract.secretsRef":
			r.secrets = p.Value
		case p.To == "contract.tunnels":
			tunnels, err := parseTunnels(p.Value)
			if err != nil {
				return fmt.Errorf("--tunnel: %w", err)
			}
			r.contract.Tunnels = append(r.contract.Tunnels, tunnels...)
		default:
			return fmt.Errorf("--%s maps to %s, which run request does not take", p.Name, p.To)
		}
	}
	if nvSet {
		nv.PLCDirectoryURL = cmp.Or(nv.PLCDirectoryURL, "https://plc.directory")
		nv.IngressProxyHost = cmp.Or(nv.IngressProxyHost, "xrpc.fedproxy.com")
		r.root.Network = &nv
	}
	if !network && r.root.Ingress == nil {
		r.root.Ingress = compose.DefaultIngressConfig()
		if nv.IngressProxyHost != "" {
			r.root.Ingress.Xrpc.IngressProxyHost = nv.IngressProxyHost
		}
	}
	r.root.Options = r.appOptions
	return nil
}

func parseTunnels(value string) ([]v1alpha1.ComputeContractTunnel, error) {
	var out []v1alpha1.ComputeContractTunnel
	for _, item := range splitList(value) {
		parts := strings.Split(item, ":")
		if len(parts) != 2 && len(parts) != 3 {
			return nil, fmt.Errorf("%q: want <tag>:<port>[:<transport>]", item)
		}
		port, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("%q: port: %w", item, err)
		}
		tunnel := v1alpha1.ComputeContractTunnel{Tag: parts[0], Port: port}
		if len(parts) == 3 {
			switch transport := v1alpha1.TunnelTransport(parts[2]); transport {
			case v1alpha1.TunnelTransportOpenTunnel, v1alpha1.TunnelTransportWebRTC:
				tunnel.Transport = transport
			default:
				return nil, fmt.Errorf("%q: transport %q is not %s or %s", item, parts[2], v1alpha1.TunnelTransportOpenTunnel, v1alpha1.TunnelTransportWebRTC)
			}
		}
		out = append(out, tunnel)
	}
	return out, nil
}

func (r *requestRun) webrtcTunnel() error {
	if r.transport != transportWebRTC {
		return nil
	}
	for _, t := range r.contract.Tunnels {
		if t.Tag == r.tag {
			if t.Transport != v1alpha1.TunnelTransportWebRTC {
				return fmt.Errorf("--transport webrtc: the tunnel %s has transport %s, not webrtc", t.Tag, cmp.Or(string(t.Transport), string(v1alpha1.TunnelTransportOpenTunnel)))
			}
			return nil
		}
	}
	r.contract.Tunnels = append(r.contract.Tunnels, v1alpha1.ComputeContractTunnel{Tag: r.tag, Port: sshPort, Transport: v1alpha1.TunnelTransportWebRTC})
	return nil
}

func secretsApplied(c *v1alpha1.ComputeContract) bool {
	return c.Status.SecretsFetchedAt != "" || meta.IsStatusConditionTrue(c.Status.Conditions, v1alpha1.ComputeContractConditionSecretsApplied)
}

func readSecrets(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Path  string `json:"path"`
		Value any    `json:"value"`
	}
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("%s is not a JSON array of {path, value}: %w", path, err)
	}
	for i, e := range entries {
		if !strings.HasPrefix(e.Path, "/") || strings.Contains(e.Path, "/../") {
			return nil, fmt.Errorf("%s: entry %d path %q is not an absolute path", path, i, e.Path)
		}
	}
	return b, nil
}

func (r *requestRun) networkLayout() (devinstance.Layout, devinstance.Record, error) {
	l, err := r.a.layoutOf(r.inv.Network, r.vals)
	if err != nil {
		return l, devinstance.Record{}, err
	}
	var rec devinstance.Record
	if err := fsx.ReadJSON(l.Record, &rec); err != nil {
		return l, rec, fmt.Errorf("market network %s has no instance record (dev up --profile market-network first): %w", r.inv.Network, err)
	}
	if rec.Profile != bootstrap.ProfileMarketNetwork || rec.AdminKubeconfig == "" {
		return l, rec, fmt.Errorf("instance %s is not a market network (profile %s)", r.inv.Network, rec.Profile)
	}
	return l, rec, nil
}

func (r *requestRun) childRecord(netLayout devinstance.Layout, instance string) (devinstance.Record, error) {
	cl, err := devinstance.NewLayout(instance, filepath.Join(netLayout.StateDir, compose.AppsDir), filepath.Join(netLayout.RuntimeDir, compose.AppsDir))
	if err != nil {
		return devinstance.Record{}, err
	}
	var rec devinstance.Record
	if err := fsx.ReadJSON(cl.Record, &rec); err != nil {
		return rec, err
	}
	return rec, nil
}

func (r *requestRun) start(ctx context.Context, fresh bool) error {
	started := time.Now()
	var kubeconfig, workspace string
	switch r.inv.Mode {
	case request.ModeNetwork:
		netLayout, netRec, err := r.networkLayout()
		if err != nil {
			return err
		}
		if fresh {
			app := v1alpha1.NetworkApp{Name: r.inv.Name, Kind: v1alpha1.ApplicationKindMarketRequester,
				Profile: marketrequester.RequesterProfileNetwork, Options: r.appOptions, SecretsFallback: r.root.SecretsFallback}
			if r.root.Comms != nil {
				app.Components = v1alpha1.ComponentOverrides{compose.ComponentKindRequester: {Comms: r.root.Comms}}
			}
			if err := request.AddToNetwork(ctx, netRec.AdminKubeconfig, app); err != nil {
				return err
			}
		} else if err := request.SetStopped(ctx, netRec.AdminKubeconfig, r.inv.Name, false); err != nil {
			return err
		}
		r.log("the market network places the invocation in its workspace of the network's kcp", "network", r.inv.Network)
		app, err := request.WaitNetworkApp(ctx, netRec.AdminKubeconfig, r.inv.Name, r.networkTimeout, func(a v1alpha1.NetworkAppStatus) bool {
			return a.Instance != "" && a.KcpURL != "" && a.Message != marketnetwork.StoppedMessage && a.Phase != v1alpha1.ApplicationPhasePending
		}, func(a v1alpha1.NetworkAppStatus) {
			r.log("network child", "phase", string(a.Phase), "message", a.Message)
		})
		if err != nil {
			return err
		}
		rec, err := r.childRecord(netLayout, app.Instance)
		if err != nil {
			return err
		}
		r.instanceDir = rec.Layout.StateDir
		r.inv.Instance, r.summary.Instance, r.summary.KcpURL = app.Instance, app.Instance, rec.KcpURL
		kubeconfig, workspace = rec.AdminKubeconfig, rec.BaseWorkspace
	default:
		vals := r.vals.With("instance", r.inv.Name).With("profile", bootstrap.ProfileQuick).
			With("state-dir", request.Dir(r.stateRoot, r.inv.Name))
		up, code := bringUp(ctx, r.a, vals, false)
		if code != exitOK {
			return fmt.Errorf("the invocation's kcp did not come up (exit %d)", code)
		}
		r.standalone, r.instanceDir = up.layout, up.layout.StateDir
		r.inv.Instance, r.summary.Instance, r.summary.KcpURL = up.Instance, up.Instance, up.KcpURL
		kubeconfig = up.AdminKubeconfig
	}
	k, err := request.OpenKcp(kubeconfig, workspace)
	if err != nil {
		return err
	}
	r.kcp = k.ForContract(r.target)
	if r.inv.Mode == request.ModeStandalone && fresh {
		if err := k.ApplyRoot(ctx, r.root); err != nil {
			return fmt.Errorf("applying the MarketRequester root object: %w", err)
		}
	}
	r.summary.KcpColdSeconds = time.Since(started).Seconds()
	r.summary.KcpRunning = true
	r.log("the invocation's kcp runs", "kcp", r.summary.KcpURL, "workspace", workspace, "instance", r.inv.Instance, "coldSeconds", r.summary.KcpColdSeconds)
	if err := r.kcp.WaitReady(ctx, r.networkTimeout, func(st v1alpha1.ApplicationStatus) {
		r.log("requester", "phase", string(st.Phase), "message", st.Message, "components", len(st.Components))
	}); err != nil {
		return err
	}
	return request.WriteInvocation(r.inv)
}

func (r *requestRun) stop(ctx context.Context, remove bool) {
	r.keepLogs()
	switch r.inv.Mode {
	case request.ModeNetwork:
		_, netRec, err := r.networkLayout()
		if err != nil {
			r.a.log.Warn("stopping the invocation", "err", err.Error())
			return
		}
		if remove {
			err = request.RemoveFromNetwork(ctx, netRec.AdminKubeconfig, r.inv.Name)
		} else {
			err = request.SetStopped(ctx, netRec.AdminKubeconfig, r.inv.Name, true)
		}
		if err != nil {
			r.a.log.Warn("stopping the invocation", "err", err.Error())
			return
		}
		if !remove {
			_, err = request.WaitNetworkApp(ctx, netRec.AdminKubeconfig, r.inv.Name, r.networkTimeout, func(a v1alpha1.NetworkAppStatus) bool {
				return a.Message == marketnetwork.StoppedMessage
			}, nil)
		} else {
			err = request.WaitNetworkGone(ctx, netRec.AdminKubeconfig, r.inv.Name, r.networkTimeout)
		}
		if err != nil {
			r.a.log.Warn("stopping the invocation", "err", err.Error())
		}
	default:
		l := r.standalone
		if l.ID == "" {
			vals := r.vals.With("instance", r.inv.Name).With("state-dir", request.Dir(r.stateRoot, r.inv.Name))
			var err error
			if l, err = r.a.layoutOf(r.inv.Name, vals); err != nil {
				r.a.log.Warn("stopping the invocation", "err", err.Error())
				return
			}
		}
		d := downInstance(ctx, r.a, l, !remove)
		if len(d.Remaining) > 0 {
			r.a.log.Warn("processes of the invocation's kcp remain", "pids", fmt.Sprint(d.Remaining))
		}
	}
	r.summary.KcpRunning = false
	r.log("the invocation's kcp is stopped", "stateKept", !remove)
}

func (r *requestRun) writeSummary(ctx context.Context, c *v1alpha1.ComputeContract) {
	r.summary.UpdatedAt = time.Now().UTC()
	if c != nil {
		r.summary.SecretsFetchedAt = c.Status.SecretsFetchedAt
		r.summary.Phase, r.summary.Message, r.summary.SecretsUrl = string(c.Status.Phase), c.Status.Message, c.Status.SecretsURL
		r.summary.Contract = request.SummaryOf(c)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), requestSummaryRead)
	defer cancel()
	if st, err := r.kcp.Root(ctx); err == nil && st.Did != "" {
		r.summary.Did = st.Did
	}
	if err := request.WriteSummary(r.stateRoot, r.summary); err != nil {
		r.a.log.Warn("writing summary.json", "err", err.Error())
	}
}

func (r *requestRun) signIn(ctx context.Context) error {
	if err := r.kcp.WaitIdentityObject(ctx, r.networkTimeout); err != nil {
		return err
	}
	ids, err := r.kcp.Identities()
	if err != nil {
		return err
	}
	var in io.Reader
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		in = os.Stdin
	}
	wctx, cancel := context.WithTimeout(ctx, requestIdentityWait)
	defer cancel()
	_, err = appcli.IdentityRenderer{Client: ids, Watch: r.kcp.AwaitIdentity, Out: r.a.stderr, In: in}.WaitReady(wctx, marketrequester.RequesterIdentity)
	return err
}

func leftHeld(c *v1alpha1.ComputeContract) bool {
	return c.Status.Phase != v1alpha1.ComputeContractHeld
}

func contractSettled(c *v1alpha1.ComputeContract) bool {
	switch c.Status.Phase {
	case v1alpha1.ComputeContractHeld, v1alpha1.ComputeContractFailed, v1alpha1.ComputeContractReleased:
		return c.Status.ObservedGeneration == c.Generation || c.Status.ObservedGeneration == 0
	}
	return false
}

func (r *requestRun) progress(c *v1alpha1.ComputeContract) {
	phase := ""
	if c.Status.Contract != nil {
		phase = string(c.Status.Contract.Phase)
	}
	r.log("contract", "phase", string(c.Status.Phase), "contractPhase", phase, "message", c.Status.Message)
}

func (r *requestRun) sshClient(ctx context.Context) (request.SSH, error) {
	s := request.SSH{Dir: filepath.Join(request.Dir(r.stateRoot, r.inv.Name), request.SSHDir)}
	if r.transport == transportWebRTC {
		proxy, err := r.proxyCommand()
		if err != nil {
			return s, err
		}
		s.ProxyCommand = proxy
		return s, nil
	}
	dp, err := r.a.devExe(ctx, r.vals, "dumbpipe", "dumbpipe-path")
	if err != nil {
		return s, err
	}
	s.Dumbpipe = dp.Path
	return s, nil
}

func (r *requestRun) proxyCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving this binary's own path for the ProxyCommand: %w", err)
	}
	words := []string{exe, "run", "request", "proxy", r.inv.Name, "--tag", r.tag, "--state-dir", r.stateRoot}
	if r.target != "" {
		words = append(words, "--contract", r.target)
	}
	for i, w := range words {
		words[i] = "'" + strings.ReplaceAll(w, "'", `'"'"'`) + "'"
	}
	return strings.Join(words, " "), nil
}

func (r *requestRun) ssh(ctx context.Context, c *v1alpha1.ComputeContract) (int, error) {
	started := time.Now()
	s, err := r.sshClient(ctx)
	if err != nil {
		return -1, err
	}
	t, err := s.Target(c)
	if err != nil {
		return -1, err
	}
	how := "its iroh ticket"
	if r.transport == transportWebRTC {
		how = "a WebRTC datachannel of tunnel " + r.tag + " (run request proxy)"
	}
	r.log("ssh to the guest over "+how+", StrictHostKeyChecking=yes against its reported host keys", "alias", t.Alias,
		"hostKeys", strings.Join(request.SummaryOf(c).HostKeys, ", "))
	held, stop := r.whileHeld(ctx)
	defer stop()
	if r.transport != transportWebRTC {
		err = s.WaitReady(held, t, requestSSHReady, func(attempt int, msg string) {
			r.log("ssh not ready yet", "attempt", attempt, "err", msg)
		})
	}
	if cause := context.Cause(held); err != nil && cause != nil && ctx.Err() == nil {
		return -1, cause
	}
	if err != nil {
		return -1, err
	}
	interactive := false
	if r.exec == "" {
		fi, err := os.Stdin.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			r.log("no --exec and stdin is not a terminal: the guest answered ssh; no session opened")
			return 0, nil
		}
		interactive = true
	}
	code, err := r.runSSH(held, s, t, interactive)
	r.log("ssh returned", "transport", r.transport, "exit", code, "seconds", time.Since(started).Seconds())
	if cause := context.Cause(held); cause != nil && ctx.Err() == nil {
		r.log("ssh session ended because the contract left Held", "err", cause)
		return 0, nil
	}
	if err != nil {
		if cause := context.Cause(held); cause != nil && ctx.Err() == nil {
			r.log("ssh session ended because the contract left Held", "err", cause)
			return 0, nil
		}
		if current, cerr := r.kcp.AwaitContract(ctx, 15*time.Second, leftHeld); cerr == nil {
			r.log("ssh session ended because the contract left Held", "phase", current.Status.Phase, "err", err)
			return 0, nil
		}
	}
	if r.hold {
		if current, cerr := r.kcp.AwaitContract(ctx, 5*time.Second, leftHeld); cerr == nil {
			r.log("ssh session ended because the contract left Held", "phase", current.Status.Phase)
			return 0, nil
		}
	}
	r.log("ssh session ended", "exit", code)
	return code, err
}

func (r *requestRun) runSSH(ctx context.Context, s request.SSH, t request.Target, interactive bool) (int, error) {
	deadline := time.Now().Add(requestSSHReady)
	for attempt := 1; ; attempt++ {
		began := time.Now()
		code, err := s.Run(ctx, t, r.exec, interactive, os.Stdin, r.a.stdout, r.a.stderr)
		if r.transport != transportWebRTC || err != nil || code != sshConnectionFailed || time.Since(began) > requestSSHRetryRun ||
			time.Now().After(deadline) || ctx.Err() != nil {
			return code, err
		}
		r.log("ssh over webrtc did not connect; trying again", "attempt", attempt, "seconds", time.Since(began).Seconds())
		select {
		case <-ctx.Done():
			return code, nil
		case <-time.After(request.SSHPollInterval):
		}
	}
}

func (r *requestRun) whileHeld(ctx context.Context) (context.Context, func()) {
	held, cancel := context.WithCancelCause(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		if c, err := r.kcp.AwaitContract(held, 0, leftHeld); err == nil {
			cancel(fmt.Errorf("the contract left Held while ssh waited for the guest: phase %s: %s", c.Status.Phase, c.Status.Message))
		}
	})
	return held, func() {
		cancel(nil)
		wg.Wait()
	}
}

func (r *requestRun) release(ctx context.Context) (*v1alpha1.ComputeContract, error) {
	if err := r.kcp.Release(ctx); err != nil {
		return nil, fmt.Errorf("setting spec.release: %w", err)
	}
	r.log("asked the requester to release the contract (spec.release)")
	return r.kcp.WaitContract(ctx, r.contractTimeout, func(c *v1alpha1.ComputeContract) bool {
		return c.Status.Phase == v1alpha1.ComputeContractReleased ||
			(c.Status.Phase == v1alpha1.ComputeContractFailed && c.Spec.Release && c.Status.Contract != nil && c.Status.Contract.Release != nil)
	}, r.progress)
}

func (r *requestRun) carryOn(ctx context.Context) int {
	if err := r.signIn(ctx); err != nil {
		r.a.log.Error("signing in", "err", err.Error())
		c, _ := r.kcp.Contract(context.WithoutCancel(ctx))
		r.writeSummary(ctx, c)
		r.stop(context.WithoutCancel(ctx), false)
		return exitFailed
	}
	c, err := r.kcp.WaitContract(ctx, r.contractTimeout, contractSettled, r.progress)
	if err != nil {
		r.a.log.Error("the contract did not settle", "err", err.Error())
		r.writeSummary(ctx, c)
		r.stop(context.WithoutCancel(ctx), false)
		r.a.printJSON(r.summary)
		return exitFailed
	}
	if c.Status.Phase == v1alpha1.ComputeContractHeld && c.Spec.SecretsRef != nil && !secretsApplied(c) {
		r.log("waiting for the guest to write its secrets (getSecrets, or the sealed fallback when enabled)")
		if fc, err := r.kcp.WaitContract(ctx, r.contractTimeout, func(c *v1alpha1.ComputeContract) bool {
			return secretsApplied(c) || c.Status.Phase != v1alpha1.ComputeContractHeld
		}, r.progress); err != nil {
			r.a.log.Error("the guest did not write its secrets", "err", err.Error())
		} else {
			c = fc
			cond := meta.FindStatusCondition(c.Status.Conditions, v1alpha1.ComputeContractConditionSecretsApplied)
			reason := ""
			if cond != nil {
				reason = cond.Reason
			}
			r.log("the guest wrote its secrets", "via", reason, "fetchedAt", c.Status.SecretsFetchedAt)
		}
	}
	code := exitOK
	hasContract := c.Status.Contract != nil && c.Status.Contract.Receipt != nil
	if c.Status.Phase == v1alpha1.ComputeContractHeld && !r.skipSSH {
		ec, err := r.ssh(ctx, c)
		if err != nil {
			r.a.log.Error("ssh", "err", err.Error())
			code = exitFailed
		} else if ec != 0 {
			code = exitFailed
		}
	}
	if c.Status.Phase == v1alpha1.ComputeContractFailed && !hasContract {
		code = exitFailed
	}
	remove := c.Status.Phase == v1alpha1.ComputeContractReleased || !hasContract
	if !r.hold && hasContract && c.Status.Phase != v1alpha1.ComputeContractReleased {
		rc, err := r.release(context.WithoutCancel(ctx))
		if rc != nil {
			c = rc
		}
		if err != nil || c.Status.Phase != v1alpha1.ComputeContractReleased {
			r.a.log.Error("the release did not finish", "err", fmt.Sprint(err), "phase", string(c.Status.Phase))
			code = exitFailed
		} else {
			remove = true
		}
	}
	r.writeSummary(ctx, c)
	r.stop(context.WithoutCancel(ctx), remove)
	r.writeSummary(ctx, nil)
	r.a.printJSON(r.summary)
	return code
}

func runRequest(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	pos, pass := splitWords(rest)
	if len(pos) > 0 {
		return a.fail(exitRefused, "run request: unexpected %q; request-vm-ssh options go after --", strings.Join(pos, " "))
	}
	for _, tool := range []string{"ssh", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			return a.fail(exitRefused, "run request: %s is not on PATH; install the OpenSSH client (apt install openssh-client, dnf install openssh-clients)", tool)
		}
	}
	r, err := newRequestRun(a, vals)
	if err != nil {
		return a.fail(exitRefused, "run request: %v", err)
	}
	name := vals.String("name")
	if name == "" {
		name = "req-" + strings.ToLower(time.Now().UTC().Format("20060102t150405"))
	}
	if err := request.CheckName(name); err != nil {
		return a.fail(exitRefused, "run request: %v", err)
	}
	if _, err := os.Stat(request.Dir(r.stateRoot, name)); err == nil {
		return a.fail(exitRefused, "run request: invocation %s exists under %s; run request resume|release %s", name, request.Root(r.stateRoot), name)
	}
	network := vals.String("network")
	if err := r.passthrough(pass, network != ""); err != nil {
		return a.fail(exitRefused, "run request: %v", err)
	}
	if err := r.webrtcTunnel(); err != nil {
		return a.fail(exitRefused, "run request: %v", err)
	}
	var bundle []byte
	if r.secrets != "" {
		if bundle, err = readSecrets(r.secrets); err != nil {
			return a.fail(exitRefused, "run request: --secrets: %v", err)
		}
	}
	r.inv = request.Invocation{Name: name, Mode: request.ModeStandalone, Network: network, StateRoot: r.stateRoot, CreatedAt: time.Now().UTC()}
	if network != "" {
		r.inv.Mode = request.ModeNetwork
	}
	r.summary = request.Summary{Name: name, Mode: r.inv.Mode, Network: network, CreatedAt: r.inv.CreatedAt}
	if !r.skipSSH {
		pub, _, err := request.EnsureKey(ctx, filepath.Join(request.Dir(r.stateRoot, name), request.SSHDir))
		if err != nil {
			return a.fail(exitFailed, "run request: %v", err)
		}
		r.contract.SshAuthorizedKeys = append([]string{pub}, r.contract.SshAuthorizedKeys...)
	}
	if err := request.WriteInvocation(r.inv); err != nil {
		return a.fail(exitFailed, "run request: %v", err)
	}
	r.log("a fresh kcp for this invocation", "mode", r.inv.Mode, "network", network)
	if err := r.start(ctx, true); err != nil {
		r.writeSummaryQuiet()
		r.stop(context.WithoutCancel(ctx), false)
		return a.fail(exitFailed, "run request: %v", err)
	}
	if bundle != nil {
		ref, err := r.kcp.PutSecrets(ctx, bundle, r.networkTimeout)
		if err != nil {
			r.stop(context.WithoutCancel(ctx), false)
			return a.fail(exitFailed, "run request: %v", err)
		}
		r.contract.SecretsRef = &ref
		r.log("the secrets bundle is in the invocation's own OpenBao", "mount", ref.Mount, "path", ref.Path)
	}
	if err := r.kcp.CreateContract(ctx, r.contract, r.networkTimeout); err != nil {
		r.stop(context.WithoutCancel(ctx), false)
		return a.fail(exitFailed, "run request: %v", err)
	}
	return r.carryOn(ctx)
}

func (r *requestRun) writeSummaryQuiet() {
	if err := request.WriteSummary(r.stateRoot, r.summary); err != nil {
		r.a.log.Warn("writing summary.json", "err", err.Error())
	}
}

func (a *app) loadRequest(vals clicfg.Values, rest []string, verb string) (*requestRun, int) {
	pos, pass := splitWords(rest)
	if len(pos) != 1 || len(pass) > 0 {
		return nil, a.fail(exitRefused, "run request %s <name>: one invocation name, no request-vm-ssh options", verb)
	}
	r, err := newRequestRun(a, vals)
	if err != nil {
		return nil, a.fail(exitRefused, "run request %s: %v", verb, err)
	}
	inv, err := request.ReadInvocation(r.stateRoot, pos[0])
	if err != nil {
		return nil, a.fail(exitRefused, "run request %s: %v", verb, err)
	}
	r.inv = inv
	if s, err := request.ReadSummary(r.stateRoot, inv.Name); err == nil {
		r.summary = s
	} else {
		r.summary = request.Summary{Name: inv.Name, Mode: inv.Mode, Network: inv.Network, Instance: inv.Instance, CreatedAt: inv.CreatedAt}
	}
	return r, exitOK
}

func (a *app) reopenRequest(ctx context.Context, vals clicfg.Values, rest []string, verb string) (*requestRun, int) {
	r, code := a.loadRequest(vals, rest, verb)
	if r == nil {
		return nil, code
	}
	r.log("starting the invocation's kcp again from its state dir", "verb", verb)
	if err := r.start(ctx, false); err != nil {
		r.stop(context.WithoutCancel(ctx), false)
		return nil, a.fail(exitFailed, "run request %s: %v", verb, err)
	}
	return r, exitOK
}

func runRequestResume(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	r, code := a.reopenRequest(ctx, vals, rest, "resume")
	if r == nil {
		return code
	}
	return r.carryOn(ctx)
}

func runRequestRelease(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	r, code := a.reopenRequest(ctx, vals, rest, "release")
	if r == nil {
		return code
	}
	c, err := r.release(ctx)
	if err != nil || c == nil || c.Status.Phase != v1alpha1.ComputeContractReleased {
		phase := ""
		if c != nil {
			phase = string(c.Status.Phase)
		}
		r.a.log.Error("the release did not finish", "err", fmt.Sprint(err), "phase", phase)
		r.writeSummary(ctx, c)
		r.stop(context.WithoutCancel(ctx), false)
		r.writeSummary(ctx, nil)
		r.a.printJSON(r.summary)
		return exitFailed
	}
	r.writeSummary(ctx, c)
	r.stop(context.WithoutCancel(ctx), true)
	r.writeSummary(ctx, nil)
	r.a.printJSON(r.summary)
	return exitOK
}

func runRequestList(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	root, err := devinstance.StateRoot(vals.String("state-dir"), a.env)
	if err != nil {
		return a.fail(exitRefused, "run request list: %v", err)
	}
	list, err := request.List(root)
	if err != nil {
		return a.fail(exitFailed, "run request list: %v", err)
	}
	if vals.Bool("json") {
		a.printJSON(list)
		return exitOK
	}
	for _, s := range list {
		c := request.ContractSummary{}
		if s.Contract != nil {
			c = *s.Contract
		}
		where := s.Mode
		if s.Network != "" {
			where = "network " + s.Network
		}
		fmt.Fprintf(a.stdout, "%-24s %-20s %-10s kcp=%-5t winner=%s guest=%s %s\n", s.Name, where, cmp.Or(s.Phase), s.KcpRunning,
			cmp.Or(c.WinnerDid), cmp.Or(c.VMAddress), s.Message)
	}
	if len(list) == 0 {
		fmt.Fprintln(a.stdout, "no invocations under", request.Root(root))
	}
	return exitOK
}

func (r *requestRun) keepLogs() {
	if r.instanceDir == "" {
		return
	}
	b, err := os.ReadFile(filepath.Join(r.instanceDir, "logs", "core.log"))
	if err != nil {
		return
	}
	out := filepath.Join(request.Dir(r.stateRoot, r.inv.Name), "logs")
	if err := os.MkdirAll(out, 0o700); err != nil {
		return
	}
	os.WriteFile(filepath.Join(out, "core.log"), b, 0o600)
}

func (r *requestRun) attach(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, requestAttachProbe)
	defer cancel()
	var kubeconfig, workspace string
	switch r.inv.Mode {
	case request.ModeNetwork:
		netLayout, netRec, err := r.networkLayout()
		if err != nil {
			return err
		}
		app, ok, err := request.NetworkApp(ctx, netRec.AdminKubeconfig, r.inv.Name)
		if err != nil {
			return err
		}
		if !ok || app.Instance == "" || app.Message == marketnetwork.StoppedMessage {
			return errors.New("the market network does not run the invocation")
		}
		rec, err := r.childRecord(netLayout, app.Instance)
		if err != nil {
			return err
		}
		r.instanceDir = rec.Layout.StateDir
		kubeconfig, workspace = rec.AdminKubeconfig, rec.BaseWorkspace
	default:
		l, err := r.a.layoutOf(r.inv.Name, r.vals.With("instance", r.inv.Name).With("state-dir", request.Dir(r.stateRoot, r.inv.Name)))
		if err != nil {
			return err
		}
		var rec devinstance.Record
		if err := fsx.ReadJSON(l.Record, &rec); err != nil {
			return err
		}
		if rec.AdminKubeconfig == "" {
			return errors.New("the invocation's kcp has no admin kubeconfig")
		}
		r.standalone, r.instanceDir = l, l.StateDir
		kubeconfig = rec.AdminKubeconfig
	}
	k, err := request.OpenKcp(kubeconfig, workspace)
	if err != nil {
		return err
	}
	st, err := k.Root(ctx)
	if err != nil {
		return err
	}
	if st.Phase != v1alpha1.ApplicationPhaseReady {
		return fmt.Errorf("the MarketRequester is %s", st.Phase)
	}
	r.kcp = k.ForContract(r.target)
	return nil
}

func (a *app) openRequest(ctx context.Context, vals clicfg.Values, rest []string, verb string) (*requestRun, bool, int) {
	r, code := a.loadRequest(vals, rest, verb)
	if r == nil {
		return nil, false, code
	}
	err := r.attach(ctx)
	if err == nil {
		return r, false, exitOK
	}
	r.log("the invocation's kcp does not answer; starting it again from its state dir", "verb", verb, "why", err.Error())
	if err := r.start(ctx, false); err != nil {
		r.stop(context.WithoutCancel(ctx), false)
		return nil, false, a.fail(exitFailed, "run request %s: %v", verb, err)
	}
	return r, true, exitOK
}

func runRequestSSH(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	r, started, code := a.openRequest(ctx, vals, rest, "ssh")
	if r == nil {
		return code
	}
	if started {
		defer r.stop(context.WithoutCancel(ctx), false)
	}
	c, err := r.kcp.WaitContract(ctx, r.contractTimeout, contractSettled, r.progress)
	if err != nil {
		return a.fail(exitFailed, "run request ssh: %v", err)
	}
	if c.Status.Phase != v1alpha1.ComputeContractHeld {
		return a.fail(exitFailed, "run request ssh: the contract is %s, not Held: %s", c.Status.Phase, c.Status.Message)
	}
	ec, err := r.ssh(ctx, c)
	if err != nil {
		return a.fail(exitFailed, "run request ssh: %v", err)
	}
	if ec != 0 {
		return exitFailed
	}
	return exitOK
}

func runRequestProxy(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	level, err := logx.ParseLevel(vals.String("log-level"))
	if err != nil {
		return a.fail(exitRefused, "run request proxy: --log-level: %v", err)
	}
	quiet := *a
	quiet.stdout = a.stderr
	quiet.log = slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: level, ReplaceAttr: dropTime})).With("service", serviceName)
	r, started, code := quiet.openRequest(ctx, vals, rest, "proxy")
	if r == nil {
		return code
	}
	if started {
		defer r.stop(context.WithoutCancel(ctx), false)
	}
	if r.tag == "" {
		return a.fail(exitRefused, "run request proxy: --tag names the webrtc tunnel")
	}
	log := quiet.log.With("request", r.inv.Name)
	conn, timings, err := webrtcproxy.Dial(ctx, r.kcp.Signaling(), webrtcproxy.Options{Tag: r.tag, Log: log})
	if err != nil {
		return a.fail(exitFailed, "run request proxy: %s", strings.Join(strings.Fields(err.Error()), " "))
	}
	defer conn.Close()
	log.Info("the proxy pipes stdin and stdout through the datachannel", "tag", r.tag, "guestSeconds", timings.Guest.Seconds(),
		"offerToAnswerSeconds", timings.Answer.Seconds(), "offerToOpenSeconds", timings.Open.Seconds())
	if err := webrtcproxy.Pipe(ctx, conn, os.Stdin, a.stdout); err != nil && ctx.Err() == nil {
		log.Warn("the proxy ends", "err", err.Error())
	}
	return exitOK
}

func dropTime(groups []string, attr slog.Attr) slog.Attr {
	if len(groups) == 0 && attr.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return attr
}
