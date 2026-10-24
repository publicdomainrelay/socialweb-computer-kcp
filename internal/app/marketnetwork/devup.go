package marketnetwork

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"

	"github.com/johnandersen777/socialweb-computer-kcp/api/kcpgvr"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

var (
	apiBindingGVR  = kcpgvr.APIBindings
	networkBinding = compose.ExportApplications
)

const (
	NetworkName    = "network"
	BidderPrefix   = "bidder-"
	GatewayAppName = "gateway"
)

type networkProfile struct {
	Bidder  v1alpha1.NetworkApp `json:"bidder"`
	Gateway v1alpha1.NetworkApp `json:"gateway"`
}

func merge(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				merge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

func defaultNetworkValues() map[string]any {
	return map[string]any{
		"bidder": map[string]any{
			"kind":    string(v1alpha1.ApplicationKindMarketBidder),
			"profile": NetworkProfile,
			"policy":  "open",
			"kcp":     map[string]any{"guestImageSource": ""},
		},
		"gateway": map[string]any{
			"kind":    string(v1alpha1.ApplicationKindGateway),
			"profile": NetworkProfile,
		},
	}
}

func loadNetworkProfile(overlay []byte) (networkProfile, error) {
	m := defaultNetworkValues()
	if len(overlay) > 0 {
		var o map[string]any
		if err := yaml.Unmarshal(overlay, &o); err != nil {
			return networkProfile{}, fmt.Errorf("marketnetwork: app-values: %w", err)
		}
		merge(m, o)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return networkProfile{}, err
	}
	var p networkProfile
	if err := yaml.UnmarshalStrict(b, &p); err != nil {
		return networkProfile{}, fmt.Errorf("marketnetwork: the network profile values: %w", err)
	}
	return p, nil
}

type BidderVMDefaults struct {
	GuestImageSource string
	VmHostBackend    string
	Handle           string
	GuestNetwork     *v1alpha1.GuestNetworkOptions
}

func (d BidderVMDefaults) fill(a *v1alpha1.NetworkApp) {
	if a.Kind != v1alpha1.ApplicationKindMarketBidder {
		return
	}
	if a.Kcp == nil {
		a.Kcp = &v1alpha1.KcpProvider{}
	}
	a.Kcp.GuestImageSource = cmp.Or(a.Kcp.GuestImageSource, d.GuestImageSource)
	if d.VmHostBackend != "" {
		a.Kcp.VmHostBackend = d.VmHostBackend
	}
	if d.Handle != "" {
		a.Kcp.Handle = d.Handle
	}
	if a.GuestNetwork == nil && d.GuestNetwork != nil {
		g := *d.GuestNetwork
		a.GuestNetwork = &g
	}
}

func NetworkBidderApp(name string, overlay []byte, vm BidderVMDefaults) (v1alpha1.NetworkApp, error) {
	p, err := loadNetworkProfile(overlay)
	if err != nil {
		return v1alpha1.NetworkApp{}, err
	}
	a := p.Bidder
	a.Name = name
	vm.fill(&a)
	return a, nil
}

func NetworkSpec(label string, bidders int, withGateway bool, overlay []byte, vm BidderVMDefaults) (v1alpha1.MarketNetworkSpec, error) {
	if bidders < 0 {
		return v1alpha1.MarketNetworkSpec{}, fmt.Errorf("marketnetwork: bidders %d is negative", bidders)
	}
	p, err := loadNetworkProfile(overlay)
	if err != nil {
		return v1alpha1.MarketNetworkSpec{}, err
	}
	spec := v1alpha1.MarketNetworkSpec{Profile: NetworkProfile, GuestLabel: label}
	for i := 1; i <= bidders; i++ {
		a := p.Bidder
		if p.Bidder.Kcp != nil {
			k := *p.Bidder.Kcp
			a.Kcp = &k
		}
		a.Name = fmt.Sprintf("%s%d", BidderPrefix, i)
		vm.fill(&a)
		spec.Apps = append(spec.Apps, a)
	}
	if withGateway {
		g := p.Gateway
		g.Name = GatewayAppName
		spec.Apps = append(spec.Apps, g)
	}
	return spec, nil
}

func rootClient(kubeconfig string) (*kube.Client, error) {
	restCfg, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	return kube.ForWorkspace(restCfg, compose.DefaultWorkspace)
}

func networkClient(kubeconfig string) (kube.Resource, error) {
	dc, err := rootClient(kubeconfig)
	if err != nil {
		return kube.Resource{}, err
	}
	return dc.Resource(compose.MarketNetworkGVR), nil
}

func bindingReady(b *apisv1alpha2.APIBinding) (bool, error) {
	if b == nil {
		return false, nil
	}
	for _, c := range b.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			return true, nil
		}
	}
	return false, nil
}

func ApplyNetwork(ctx context.Context, kubeconfig string, spec v1alpha1.MarketNetworkSpec, timeout time.Duration) error {
	dc, err := rootClient(kubeconfig)
	if err != nil {
		return err
	}
	ri := dc.Resource(compose.MarketNetworkGVR)
	obj := &v1alpha1.MarketNetwork{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "MarketNetwork"},
		ObjectMeta: metav1.ObjectMeta{Name: NetworkName}, Spec: spec}
	if _, err := kube.Await(ctx, dc.Resource(apiBindingGVR), networkBinding, timeout, bindingReady); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("marketnetwork: applying the MarketNetwork (the core applies the applications set first): %w", err)
	}
	if _, err := kube.Apply[v1alpha1.MarketNetwork](ctx, ri, obj, kube.ApplyOptions{FieldManager: "socialweb-computer", Force: true}); err != nil {
		return fmt.Errorf("marketnetwork: applying the MarketNetwork (the core applies the applications set first): %w", err)
	}
	return nil
}

func ReadNetwork(ctx context.Context, kubeconfig string) (v1alpha1.MarketNetwork, error) {
	ri, err := networkClient(kubeconfig)
	if err != nil {
		return v1alpha1.MarketNetwork{}, err
	}
	o, err := kube.Get[v1alpha1.MarketNetwork](ctx, ri, NetworkName)
	if err != nil {
		return v1alpha1.MarketNetwork{}, err
	}
	return *o, nil
}

func ReadJoinedNetwork(ctx context.Context, kubeconfig string) (*v1alpha1.MarketNetworkValue, error) {
	dc, err := rootClient(kubeconfig)
	if err != nil {
		return nil, err
	}
	o, err := kube.Get[v1alpha1.MarketBidder](ctx, dc.Resource(compose.MarketBidderGVR), compose.ChildRootName)
	if err != nil {
		return nil, err
	}
	return o.Spec.Network, nil
}

func WaitNetwork(ctx context.Context, kubeconfig string, timeout time.Duration, progress func(v1alpha1.MarketNetworkStatus)) (v1alpha1.MarketNetwork, error) {
	ri, err := networkClient(kubeconfig)
	if err != nil {
		return v1alpha1.MarketNetwork{}, err
	}
	last := ""
	var o v1alpha1.MarketNetwork
	_, err = kube.Await(ctx, ri, NetworkName, timeout, func(cur *v1alpha1.MarketNetwork) (bool, error) {
		if cur == nil {
			return false, nil
		}
		o = *cur
		var key strings.Builder
		key.WriteString(string(o.Status.Phase) + o.Status.Message)
		for _, a := range o.Status.Apps {
			key.WriteString(a.Name + string(a.Phase))
		}
		if key.String() != last && progress != nil {
			progress(o.Status)
			last = key.String()
		}
		if o.Status.Phase == v1alpha1.ApplicationPhaseReady && o.Status.ObservedGeneration == o.Generation {
			return true, nil
		}
		if o.Status.Phase == v1alpha1.ApplicationPhaseFailed {
			return false, fmt.Errorf("marketnetwork: the MarketNetwork failed: %s", o.Status.Message)
		}
		if why := networkFailure(o.Status); why != "" {
			return false, fmt.Errorf("marketnetwork: the MarketNetwork cannot become Ready: %s", why)
		}
		return false, nil
	})
	if errors.Is(err, kube.ErrAwaitTimeout) {
		return o, fmt.Errorf("marketnetwork: the MarketNetwork was not Ready within %s (phase %s: %s)", timeout, o.Status.Phase, o.Status.Message)
	}
	return o, err
}

func DeleteNetwork(ctx context.Context, kubeconfig string, timeout time.Duration) error {
	ri, err := networkClient(kubeconfig)
	if err != nil {
		return err
	}
	if err := ri.Delete(ctx, NetworkName, metav1.DeleteOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	_, err = kube.Await(ctx, ri, NetworkName, timeout, kube.Absent[v1alpha1.MarketNetwork])
	if errors.Is(err, kube.ErrAwaitTimeout) {
		return fmt.Errorf("marketnetwork: the MarketNetwork was not gone within %s", timeout)
	}
	return err
}

func networkFailure(st v1alpha1.MarketNetworkStatus) string {
	var out []string
	failed := func(where string, cs []v1alpha1.ApplicationComponent) {
		for _, c := range cs {
			if c.Phase == string(v1alpha1.ComponentPhaseFailed) {
				out = append(out, fmt.Sprintf("%s %s %s/%s is Failed", where, c.Kind, c.Workspace, c.Name))
			}
		}
	}
	failed("the network's", st.Components)
	for _, a := range st.Apps {
		if a.Kind == v1alpha1.ApplicationKindMarketRequester || a.Kind == v1alpha1.ApplicationKindSshGateway {
			continue
		}
		if a.Phase == v1alpha1.ApplicationPhaseFailed {
			out = append(out, fmt.Sprintf("child application %s is Failed: %s", a.Name, a.Message))
		}
		failed("child application "+a.Name+"'s", a.Components)
	}
	return strings.Join(out, "; ")
}
