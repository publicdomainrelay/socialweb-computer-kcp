package appctl

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/clock"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/kcprest"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/unstruct"
)

const (
	NetworkName    = "network"
	NetworkPoll    = time.Second
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
			return networkProfile{}, fmt.Errorf("appctl: app-values: %w", err)
		}
		merge(m, o)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return networkProfile{}, err
	}
	var p networkProfile
	if err := yaml.UnmarshalStrict(b, &p); err != nil {
		return networkProfile{}, fmt.Errorf("appctl: the network profile values: %w", err)
	}
	return p, nil
}

type BidderVMDefaults struct {
	GuestImageSource string
	VmHostBackend    string
	Handle           string
}

func (d BidderVMDefaults) fill(a *v1alpha1.NetworkApp) {
	if a.Kind != v1alpha1.ApplicationKindMarketBidder {
		return
	}
	if a.Kcp == nil {
		a.Kcp = &v1alpha1.KcpProvider{}
	}
	if a.Kcp.GuestImageSource == "" {
		a.Kcp.GuestImageSource = d.GuestImageSource
	}
	if d.VmHostBackend != "" {
		a.Kcp.VmHostBackend = d.VmHostBackend
	}
	if d.Handle != "" {
		a.Kcp.Handle = d.Handle
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
		return v1alpha1.MarketNetworkSpec{}, fmt.Errorf("appctl: bidders %d is negative", bidders)
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

func networkClient(kubeconfig string) (dynamic.ResourceInterface, error) {
	restCfg, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	dc, err := dynamic.NewForConfig(kcprest.ForCluster(restCfg, DefaultWorkspace))
	if err != nil {
		return nil, err
	}
	return dc.Resource(MarketNetworkGVR), nil
}

func ApplyNetwork(ctx context.Context, kubeconfig string, spec v1alpha1.MarketNetworkSpec, timeout time.Duration) error {
	ri, err := networkClient(kubeconfig)
	if err != nil {
		return err
	}
	u, err := unstruct.To(&v1alpha1.MarketNetwork{ObjectMeta: metav1.ObjectMeta{Name: NetworkName}, Spec: spec}, "MarketNetwork")
	if err != nil {
		return err
	}
	delete(u.Object, "status")
	delete(u.Object["metadata"].(map[string]any), "creationTimestamp")
	body, err := u.MarshalJSON()
	if err != nil {
		return err
	}
	force := true
	deadline := time.Now().Add(timeout)
	for {
		_, err := ri.Patch(ctx, NetworkName, types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: "socialweb-computer", Force: &force})
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("appctl: applying the MarketNetwork (the core applies the applications set first): %w", err)
		}
		if !clock.Sleep(ctx, NetworkPoll) {
			return ctx.Err()
		}
	}
}

func ReadNetwork(ctx context.Context, kubeconfig string) (v1alpha1.MarketNetwork, error) {
	ri, err := networkClient(kubeconfig)
	if err != nil {
		return v1alpha1.MarketNetwork{}, err
	}
	u, err := ri.Get(ctx, NetworkName, metav1.GetOptions{})
	if err != nil {
		return v1alpha1.MarketNetwork{}, err
	}
	var o v1alpha1.MarketNetwork
	return o, unstruct.Into(u, &o)
}

func WaitNetwork(ctx context.Context, kubeconfig string, timeout time.Duration, progress func(v1alpha1.MarketNetworkStatus)) (v1alpha1.MarketNetwork, error) {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		o, err := ReadNetwork(ctx, kubeconfig)
		if err == nil {
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
				return o, nil
			}
			if o.Status.Phase == v1alpha1.ApplicationPhaseFailed {
				return o, fmt.Errorf("appctl: the MarketNetwork failed: %s", o.Status.Message)
			}
			if why := networkFailure(o.Status); why != "" {
				return o, fmt.Errorf("appctl: the MarketNetwork cannot become Ready: %s", why)
			}
		}
		if time.Now().After(deadline) {
			return o, fmt.Errorf("appctl: the MarketNetwork was not Ready within %s (phase %s: %s)", timeout, o.Status.Phase, o.Status.Message)
		}
		if !clock.Sleep(ctx, NetworkPoll) {
			return o, ctx.Err()
		}
	}
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
	deadline := time.Now().Add(timeout)
	for {
		_, err := ri.Get(ctx, NetworkName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("appctl: the MarketNetwork was not gone within %s", timeout)
		}
		if !clock.Sleep(ctx, NetworkPoll) {
			return ctx.Err()
		}
	}
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
