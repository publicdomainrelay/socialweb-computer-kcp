package gatewayssh

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/sshgateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

const (
	RootName     = "app"
	FieldManager = "run-gateway-ssh"
	Poll         = time.Second
	DefaultName  = "ssh-gateway"
)

type SpecInput struct {
	Profile           string
	Network           *v1alpha1.MarketNetworkValue
	BidderDids        []string
	Options           []v1alpha1.ApplicationOption
	Comms             *v1alpha1.MarketComms
	RequesterIngress  *v1alpha1.IngressConfig
	OwnerSessionsFile string
}

func Spec(in SpecInput) (v1alpha1.SshGatewaySpec, error) {
	if !slices.Contains(sshgateway.SshGatewayProfiles, in.Profile) {
		return v1alpha1.SshGatewaySpec{}, fmt.Errorf("gatewayssh: --profile %q: want one of %s", in.Profile, strings.Join(sshgateway.SshGatewayProfiles, ", "))
	}
	if in.Profile == sshgateway.SshGatewayProfileNetwork && in.Network == nil {
		return v1alpha1.SshGatewaySpec{}, fmt.Errorf("gatewayssh: the %s profile takes its network value from a market network (--network)", in.Profile)
	}
	return v1alpha1.SshGatewaySpec{Profile: in.Profile, Network: in.Network, BidderDids: in.BidderDids, Comms: in.Comms, Options: in.Options,
		RequesterIngress: in.RequesterIngress, OwnerSessionsFile: in.OwnerSessionsFile}, nil
}

func InstanceID(name string) string {
	return "ssh-gateway-" + name
}

type Kcp struct {
	dyn *kube.Client
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
	return &Kcp{dyn: dc}, nil
}

func (k *Kcp) ApplyRoot(ctx context.Context, spec v1alpha1.SshGatewaySpec) error {
	if err := compose.AwaitBound(ctx, k.dyn.Resource(compose.APIBindingGVR), compose.RootBindTimeout, compose.RootExports...); err != nil {
		return err
	}
	obj := &v1alpha1.SshGateway{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: string(v1alpha1.ApplicationKindSshGateway)},
		ObjectMeta: metav1.ObjectMeta{Name: RootName}, Spec: spec}
	_, err := kube.Apply[kube.Doc](ctx, k.dyn.Resource(compose.SshGatewayGVR), obj, kube.ApplyOptions{FieldManager: FieldManager, Force: true})
	return err
}

func (k *Kcp) Root(ctx context.Context) (*v1alpha1.SshGateway, error) {
	return kube.Get[v1alpha1.SshGateway](ctx, k.dyn.Resource(compose.SshGatewayGVR), RootName)
}

func (k *Kcp) WaitReady(ctx context.Context, timeout time.Duration, progress func(v1alpha1.ApplicationStatus)) (*v1alpha1.SshGateway, error) {
	last := ""
	o, err := kube.Await(ctx, k.dyn.Resource(compose.SshGatewayGVR), RootName, timeout, func(o *v1alpha1.SshGateway) (bool, error) {
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
		return o, fmt.Errorf("gatewayssh: the SshGateway was not Ready within %s", timeout)
	case err != nil:
		return o, err
	case o.Status.Phase == v1alpha1.ApplicationPhaseFailed:
		return o, fmt.Errorf("gatewayssh: the SshGateway failed: %s", o.Status.Message)
	}
	return o, nil
}

func SshListener(ls []v1alpha1.ApplicationListener) string {
	for _, l := range ls {
		if !strings.HasSuffix(l.Component, "-ssh-local") || l.Address == "" {
			continue
		}
		if strings.Contains(l.Address, "://") {
			return l.Address
		}
		return "ssh://" + l.Address
	}
	return ""
}
