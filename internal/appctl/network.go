package appctl

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	denov1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/internal/denokcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpapply"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
)

const (
	NetworkSet          = "market-network"
	NetworkProfile      = "market-network"
	BidderAccountDomain = "bob.test"
	CABundleFile        = "ca-bundle.pem"
	NetworkServiceState = "state"
)

type service struct {
	Workspace string
	Name      string
	Host      string
	Long      bool
}

var networkServices = []service{
	{"global", "plc", "plc.default.global.svc.kcp.local", true},
	{"relay", "relay", "relay.default.relay.svc.kcp.local", true},
	{"relay", "dispatcher", "dispatcher.default.relay.svc.kcp.local", true},
	{"relay", "dispatcher-tls", "", true},
	{"alice", "pds", "pds.default.alice.svc.kcp.local", true},
	{"bob", "pds", "pds.default.bob.svc.kcp.local", true},
	{"alice", "verifier", "", false},
}

type networks struct {
	ctl     *Controller
	mu      sync.Mutex
	applied map[string]int64
	pkiPEM  []byte
}

func newNetworks(ctl *Controller) *networks {
	return &networks{ctl: ctl, applied: map[string]int64{}}
}

func (n *networks) values(spec v1alpha1.MarketNetworkSpec) (manifests.MarketNetwork, error) {
	dir := NetworkStateDir(n.ctl.c.Layout.StateDir)
	alice, err := EnsureHexKey(filepath.Join(dir, keysDir, "alice-pds.hex"))
	if err != nil {
		return manifests.MarketNetwork{}, err
	}
	bob, err := EnsureHexKey(filepath.Join(dir, keysDir, "bob-pds.hex"))
	if err != nil {
		return manifests.MarketNetwork{}, err
	}
	guest := filepath.Join(dir, GuestTLSDir)
	if err := EnsureGuestTLS(guest); err != nil {
		return manifests.MarketNetwork{}, err
	}
	label := spec.GuestLabel
	if label == "" {
		label = n.ctl.c.Layout.ID
	}
	return manifests.MarketNetwork{NetworkID: label, StateDir: filepath.Join(dir, NetworkServiceState), GuestTLSDir: guest,
		PDSPrivateKeyHex: alice, BobPDSPrivateKeyHex: bob}, nil
}

func (n *networks) applyServices(ctx context.Context, owner string, values *manifests.MarketNetwork) error {
	ix, err := manifests.Load()
	if err != nil {
		return err
	}
	set, ok := ix.Set(NetworkSet)
	if !ok {
		return fmt.Errorf("appctl: deploy/sets.yaml names no %s set", NetworkSet)
	}
	ap, err := kcpapply.New(n.ctl.restCfg, kcpapply.Options{WaitTimeout: ApplyTimeout, FieldManager: FieldManager,
		Labels: map[string]string{OwnerLabel: owner}, Log: n.ctl.c.log()})
	if err != nil {
		return err
	}
	rep, err := ap.Apply(ctx, set, values)
	if err != nil {
		return err
	}
	n.ctl.recordPlaced(owner, true, rep)
	return nil
}

type serviceReport struct {
	views      map[string]podView
	components []v1alpha1.ApplicationComponent
	listeners  []v1alpha1.ApplicationListener
	notReady   []string
	checks     []string
	verifier   string
	long       int
}

func (r serviceReport) ready() bool { return len(r.notReady) == 0 }

func (n *networks) services(ctx context.Context) (serviceReport, error) {
	r := serviceReport{views: map[string]podView{}}
	for _, s := range networkServices {
		ws := n.ctl.c.workspace() + ":" + s.Workspace
		v, found, err := n.ctl.pod(ctx, ws, s.Name)
		if err != nil {
			return r, err
		}
		key := s.Workspace + "/" + s.Name
		r.views[key] = v
		r.components = append(r.components, v.component())
		for _, l := range v.Listeners {
			l.Component = key
			r.listeners = append(r.listeners, l)
		}
		if s.Long {
			r.long++
		}
		if !s.Long {
			r.verifier = v.Phase
			continue
		}
		switch {
		case !found:
			r.notReady = append(r.notReady, key+" is not there")
		case v.Phase != "Running" || !v.Ready:
			r.notReady = append(r.notReady, fmt.Sprintf("%s is %s ready=%t", key, v.Phase, v.Ready))
		case len(v.Listeners) == 0:
			r.checks = append(r.checks, key+" reports no listener")
		case !v.stopsItsProcess():
			r.checks = append(r.checks, key+" carries no "+denov1alpha1.FinalizerDenoPod+" finalizer")
		}
	}
	return r, nil
}

const (
	ConditionServicesReady  = "ServicesReady"
	ConditionVerifierPassed = "VerifierPassed"
	ConditionOneKcp         = "OneKcp"
)

func setCondition(conds *[]metav1.Condition, gen int64, t string, ok bool, reason, msg string) {
	s := metav1.ConditionFalse
	if ok {
		s = metav1.ConditionTrue
	}
	meta.SetStatusCondition(conds, metav1.Condition{Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: gen})
}

func (n *networks) oneKcp(apps []v1alpha1.NetworkAppStatus) []string {
	front, _, _ := strings.Cut(strings.TrimSuffix(n.ctl.restCfg.Host, "/"), "/clusters/")
	workspaces := map[string]string{n.ctl.c.workspace(): "the network"}
	var wrong []string
	for _, a := range apps {
		if a.KcpUrl == "" {
			continue
		}
		if f, _, _ := strings.Cut(strings.TrimSuffix(a.KcpUrl, "/"), "/clusters/"); f != front {
			wrong = append(wrong, fmt.Sprintf("%s reports the kcp %s, not the network's %s", a.Name, f, front))
		}
		if a.Workspace == "" {
			wrong = append(wrong, a.Name+" reports no workspace")
			continue
		}
		if other, ok := workspaces[a.Workspace]; ok {
			wrong = append(wrong, fmt.Sprintf("%s shares the workspace %s with %s", a.Name, a.Workspace, other))
		}
		workspaces[a.Workspace] = a.Name
	}
	return wrong
}

func portFrom(address string) (string, error) {
	_, p, err := net.SplitHostPort(address)
	if err != nil || p == "" || p == "0" {
		return "", fmt.Errorf("no bound port in %q", address)
	}
	return p, nil
}

func NetworkValue(views map[string]podView, caBundle string) (v1alpha1.MarketNetworkValue, error) {
	ports := map[string]string{}
	for _, k := range []string{"global/plc", "relay/relay", "relay/dispatcher", "relay/dispatcher-tls"} {
		p, err := portFrom(views[k].Address)
		if err != nil {
			return v1alpha1.MarketNetworkValue{}, fmt.Errorf("%s: %w", k, err)
		}
		ports[k] = p
	}
	var hosts []v1alpha1.NetworkHost
	for _, s := range networkServices {
		if v := views[s.Workspace+"/"+s.Name]; s.Host != "" && v.Address != "" {
			hosts = append(hosts, v1alpha1.NetworkHost{Name: s.Host, Address: v.Address})
		}
	}
	return v1alpha1.MarketNetworkValue{
		Hosts:            hosts,
		PlcDirectoryUrl:  "https://127.0.0.1:" + ports["global/plc"],
		IngressProxyHost: GuestTLSHost + ":" + ports["relay/dispatcher"],
		RelayUrls:        []string{"https://127.0.0.1:" + ports["relay/relay"]},
		FirehoseUrls:     []string{},
		QrOrigin:         "",
		GuestTlsUrl:      "https://" + GuestTLSHost + ":" + ports["relay/dispatcher-tls"],
		CaBundleRef:      caBundle,
	}, nil
}

func (n *networks) caBundle(ctx context.Context) (string, error) {
	dir := NetworkStateDir(n.ctl.c.Layout.StateDir)
	if len(n.pkiPEM) == 0 {
		v, err := n.vault(ctx)
		if err != nil {
			return "", err
		}
		if n.pkiPEM, err = pkiCA(ctx, v); err != nil {
			return "", err
		}
	}
	pki := n.pkiPEM
	guest, err := os.ReadFile(filepath.Join(dir, GuestTLSDir, guestCAFile))
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, CABundleFile)
	want := strings.TrimSpace(string(pki)) + "\n" + strings.TrimSpace(string(guest)) + "\n"
	if have, err := os.ReadFile(path); err == nil && string(have) == want {
		return path, nil
	}
	return path, os.WriteFile(path, []byte(want), 0o644)
}

func (n *networks) deleteServices(ctx context.Context, owner string) (int, error) {
	left := 0
	seen := map[string]bool{}
	for _, s := range networkServices {
		ws := n.ctl.c.workspace() + ":" + s.Workspace
		if seen[ws] {
			continue
		}
		seen[ws] = true
		l, err := n.ctl.deletePods(ctx, ws, owner, nil)
		if err != nil && !apierrors.IsNotFound(err) {
			return left, err
		}
		left += l
	}
	return left, nil
}

func (n *networks) knownChildren() []string {
	entries, err := os.ReadDir(devinstance.DevRoot(filepath.Join(n.ctl.c.Layout.StateDir, AppsDir)))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

func (n *networks) childByID(id string) child {
	name := strings.TrimPrefix(id, n.ctl.c.Layout.ID+"-")
	c, err := n.child(v1alpha1.NetworkApp{Name: name})
	if err != nil {
		l, _ := devinstance.NewLayout(id, filepath.Join(n.ctl.c.Layout.StateDir, AppsDir), filepath.Join(n.ctl.c.Layout.RuntimeDir, AppsDir))
		return child{id: id, layout: l}
	}
	return c
}

func (n *networks) teardownAll(ctx context.Context, keep []string) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []string
	for _, id := range n.knownChildren() {
		if slices.Contains(keep, id) {
			continue
		}
		wg.Add(1)
		go func(c child) {
			defer wg.Done()
			if err := n.teardownChild(ctx, c); err != nil {
				mu.Lock()
				errs = append(errs, c.id+": "+err.Error())
				mu.Unlock()
			}
		}(n.childByID(id))
	}
	wg.Wait()
	if len(errs) > 0 {
		return fmt.Errorf("tearing children down: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (n *networks) reconcile(ctx context.Context, u *unstructured.Unstructured) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	var o v1alpha1.MarketNetwork
	if err := fromUnstructured(u, &o); err != nil {
		return err
	}
	owner := ownerOf(u)
	log := n.ctl.c.log().With("marketnetwork", o.Name)
	st := v1alpha1.MarketNetworkStatus{ObservedGeneration: o.Generation, Network: o.Status.Network, BidderPdsUrl: o.Status.BidderPdsUrl,
		Apps: o.Status.Apps, Conditions: o.Status.Conditions}
	write := func(phase v1alpha1.ApplicationPhase, msg string) error {
		st.Phase, st.Message = phase, msg
		return n.ctl.writeStatus(ctx, MarketNetworkGVR, o.Name, st)
	}
	if u.GetDeletionTimestamp() != nil {
		write(v1alpha1.ApplicationPhaseDeleting, "tearing down the child applications, then the network's services")
		if err := n.teardownAll(ctx, nil); err != nil {
			return write(v1alpha1.ApplicationPhaseDeleting, err.Error())
		}
		left, err := n.deleteServices(ctx, owner)
		if err != nil {
			return err
		}
		if left > 0 {
			return write(v1alpha1.ApplicationPhaseDeleting, fmt.Sprintf("%d service(s) stopping", left))
		}
		gone, err := n.deleteVault(ctx)
		if err != nil {
			return err
		}
		if !gone {
			return write(v1alpha1.ApplicationPhaseDeleting, "the OpenBaoServer "+NetworkVaultServer+" is stopping")
		}
		delete(n.applied, string(o.UID))
		n.ctl.forgetPlaced(owner)
		log.Info("the market network is gone: children, services and its OpenBaoServer")
		return n.ctl.setFinalizer(ctx, MarketNetworkGVR, u, false)
	}
	if !hasFinalizer(u) {
		if err := n.ctl.setFinalizer(ctx, MarketNetworkGVR, u, true); err != nil {
			return err
		}
	}
	if o.Spec.Profile != NetworkProfile {
		return write(v1alpha1.ApplicationPhaseFailed, fmt.Sprintf("profile %q: the built MarketNetwork profile is %s", o.Spec.Profile, NetworkProfile))
	}
	if n.applied[string(o.UID)] != o.Generation || n.ctl.lostComponent(ctx, owner) != "" {
		values, err := n.values(o.Spec)
		if err != nil {
			return write(v1alpha1.ApplicationPhaseFailed, err.Error())
		}
		write(v1alpha1.ApplicationPhaseProvisioning, "applying the network's services")
		if err := n.applyServices(ctx, owner, &values); err != nil {
			return write(v1alpha1.ApplicationPhaseProvisioning, "the network's services: "+err.Error())
		}
		n.applied[string(o.UID)] = o.Generation
		log.Info("applied the network's services", "set", NetworkSet)
	}
	rep, err := n.services(ctx)
	if err != nil {
		return err
	}
	views := rep.views
	st.Components, st.Listeners = rep.components, rep.listeners
	if !rep.ready() {
		setCondition(&st.Conditions, o.Generation, ConditionServicesReady, false, "Waiting", strings.Join(rep.notReady, "; "))
		return write(v1alpha1.ApplicationPhaseProvisioning, "waiting for the network's services to report ready")
	}
	bundle, err := n.caBundle(ctx)
	if err != nil {
		return write(v1alpha1.ApplicationPhaseProvisioning, "the network CA bundle: "+err.Error())
	}
	value, err := NetworkValue(views, bundle)
	if err != nil {
		return write(v1alpha1.ApplicationPhaseProvisioning, err.Error())
	}
	st.Network = &value
	bob := views["bob/pds"]
	pdsPort, err := portFrom(bob.Address)
	if err != nil {
		return write(v1alpha1.ApplicationPhaseProvisioning, "bob/pds: "+err.Error())
	}
	pdsURL := "https://127.0.0.1:" + pdsPort
	st.BidderPdsUrl = pdsURL
	alicePort, err := portFrom(views["alice/pds"].Address)
	if err != nil {
		return write(v1alpha1.ApplicationPhaseProvisioning, "alice/pds: "+err.Error())
	}
	aliceURL := "https://127.0.0.1:" + alicePort
	var keep []string
	for _, app := range o.Spec.Apps {
		if c, err := n.child(app); err == nil {
			keep = append(keep, c.id)
		}
	}
	if err := n.teardownAll(ctx, keep); err != nil {
		log.Warn("removing children the spec no longer names", "err", err.Error())
	}
	var bidderDids []string
	for _, a := range o.Status.Apps {
		if a.Kind == v1alpha1.ApplicationKindMarketBidder && a.Did != "" {
			bidderDids = append(bidderDids, a.Did)
		}
	}
	apps := make([]v1alpha1.NetworkAppStatus, len(o.Spec.Apps))
	var wg sync.WaitGroup
	for i, app := range o.Spec.Apps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			apps[i] = n.ensureApp(ctx, app, value, pdsURL, aliceURL, bidderDids)
		}()
	}
	wg.Wait()
	st.Apps = apps
	allReady := true
	for i, a := range apps {
		if k := o.Spec.Apps[i].Kind; k == v1alpha1.ApplicationKindMarketRequester || k == v1alpha1.ApplicationKindSshGateway {
			continue
		}
		allReady = allReady && a.Phase == v1alpha1.ApplicationPhaseReady
	}
	if len(rep.checks) > 0 {
		setCondition(&st.Conditions, o.Generation, ConditionServicesReady, false, "Checks", strings.Join(rep.checks, "; "))
	} else {
		setCondition(&st.Conditions, o.Generation, ConditionServicesReady, true, "Ready",
			fmt.Sprintf("%d service(s) Running and ready, each listening and carrying %s", rep.long, denov1alpha1.FinalizerDenoPod))
	}
	switch rep.verifier {
	case "Succeeded":
		setCondition(&st.Conditions, o.Generation, ConditionVerifierPassed, true, "Succeeded", "alice/verifier created an account on alice's PDS and saw its commit on the relay")
	default:
		setCondition(&st.Conditions, o.Generation, ConditionVerifierPassed, false, "Phase"+rep.verifier, "alice/verifier is "+rep.verifier)
	}
	if wrong := n.oneKcp(apps); len(wrong) > 0 {
		setCondition(&st.Conditions, o.Generation, ConditionOneKcp, false, "Apart", strings.Join(wrong, "; "))
	} else {
		setCondition(&st.Conditions, o.Generation, ConditionOneKcp, true, "Workspaces", "every child application runs in its own workspace of the network's kcp")
	}
	if !allReady {
		return write(v1alpha1.ApplicationPhaseProvisioning, "the network's services are ready; waiting for its child applications")
	}
	for _, t := range []string{ConditionServicesReady, ConditionVerifierPassed, ConditionOneKcp} {
		if c := meta.FindStatusCondition(st.Conditions, t); c == nil || c.Status != metav1.ConditionTrue {
			return write(v1alpha1.ApplicationPhaseProvisioning, "the child applications are ready; waiting for condition "+t)
		}
	}
	return write(v1alpha1.ApplicationPhaseReady, fmt.Sprintf("%d service(s) and %d child application(s), each in its own workspace of one kcp, are ready", len(rep.components), len(apps)))
}

func (n *networks) ensureApp(ctx context.Context, app v1alpha1.NetworkApp, value v1alpha1.MarketNetworkValue, pdsURL, aliceURL string, bidderDids []string) v1alpha1.NetworkAppStatus {
	out := v1alpha1.NetworkAppStatus{Name: app.Name, Kind: app.Kind, Phase: v1alpha1.ApplicationPhaseProvisioning}
	fail := func(err error) v1alpha1.NetworkAppStatus {
		out.Message = err.Error()
		n.ctl.c.log().Warn("a child application is not ready yet", "app", app.Name, "err", err.Error())
		return out
	}
	c, err := n.child(app)
	if err != nil {
		out.Phase = v1alpha1.ApplicationPhaseFailed
		return fail(err)
	}
	out.Instance = c.id
	if app.Stopped {
		if err := n.stopChild(ctx, c); err != nil {
			return fail(err)
		}
		out.Phase, out.Message = v1alpha1.ApplicationPhasePending, StoppedMessage
		return out
	}
	if _, err := GVRFor(app.Kind); err != nil {
		out.Phase = v1alpha1.ApplicationPhaseFailed
		return fail(err)
	}
	var gw *devinstance.GatewayChild
	if app.Kind == v1alpha1.ApplicationKindGateway {
		if gw, err = GatewayChildOf(ctx, c.id, c.layout.StateDir, app, value, aliceURL); err != nil {
			return fail(err)
		}
	}
	rec, err := n.placeChild(ctx, c, gw)
	if err != nil {
		return fail(err)
	}
	out.KcpUrl, out.Workspace = rec.KcpURL, rec.BaseWorkspace
	meta := metav1.ObjectMeta{Name: ChildRootName}
	var root any
	switch app.Kind {
	case v1alpha1.ApplicationKindMarketBidder:
		handle := app.Name + "." + BidderAccountDomain
		var fc *v1alpha1.FirecrackerProvider
		if app.Firecracker != nil {
			f := *app.Firecracker
			fc = &f
		}
		var kcp *v1alpha1.KcpProvider
		if app.Kcp != nil {
			k := *app.Kcp
			k.Handle = KcpHandle(app)
			kcp = &k
			fc = nil
		}
		root = &v1alpha1.MarketBidder{ObjectMeta: meta, Spec: v1alpha1.MarketBidderSpec{Profile: app.Profile, Network: &value,
			Identity: v1alpha1.ApplicationIdentity{Mode: v1alpha1.MarketIdentityModeLocalRepo, Handle: handle, PdsUrl: pdsURL},
			Policy:   app.Policy, Firecracker: fc, Kcp: kcp, Options: app.Options, Quota: app.Quota}}
	case v1alpha1.ApplicationKindMarketRequester:
		root = &v1alpha1.MarketRequester{ObjectMeta: meta, Spec: v1alpha1.MarketRequesterSpec{Profile: app.Profile, Network: &value,
			Identity:   v1alpha1.ApplicationIdentity{Mode: v1alpha1.MarketIdentityModeLocalRepo, Association: v1alpha1.AssociationSkip},
			BidderDids: bidderDids, Options: app.Options, Quota: app.Quota}}
	case v1alpha1.ApplicationKindSshGateway:
		root = &v1alpha1.SshGateway{ObjectMeta: meta, Spec: v1alpha1.SshGatewaySpec{Profile: app.Profile, Network: &value,
			BidderDids: bidderDids, Options: app.Options, Quota: app.Quota, OwnerSessionsFile: app.OwnerSessionsFile}}
	}
	if root != nil {
		if err := applyRoot(ctx, rec, app.Kind, root); err != nil {
			return fail(fmt.Errorf("applying the root object into %s's workspace %s: %w", c.id, rec.BaseWorkspace, err))
		}
	}
	rs, found, err := readRoot(ctx, rec, app.Kind)
	if err != nil {
		return fail(err)
	}
	if !found {
		return fail(fmt.Errorf("the root object of %s is not readable yet", c.id))
	}
	out.Phase, out.Message, out.Did, out.Components, out.Listeners = rs.Phase, rs.Message, rs.Did, rs.Components, rs.Listeners
	if out.Phase == "" {
		out.Phase = v1alpha1.ApplicationPhasePending
	}
	return out
}
