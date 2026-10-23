package bidder

import (
	"context"
	"errors"
	"fmt"
	"os/user"
	"regexp"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostcontract"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/install"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

const (
	RootName       = appctl.ChildRootName
	FieldManager   = "socialweb-computer"
	Poll           = time.Second
	PublicPlc      = "https://plc.directory"
	PublicIngress  = "xrpc.fedproxy.com"
	InstancePrefix = "b-"
	maxInstanceID  = 40
)

var (
	handleRE  = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	nonLabel  = regexp.MustCompile(`[^a-z0-9-]+`)
	ErrNoQrID = errors.New("the bidder became ready without a sign-in naming its account")
)

func CheckHandle(handle string) error {
	if !handleRE.MatchString(handle) {
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
	if name == "" {
		name = "bidder"
	}
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
}

func Spec(in SpecInput) (v1alpha1.MarketBidderSpec, error) {
	mode := in.Mode
	if mode == "" {
		mode = v1alpha1.MarketIdentityModeLocalRepo
	}
	if in.Profile == appctl.BidderProfileProdQr && in.Handle == "" {
		return v1alpha1.MarketBidderSpec{}, errors.New("bidder: profile prod-qr needs --handle: the QR sign-in is refused for any other account")
	}
	if in.Handle != "" {
		if err := CheckHandle(in.Handle); err != nil {
			return v1alpha1.MarketBidderSpec{}, err
		}
	}
	if in.Kcp == nil {
		return v1alpha1.MarketBidderSpec{}, errors.New("bidder: the bidder's provider is compute-provider-kcp and needs its guest image source (--guest-image-source)")
	}
	n := in.Network
	if n == nil {
		n = &v1alpha1.MarketNetworkValue{}
	}
	if n.PLCDirectoryURL == "" {
		n.PLCDirectoryURL = PublicPlc
	}
	if n.IngressProxyHost == "" {
		n.IngressProxyHost = PublicIngress
	}
	if n.RelayURLs == nil {
		n.RelayURLs = []string{}
	}
	if n.FirehoseURLs == nil {
		n.FirehoseURLs = []string{}
	}
	if in.Profile == appctl.BidderProfileNetwork && (in.PdsUrl == "" || n.CABundle == "" || n.GuestTLSURL() == "") {
		return v1alpha1.MarketBidderSpec{}, errors.New("bidder: profile market-network needs the market network's value and its bidders' PDS (--network, or --market-network-file)")
	}
	ingress := in.Ingress
	if ingress == nil && (in.Profile == appctl.BidderProfileProdQr || in.Profile == appctl.BidderProfileNetwork) {
		ingress = appctl.DefaultIngressConfig()
		if n.IngressProxyHost != "" {
			ingress.Xrpc.IngressProxyHost = n.IngressProxyHost
		}
	}
	return v1alpha1.MarketBidderSpec{Profile: in.Profile, Network: n, Ingress: ingress, Comms: in.Comms, Policy: in.Policy, Kcp: in.Kcp, Options: in.Options,
		Identity: v1alpha1.ApplicationIdentity{Mode: mode, Handle: in.Handle, PdsURL: in.PdsUrl}}, nil
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
	dyn  dynamic.Interface
}

func OpenKcp(kubeconfig string) (*Kcp, error) {
	cfg, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	root := rest.CopyConfig(cfg)
	root.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + appctl.DefaultWorkspace
	dc, err := dynamic.NewForConfig(root)
	if err != nil {
		return nil, err
	}
	return &Kcp{rest: cfg, dyn: dc}, nil
}

func (k *Kcp) ApplyRoot(ctx context.Context, spec v1alpha1.MarketBidderSpec) error {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&v1alpha1.MarketBidder{ObjectMeta: metav1.ObjectMeta{Name: RootName}, Spec: spec})
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(v1alpha1.GroupVersion.String())
	u.SetKind(string(v1alpha1.ApplicationKindMarketBidder))
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	body, err := u.MarshalJSON()
	if err != nil {
		return err
	}
	force := true
	_, err = k.dyn.Resource(appctl.MarketBidderGVR).Patch(ctx, RootName, types.ApplyPatchType, body,
		metav1.PatchOptions{FieldManager: FieldManager, Force: &force})
	return err
}

func (k *Kcp) Root(ctx context.Context) (*v1alpha1.MarketBidder, error) {
	u, err := k.dyn.Resource(appctl.MarketBidderGVR).Get(ctx, RootName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var o v1alpha1.MarketBidder
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

func (k *Kcp) Identities() (*kcpclient.MarketIdentities, error) {
	c, err := kcpclient.New(kcpclient.Options{Host: k.rest.Host, RestConfig: k.rest})
	if err != nil {
		return nil, err
	}
	return c.MarketIdentities(appctl.DefaultWorkspace)
}

func (k *Kcp) WaitIdentityObject(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := k.dyn.Resource(appctl.MarketIdentityGVR).Get(ctx, appctl.BidderIdentity, metav1.GetOptions{})
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bidder: the core created no MarketIdentity %s within %s: %w", appctl.BidderIdentity, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(Poll):
		}
	}
}

func (k *Kcp) WaitReady(ctx context.Context, timeout time.Duration, progress func(v1alpha1.ApplicationStatus)) (*v1alpha1.MarketBidder, error) {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		o, err := k.Root(ctx)
		if err == nil {
			if key := string(o.Status.Phase) + o.Status.Message; key != last && progress != nil {
				progress(o.Status)
				last = key
			}
			switch o.Status.Phase {
			case v1alpha1.ApplicationPhaseReady:
				return o, nil
			case v1alpha1.ApplicationPhaseFailed:
				return o, fmt.Errorf("bidder: the MarketBidder failed: %s", o.Status.Message)
			}
		}
		if time.Now().After(deadline) {
			return o, fmt.Errorf("bidder: the MarketBidder was not Ready within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return o, ctx.Err()
		case <-time.After(Poll):
		}
	}
}
