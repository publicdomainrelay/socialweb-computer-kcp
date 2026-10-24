package request

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketnetwork"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketrequester"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	RequestsDir                  = "requests"
	InvocationFile               = "invocation.json"
	SummaryFile                  = "summary.json"
	SSHDir                       = "ssh"
	ModeNetwork                  = "network"
	ModeStandalone               = "standalone"
	RootName                     = "app"
	FieldManager                 = "socialweb-computer-run-request"
	ReleaseFieldManager          = "socialweb.computer/run-request"
	NetworkAppFieldManager       = "socialweb.computer/network-app"
	ContractTTLSeconds     int32 = 8 * 24 * 60 * 60
	SecretsPath                  = "contracts/" + marketrequester.RequesterContract + "/secrets"
	Poll                         = time.Second
	RetryMax                     = 5 * time.Second
	irohScheme                   = "iroh://"
	maxNameLength                = 30
)

var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func CheckName(name string) error {
	if !nameRE.MatchString(name) || len(name) > maxNameLength {
		return fmt.Errorf("request: name %q is not a DNS label of at most %d characters", name, maxNameLength)
	}
	return nil
}

func Root(stateRoot string) string { return filepath.Join(stateRoot, RequestsDir) }

func Dir(stateRoot, name string) string { return filepath.Join(Root(stateRoot), name) }

type Invocation struct {
	Name      string    `json:"name"`
	Mode      string    `json:"mode"`
	Network   string    `json:"network,omitempty"`
	Instance  string    `json:"instance"`
	StateRoot string    `json:"stateRoot"`
	CreatedAt time.Time `json:"createdAt"`
}

type ContractSummary struct {
	Phase         string   `json:"phase,omitempty"`
	WinnerDid     string   `json:"winnerDid,omitempty"`
	Bids          int      `json:"bids,omitempty"`
	ReceiptUri    string   `json:"receiptUri,omitempty"`
	VMAddress     string   `json:"vmAddress,omitempty"`
	Networks      []string `json:"networks,omitempty"`
	HostKeys      []string `json:"hostKeys,omitempty"`
	ReleaseOK     *bool    `json:"releaseOk,omitempty"`
	ReleaseStatus int      `json:"releaseStatus,omitempty"`
	Termination   string   `json:"termination,omitempty"`
}

type Summary struct {
	Name             string           `json:"name"`
	Mode             string           `json:"mode"`
	Network          string           `json:"network,omitempty"`
	Instance         string           `json:"instance"`
	KcpURL           string           `json:"kcpURL,omitempty"`
	KcpColdSeconds   float64          `json:"kcpColdSeconds,omitempty"`
	Did              string           `json:"did,omitempty"`
	Phase            string           `json:"phase,omitempty"`
	Message          string           `json:"message,omitempty"`
	SecretsUrl       string           `json:"secretsUrl,omitempty"`
	SecretsFetchedAt string           `json:"secretsFetchedAt,omitempty"`
	Contract         *ContractSummary `json:"contract,omitempty"`
	KcpRunning       bool             `json:"kcpRunning"`
	CreatedAt        time.Time        `json:"createdAt"`
	UpdatedAt        time.Time        `json:"updatedAt"`
}

func WriteInvocation(inv Invocation) error {
	return fsx.WriteJSON(filepath.Join(Dir(inv.StateRoot, inv.Name), InvocationFile), inv, 0o600)
}

func ReadInvocation(stateRoot, name string) (Invocation, error) {
	var inv Invocation
	if err := CheckName(name); err != nil {
		return inv, err
	}
	if err := fsx.ReadJSON(filepath.Join(Dir(stateRoot, name), InvocationFile), &inv); err != nil {
		return inv, fmt.Errorf("request: no invocation %s under %s: %w", name, Root(stateRoot), err)
	}
	return inv, nil
}

func WriteSummary(stateRoot string, s Summary) error {
	s.UpdatedAt = time.Now().UTC()
	return fsx.WriteJSON(filepath.Join(Dir(stateRoot, s.Name), SummaryFile), s, 0o600)
}

func ReadSummary(stateRoot, name string) (Summary, error) {
	var s Summary
	err := fsx.ReadJSON(filepath.Join(Dir(stateRoot, name), SummaryFile), &s)
	return s, err
}

func List(stateRoot string) ([]Summary, error) {
	entries, err := os.ReadDir(Root(stateRoot))
	if errors.Is(err, os.ErrNotExist) {
		return []Summary{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Summary{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := ReadSummary(stateRoot, e.Name())
		if err != nil {
			inv, ierr := ReadInvocation(stateRoot, e.Name())
			if ierr != nil {
				continue
			}
			s = Summary{Name: inv.Name, Mode: inv.Mode, Network: inv.Network, Instance: inv.Instance, CreatedAt: inv.CreatedAt,
				Message: "no summary yet: the invocation did not finish writing one"}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func SummaryOf(c *v1alpha1.ComputeContract) *ContractSummary {
	if c == nil || c.Status.Contract == nil {
		return nil
	}
	k := c.Status.Contract
	s := &ContractSummary{Phase: string(k.Phase), VMAddress: k.VMAddress}
	for _, n := range k.Networks {
		s.Networks = append(s.Networks, n.Tag+"="+n.Address)
	}
	if k.Winner != nil {
		s.WinnerDid = k.Winner.DID
	}
	if k.Bids != nil {
		s.Bids = *k.Bids
	}
	if k.Receipt != nil {
		s.ReceiptUri = k.Receipt.URI
	}
	if k.HostKeys != nil {
		for _, h := range k.HostKeys.Keys {
			s.HostKeys = append(s.HostKeys, h.Type+" "+h.Fingerprint)
		}
	}
	if k.Release != nil {
		ok := k.Release.OK
		s.ReleaseOK = &ok
		if k.Release.Status != nil {
			s.ReleaseStatus = *k.Release.Status
		}
	}
	if k.Termination != nil {
		s.Termination = k.Termination.Reason
	}
	return s
}

type Kcp struct {
	rest      *rest.Config
	dyn       *kube.Client
	workspace string
}

func OpenKcp(kubeconfig, workspace string) (*Kcp, error) {
	cfg, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	workspace = cmp.Or(workspace, compose.DefaultWorkspace)
	root := rest.CopyConfig(cfg)
	root.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + workspace
	dc, err := kube.New(root)
	if err != nil {
		return nil, err
	}
	return &Kcp{rest: cfg, dyn: dc, workspace: workspace}, nil
}

func typeMeta(kind string) metav1.TypeMeta {
	return metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: kind}
}

func (k *Kcp) ApplyRoot(ctx context.Context, spec v1alpha1.MarketRequesterSpec) error {
	obj := &v1alpha1.MarketRequester{TypeMeta: typeMeta("MarketRequester"), ObjectMeta: metav1.ObjectMeta{Name: RootName}, Spec: spec}
	_, err := kube.Apply[kube.Doc](ctx, k.dyn.Resource(compose.MarketRequesterGVR), obj, kube.ApplyOptions{FieldManager: FieldManager, Force: true})
	return err
}

func (k *Kcp) Root(ctx context.Context) (v1alpha1.ApplicationStatus, error) {
	o, err := kube.Get[v1alpha1.MarketRequester](ctx, k.dyn.Resource(compose.MarketRequesterGVR), RootName)
	if err != nil {
		return v1alpha1.ApplicationStatus{}, err
	}
	return o.Status, nil
}

func (k *Kcp) WaitReady(ctx context.Context, timeout time.Duration, progress func(v1alpha1.ApplicationStatus)) error {
	last := ""
	var failed error
	err := await(ctx, k.dyn.Resource(compose.MarketRequesterGVR), compose.MarketRequesterGVR.GroupResource(), RootName, timeout, "waiting for the MarketRequester to be Ready",
		func(o *v1alpha1.MarketRequester) bool {
			st := o.Status
			if key := string(st.Phase) + st.Message; key != last && progress != nil {
				progress(st)
				last = key
			}
			if st.Phase == v1alpha1.ApplicationPhaseFailed {
				failed = fmt.Errorf("request: the MarketRequester failed: %s", st.Message)
				return true
			}
			if st.Phase != v1alpha1.ApplicationPhaseReady || len(st.Components) == 0 {
				return false
			}
			for _, c := range st.Components {
				if !c.Ready {
					return false
				}
			}
			return true
		})
	if err != nil {
		return err
	}
	return failed
}

func (k *Kcp) Contract(ctx context.Context) (*v1alpha1.ComputeContract, error) {
	return kube.Get[v1alpha1.ComputeContract](ctx, k.dyn.Resource(compose.ComputeContractGVR), marketrequester.RequesterContract)
}

func await[T any](ctx context.Context, r kube.Resource, gr schema.GroupResource, name string, timeout time.Duration, what string, done func(*T) bool) error {
	var missing error
	_, err := kube.Await(ctx, r, name, timeout, func(o *T) (bool, error) {
		if o == nil {
			missing = apierrors.NewNotFound(gr, name)
			return false, nil
		}
		return done(o), nil
	})
	return awaitErr(err, what, timeout, missing)
}

func awaitErr(err error, what string, timeout time.Duration, last error) error {
	if !errors.Is(err, kube.ErrAwaitTimeout) {
		return err
	}
	if last != nil {
		return fmt.Errorf("request: %s within %s: %w", what, timeout, last)
	}
	return fmt.Errorf("request: %s within %s", what, timeout)
}

func retry(ctx context.Context, deadline time.Time, timeout time.Duration, what string, try func() (bool, error)) error {
	delay := Poll
	var last error
	for {
		done, err := try()
		if done {
			return err
		}
		if err != nil {
			last = err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return awaitErr(kube.ErrAwaitTimeout, what, timeout, last)
		}
		t := time.NewTimer(min(delay, left))
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		delay = min(2*delay, RetryMax)
	}
}

func (k *Kcp) CreateContract(ctx context.Context, spec v1alpha1.ComputeContractSpec, timeout time.Duration) error {
	if spec.TTLSecondsAfterFinished == nil {
		ttl := ContractTTLSeconds
		spec.TTLSecondsAfterFinished = &ttl
	}
	body, err := kube.ApplyBody(&v1alpha1.ComputeContract{TypeMeta: typeMeta("ComputeContract"),
		ObjectMeta: metav1.ObjectMeta{Name: marketrequester.RequesterContract}, Spec: spec}, false)
	if err != nil {
		return err
	}
	return retry(ctx, time.Now().Add(timeout), timeout, "creating the ComputeContract (its schema is bound by the requester's core)", func() (bool, error) {
		_, err := kube.Create[kube.Doc](ctx, k.dyn.Resource(compose.ComputeContractGVR), body, metav1.CreateOptions{FieldManager: FieldManager})
		if err == nil || apierrors.IsAlreadyExists(err) {
			return true, nil
		}
		return false, err
	})
}

func (k *Kcp) Release(ctx context.Context) error {
	contracts := k.dyn.Resource(compose.ComputeContractGVR)
	if _, err := kube.Get[kube.Doc](ctx, contracts, marketrequester.RequesterContract); err != nil {
		return err
	}
	doc := kube.Doc{"apiVersion": v1alpha1.GroupVersion.String(), "kind": "ComputeContract",
		"metadata": map[string]any{"name": marketrequester.RequesterContract}, "spec": map[string]any{"release": true}}
	_, err := kube.Apply[kube.Doc](ctx, contracts, doc, kube.ApplyOptions{FieldManager: ReleaseFieldManager, Force: true})
	return err
}

func (k *Kcp) WaitContract(ctx context.Context, timeout time.Duration, done func(*v1alpha1.ComputeContract) bool,
	progress func(*v1alpha1.ComputeContract)) (*v1alpha1.ComputeContract, error) {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watch := supervise.New(wctx)
	defer func() { _ = watch.Stop() }()
	var mu sync.Mutex
	why := ""
	watch.Go("request: requester failure", func(ctx context.Context) error {
		_, _ = kube.Await(ctx, k.dyn.Resource(compose.MarketRequesterGVR), RootName, 0, func(o *v1alpha1.MarketRequester) (bool, error) {
			if o == nil {
				return false, nil
			}
			w := requesterFailure(o.Status)
			mu.Lock()
			why = w
			mu.Unlock()
			if w != "" {
				cancel()
			}
			return w != "", nil
		})
		return nil
	})
	failure := func() string {
		mu.Lock()
		defer mu.Unlock()
		return why
	}
	var cur *v1alpha1.ComputeContract
	var missing error
	last := ""
	_, err := kube.Await(wctx, k.dyn.Resource(compose.ComputeContractGVR), marketrequester.RequesterContract, timeout,
		func(c *v1alpha1.ComputeContract) (bool, error) {
			if c == nil {
				missing = apierrors.NewNotFound(compose.ComputeContractGVR.GroupResource(), marketrequester.RequesterContract)
				return false, nil
			}
			cur = c
			key := string(c.Status.Phase) + "|" + c.Status.Message
			if c.Status.Contract != nil {
				key += "|" + string(c.Status.Contract.Phase)
			}
			if key != last && progress != nil {
				progress(c)
				last = key
			}
			return done(c), nil
		})
	if cur != nil && done(cur) {
		return cur, nil
	}
	if w := failure(); w != "" && ctx.Err() == nil {
		return cur, fmt.Errorf("request: the requester application cannot carry the contract: %s", w)
	}
	return cur, awaitErr(err, "waiting for the ComputeContract", timeout, missing)
}

func requesterFailure(st v1alpha1.ApplicationStatus) string {
	if st.Phase == v1alpha1.ApplicationPhaseFailed {
		return "phase Failed: " + st.Message
	}
	for _, c := range st.Components {
		if c.Phase == string(v1alpha1.ComponentPhaseFailed) {
			return fmt.Sprintf("%s %s/%s is Failed", c.Kind, c.Workspace, c.Name)
		}
	}
	return ""
}

func (k *Kcp) PutSecrets(ctx context.Context, bundle []byte, timeout time.Duration) (commonv1.SecretRef, error) {
	ref := commonv1.SecretRef{Mount: marketrequester.RequesterVaultClient, Path: SecretsPath}
	what := "writing the secrets bundle into the requester's own OpenBao"
	deadline := time.Now().Add(timeout)
	ri := k.dyn.Resource(compose.OpenBaoServerGVR)
	server, client := marketrequester.RequesterVaultServer, marketrequester.RequesterVaultClient
	var pending error
	_, err := kube.Await(ctx, ri, server, timeout, func(o *v1alpha1.OpenBaoServer) (bool, error) {
		pending = vaultPending(o, server, client)
		return pending == nil, nil
	})
	if err != nil {
		return ref, awaitErr(err, what, timeout, pending)
	}
	err = retry(ctx, deadline, timeout, what, func() (bool, error) {
		c, _, refs, err := compose.ServerClient(ctx, ri, server, client)
		if err != nil {
			return false, err
		}
		defer c.RevokeSelf(context.WithoutCancel(ctx))
		ref.Mount = refs.KVMount
		if err := c.PutKV(ctx, refs.KVMount+"/data/"+SecretsPath, map[string]any{"bundle": string(bundle)}); err != nil {
			return false, err
		}
		return true, nil
	})
	return ref, err
}

func vaultPending(o *v1alpha1.OpenBaoServer, server, client string) error {
	if o == nil {
		return apierrors.NewNotFound(compose.OpenBaoServerGVR.GroupResource(), server)
	}
	st := o.Status
	if st.Address == "" || st.Refs == nil {
		return fmt.Errorf("the OpenBaoServer %s reports no address yet (phase %s)", server, st.Phase)
	}
	for _, x := range st.Clients {
		if x.Name == client && x.ClientCert != "" {
			return nil
		}
	}
	return fmt.Errorf("the OpenBaoServer %s reports no client %s yet", server, client)
}

func (k *Kcp) Identities() (*kcpclient.MarketIdentities, error) {
	c, err := kcpclient.New(kcpclient.Options{Host: k.rest.Host, RestConfig: k.rest})
	if err != nil {
		return nil, err
	}
	return c.MarketIdentities(k.workspace)
}

func (k *Kcp) WaitIdentityObject(ctx context.Context, timeout time.Duration) error {
	return await(ctx, k.dyn.Resource(compose.MarketIdentityGVR), compose.MarketIdentityGVR.GroupResource(), marketrequester.RequesterIdentity,
		timeout, "waiting for the requester's MarketIdentity (its core creates it)", func(*v1alpha1.MarketIdentity) bool { return true })
}

func (k *Kcp) AwaitIdentity(ctx context.Context, name string, each func(*v1alpha1.MarketIdentity) (bool, error)) (*v1alpha1.MarketIdentity, error) {
	return kube.Await(ctx, k.dyn.Resource(compose.MarketIdentityGVR), name, 0, each)
}

func (k *Kcp) AwaitContract(ctx context.Context, timeout time.Duration, done func(*v1alpha1.ComputeContract) bool) (*v1alpha1.ComputeContract, error) {
	return kube.Await(ctx, k.dyn.Resource(compose.ComputeContractGVR), marketrequester.RequesterContract, timeout,
		func(c *v1alpha1.ComputeContract) (bool, error) { return c != nil && done(c), nil })
}

func networkClient(kubeconfig string) (kube.Resource, error) {
	cfg, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return kube.Resource{}, err
	}
	cfg.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + compose.DefaultWorkspace
	dc, err := kube.New(cfg)
	if err != nil {
		return kube.Resource{}, err
	}
	return dc.Resource(compose.MarketNetworkGVR), nil
}

func networkApps(ctx context.Context, ri kube.Resource) ([]v1alpha1.NetworkApp, error) {
	o, err := kube.Get[v1alpha1.MarketNetwork](ctx, ri, marketnetwork.NetworkName)
	if err != nil {
		return nil, fmt.Errorf("request: the market network's MarketNetwork: %w", err)
	}
	return o.Spec.Apps, nil
}

func applyNetworkApp(ctx context.Context, ri kube.Resource, name string, app *v1alpha1.NetworkApp) error {
	spec := map[string]any{}
	if app != nil {
		entry, err := runtime.DefaultUnstructuredConverter.ToUnstructured(app)
		if err != nil {
			return err
		}
		spec["apps"] = []any{entry}
	}
	doc := kube.Doc{"apiVersion": v1alpha1.GroupVersion.String(), "kind": "MarketNetwork",
		"metadata": map[string]any{"name": marketnetwork.NetworkName}, "spec": spec}
	if _, err := kube.Apply[kube.Doc](ctx, ri, doc, kube.ApplyOptions{FieldManager: NetworkAppFieldManager + "/" + name}); err != nil {
		return fmt.Errorf("request: applying the market network's application %s: %w", name, err)
	}
	return nil
}

func AddToNetwork(ctx context.Context, kubeconfig string, app v1alpha1.NetworkApp) error {
	ri, err := networkClient(kubeconfig)
	if err != nil {
		return err
	}
	apps, err := networkApps(ctx, ri)
	if err != nil {
		return err
	}
	for _, a := range apps {
		if a.Name == app.Name {
			return fmt.Errorf("request: the market network already has an application %s", app.Name)
		}
	}
	return applyNetworkApp(ctx, ri, app.Name, &app)
}

func SetStopped(ctx context.Context, kubeconfig, name string, stopped bool) error {
	ri, err := networkClient(kubeconfig)
	if err != nil {
		return err
	}
	apps, err := networkApps(ctx, ri)
	if err != nil {
		return err
	}
	for _, a := range apps {
		if a.Name == name {
			a.Stopped = stopped
			return applyNetworkApp(ctx, ri, name, &a)
		}
	}
	return fmt.Errorf("request: the market network has no application %s", name)
}

func RemoveFromNetwork(ctx context.Context, kubeconfig, name string) error {
	ri, err := networkClient(kubeconfig)
	if err != nil {
		return err
	}
	if err := applyNetworkApp(ctx, ri, name, nil); err != nil {
		return err
	}
	apps, err := networkApps(ctx, ri)
	if err != nil {
		return err
	}
	for _, a := range apps {
		if a.Name == name {
			return fmt.Errorf("request: another writer of the MarketNetwork still applies its application %s", name)
		}
	}
	return nil
}

func NetworkApp(ctx context.Context, kubeconfig, name string) (v1alpha1.NetworkAppStatus, bool, error) {
	ri, err := networkClient(kubeconfig)
	if err != nil {
		return v1alpha1.NetworkAppStatus{}, false, err
	}
	o, err := kube.Get[v1alpha1.MarketNetwork](ctx, ri, marketnetwork.NetworkName)
	if err != nil {
		return v1alpha1.NetworkAppStatus{}, false, err
	}
	for _, a := range o.Status.Apps {
		if a.Name == name {
			return a, true, nil
		}
	}
	return v1alpha1.NetworkAppStatus{}, false, nil
}

func WaitNetworkApp(ctx context.Context, kubeconfig, name string, timeout time.Duration, done func(v1alpha1.NetworkAppStatus) bool,
	progress func(v1alpha1.NetworkAppStatus)) (v1alpha1.NetworkAppStatus, error) {
	ri, err := networkClient(kubeconfig)
	if err != nil {
		return v1alpha1.NetworkAppStatus{}, err
	}
	var cur v1alpha1.NetworkAppStatus
	var failed error
	last := ""
	err = await(ctx, ri, compose.MarketNetworkGVR.GroupResource(), marketnetwork.NetworkName, timeout, "waiting for the market network to place "+name,
		func(o *v1alpha1.MarketNetwork) bool {
			i := slices.IndexFunc(o.Status.Apps, func(a v1alpha1.NetworkAppStatus) bool { return a.Name == name })
			if i < 0 {
				return false
			}
			a := o.Status.Apps[i]
			cur = a
			if key := string(a.Phase) + a.Message; key != last && progress != nil {
				progress(a)
				last = key
			}
			if a.Phase == v1alpha1.ApplicationPhaseFailed {
				failed = fmt.Errorf("request: the market network failed %s: %s", name, a.Message)
				return true
			}
			return done(a)
		})
	if err != nil {
		return cur, err
	}
	return cur, failed
}

func WaitNetworkGone(ctx context.Context, kubeconfig, name string, timeout time.Duration) error {
	ri, err := networkClient(kubeconfig)
	if err != nil {
		return err
	}
	_, err = kube.Await(ctx, ri, marketnetwork.NetworkName, timeout, func(o *v1alpha1.MarketNetwork) (bool, error) {
		return o != nil && !slices.ContainsFunc(o.Status.Apps, func(a v1alpha1.NetworkAppStatus) bool { return a.Name == name }), nil
	})
	if errors.Is(err, kube.ErrAwaitTimeout) {
		return fmt.Errorf("the market network still lists %s", name)
	}
	return err
}

func HostKeyAlias(receiptURI string) string {
	clean := func(v string) string {
		v = strings.ToLower(v)
		var b strings.Builder
		dash := false
		for _, r := range v {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
				dash = false
				continue
			}
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
		return strings.Trim(b.String(), "-")
	}
	rest := strings.TrimPrefix(receiptURI, "at://")
	authority, _, _ := strings.Cut(rest, "/")
	rkey := receiptURI[strings.LastIndex(receiptURI, "/")+1:]
	return "swc-guest-" + clean(authority) + "-" + clean(rkey)
}

func Ticket(vmAddress string) (string, error) {
	if !strings.HasPrefix(vmAddress, irohScheme) {
		return "", fmt.Errorf("request: the guest address %q is not an iroh ticket; iroh is the only guest transport", vmAddress)
	}
	return strings.TrimPrefix(vmAddress, irohScheme), nil
}
