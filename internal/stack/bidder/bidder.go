package bidder

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os/user"
	"regexp"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketbidder"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostcontract"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/install"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

const (
	RootName       = compose.ChildRootName
	FieldManager   = "socialweb-computer"
	Poll           = time.Second
	PublicPlc      = "https://plc.directory"
	PublicIngress  = "xrpc.fedproxy.com"
	InstancePrefix = "b-"
	maxInstanceID  = 40
)

var (
	handleRE  = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]([a-z0-9-]*[a-z0-9])?$`)
	nonLabel  = regexp.MustCompile(`[^a-z0-9-]+`)
	ErrNoQrID = errors.New("the bidder became ready without a sign-in naming its account")
)

func CheckHandle(handle string) error {
	if !handleRE.MatchString(handle) || slices.ContainsFunc(strings.Split(handle, "."), func(l string) bool { return len(l) > 63 }) {
		return fmt.Errorf("bidder: handle %q is not a lowercase atproto handle", handle)
	}
	return nil
}

func InstanceID(handle string) string {
	id := InstancePrefix + strings.Trim(nonLabel.ReplaceAllString(strings.ReplaceAll(strings.ToLower(handle), ".", "-"), "-"), "-")
	if len(id) > maxInstanceID {
		id = strings.TrimRight(id[:maxInstanceID], "-")
	}
	return id
}

func AppName(handle string) string {
	first, _, _ := strings.Cut(strings.ToLower(handle), ".")
	name := strings.Trim(nonLabel.ReplaceAllString(first, "-"), "-")
	name = cmp.Or(name, "bidder")
	return name
}

type SpecInput struct {
	Profile string
	Handle  string
	Policy  string
	Network *v1alpha1.MarketNetworkValue
	PdsUrl  string
	Mode    v1alpha1.MarketIdentityMode
	Kcp     *v1alpha1.KcpProvider
	Ingress *v1alpha1.IngressConfig
	Comms   *v1alpha1.MarketComms
	Options []v1alpha1.ApplicationOption

	GuestNetwork *v1alpha1.GuestNetworkOptions

	ComputeKinds []string
}

func Spec(in SpecInput) (v1alpha1.MarketBidderSpec, error) {
	mode := in.Mode
	mode = cmp.Or(mode, v1alpha1.MarketIdentityModeLocalRepo)
	if in.Profile == marketbidder.BidderProfileProdQr && in.Handle == "" {
		return v1alpha1.MarketBidderSpec{}, errors.New("bidder: profile prod-qr needs --handle: the QR sign-in is refused for any other account")
	}
	if in.Handle != "" {
		if err := CheckHandle(in.Handle); err != nil {
			return v1alpha1.MarketBidderSpec{}, err
		}
	}
	kinds := computeKinds(in.ComputeKinds)
	if in.Kcp == nil && slices.Contains(kinds, string(v1alpha1.ComputeKindVM)) {
		return v1alpha1.MarketBidderSpec{}, errors.New("bidder: the bidder's provider is compute-provider-kcp and needs its guest image source (--guest-image-source)")
	}
	n := in.Network
	if n == nil {
		n = &v1alpha1.MarketNetworkValue{}
	}
	n.PLCDirectoryURL = cmp.Or(n.PLCDirectoryURL, PublicPlc)
	n.IngressProxyHost = cmp.Or(n.IngressProxyHost, PublicIngress)
	if in.Profile == marketbidder.BidderProfileNetwork && (in.PdsUrl == "" || n.CABundle == "" || n.GuestTLSURL() == "") {
		return v1alpha1.MarketBidderSpec{}, errors.New("bidder: profile market-network needs the market network's value and its bidders' PDS (--network, or --market-network-file)")
	}
	ingress := in.Ingress
	if ingress == nil && (in.Profile == marketbidder.BidderProfileProdQr || in.Profile == marketbidder.BidderProfileNetwork) {
		ingress = compose.DefaultIngressConfig()
		if n.IngressProxyHost != "" {
			ingress.Xrpc.IngressProxyHost = n.IngressProxyHost
		}
	}
	return v1alpha1.MarketBidderSpec{Profile: in.Profile, Network: n, Ingress: ingress, Comms: in.Comms, Policy: in.Policy, Kcp: in.Kcp, Options: in.Options,
		GuestNetwork: in.GuestNetwork, ComputeKinds: kinds, Identity: v1alpha1.ApplicationIdentity{Mode: mode, Handle: in.Handle, PdsURL: in.PdsUrl}}, nil
}

func computeKinds(in []string) []string {
	var out []string
	for _, k := range in {
		switch k = strings.TrimSpace(k); k {
		case string(v1alpha1.ComputeKindVM), string(v1alpha1.ComputeKindDeno):
			if !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	if len(out) == 0 {
		return []string{string(v1alpha1.ComputeKindVM)}
	}
	return out
}

func CheckSignIn(spec v1alpha1.MarketBidderSpec, id *v1alpha1.MarketIdentity) error {
	if spec.Identity.Mode != v1alpha1.MarketIdentityModeOAuthQr || id == nil || id.Status.Phase != v1alpha1.MarketIdentityPhaseReady {
		return nil
	}
	want := strings.ToLower(spec.Identity.Handle)
	got := strings.ToLower(id.Status.Handle)
	if got == "" {
		return ErrNoQrID
	}
	if want != "" && got != want {
		return fmt.Errorf("the session belongs to %s; scan the QR with %s", got, want)
	}
	return nil
}

func CheckAccount(backend hostsvc.Backend, handle, root string) error {
	if backend != hostsvc.BackendSystemdUser {
		return nil
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	rec, err := install.ReadRecord(root, u.Username)
	if err != nil {
		return fmt.Errorf("bidder: host-backend=systemd-user runs only as an account install bidder made, and %s has no install record: %w", u.Username, err)
	}
	if rec.Role != hostcontract.RoleBidder || rec.Account != u.Username {
		return fmt.Errorf("bidder: host-backend=systemd-user: %s was installed as %s %s, not as a bidder account", u.Username, rec.Role, rec.Account)
	}
	if slices.Contains(rec.Handles, handle) {
		return nil
	}
	return fmt.Errorf("bidder: install bidder did not install handle %s (installed: %s)", handle, strings.Join(rec.Handles, ", "))
}

type Kcp struct {
	rest *rest.Config
	dyn  *kube.Client
}

func OpenKcp(kubeconfig string) (*Kcp, error) {
	cfg, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	root := rest.CopyConfig(cfg)
	root.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + compose.DefaultWorkspace
	dc, err := kube.New(root)
	if err != nil {
		return nil, err
	}
	return &Kcp{rest: cfg, dyn: dc}, nil
}

func (k *Kcp) ApplyRoot(ctx context.Context, spec v1alpha1.MarketBidderSpec) error {
	if err := compose.AwaitBound(ctx, k.dyn.Resource(compose.APIBindingGVR), compose.RootBindTimeout, compose.RootExports...); err != nil {
		return err
	}
	obj := &v1alpha1.MarketBidder{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: string(v1alpha1.ApplicationKindMarketBidder)},
		ObjectMeta: metav1.ObjectMeta{Name: RootName}, Spec: spec}
	_, err := kube.Apply[kube.Doc](ctx, k.dyn.Resource(compose.MarketBidderGVR), obj, kube.ApplyOptions{FieldManager: FieldManager, Force: true})
	return err
}

func (k *Kcp) Root(ctx context.Context) (*v1alpha1.MarketBidder, error) {
	return kube.Get[v1alpha1.MarketBidder](ctx, k.dyn.Resource(compose.MarketBidderGVR), RootName)
}

func (k *Kcp) Identities() (*kcpclient.MarketIdentities, error) {
	c, err := kcpclient.New(kcpclient.Options{Host: k.rest.Host, RestConfig: k.rest})
	if err != nil {
		return nil, err
	}
	return c.MarketIdentities(compose.DefaultWorkspace)
}

func (k *Kcp) WaitIdentityObject(ctx context.Context, timeout time.Duration) error {
	_, err := kube.Await(ctx, k.dyn.Resource(compose.MarketIdentityGVR), marketbidder.BidderIdentity, timeout, kube.Present[kube.Doc])
	if errors.Is(err, kube.ErrAwaitTimeout) {
		missing := apierrors.NewNotFound(compose.MarketIdentityGVR.GroupResource(), marketbidder.BidderIdentity)
		return fmt.Errorf("bidder: the core created no MarketIdentity %s within %s: %w", marketbidder.BidderIdentity, timeout, missing)
	}
	return err
}

func (k *Kcp) WaitReady(ctx context.Context, timeout time.Duration, progress func(v1alpha1.ApplicationStatus)) (*v1alpha1.MarketBidder, error) {
	last := ""
	o, err := kube.Await(ctx, k.dyn.Resource(compose.MarketBidderGVR), RootName, timeout, func(o *v1alpha1.MarketBidder) (bool, error) {
		if o == nil {
			return false, nil
		}
		if key := string(o.Status.Phase) + o.Status.Message; key != last && progress != nil {
			progress(o.Status)
			last = key
		}
		return o.Status.Phase == v1alpha1.ApplicationPhaseReady || o.Status.Phase == v1alpha1.ApplicationPhaseFailed, nil
	})
	switch {
	case errors.Is(err, kube.ErrAwaitTimeout):
		return o, fmt.Errorf("bidder: the MarketBidder was not Ready within %s", timeout)
	case err != nil:
		return o, err
	case o.Status.Phase == v1alpha1.ApplicationPhaseFailed:
		return o, fmt.Errorf("bidder: the MarketBidder failed: %s", o.Status.Message)
	}
	return o, nil
}

func (k *Kcp) AwaitIdentity(ctx context.Context, name string, each func(*v1alpha1.MarketIdentity) (bool, error)) (*v1alpha1.MarketIdentity, error) {
	return kube.Await(ctx, k.dyn.Resource(compose.MarketIdentityGVR), name, 0, each)
}
