package gatewayssh

import (
	"context"
	"fmt"
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
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/sshgateway"
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
		OwnerSessionsFile: in.OwnerSessionsFile}, nil
}

func InstanceID(name string) string {
	return "ssh-gateway-" + name
}

type Kcp struct {
	dyn dynamic.Interface
}

func OpenKcp(kubeconfig string) (*Kcp, error) {
	cfg, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	root := rest.CopyConfig(cfg)
	root.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + compose.DefaultWorkspace
	dc, err := dynamic.NewForConfig(root)
	if err != nil {
		return nil, err
	}
	return &Kcp{dyn: dc}, nil
}

func (k *Kcp) ApplyRoot(ctx context.Context, spec v1alpha1.SshGatewaySpec) error {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&v1alpha1.SshGateway{ObjectMeta: metav1.ObjectMeta{Name: RootName}, Spec: spec})
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(v1alpha1.GroupVersion.String())
	u.SetKind(string(v1alpha1.ApplicationKindSshGateway))
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	body, err := u.MarshalJSON()
	if err != nil {
		return err
	}
	force := true
	_, err = k.dyn.Resource(compose.SshGatewayGVR).Patch(ctx, RootName, types.ApplyPatchType, body,
		metav1.PatchOptions{FieldManager: FieldManager, Force: &force})
	return err
}

func (k *Kcp) Root(ctx context.Context) (*v1alpha1.SshGateway, error) {
	u, err := k.dyn.Resource(compose.SshGatewayGVR).Get(ctx, RootName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var o v1alpha1.SshGateway
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

func (k *Kcp) WaitReady(ctx context.Context, timeout time.Duration, progress func(v1alpha1.ApplicationStatus)) (*v1alpha1.SshGateway, error) {
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
				return o, fmt.Errorf("gatewayssh: the SshGateway failed: %s", o.Status.Message)
			}
		}
		if time.Now().After(deadline) {
			return o, fmt.Errorf("gatewayssh: the SshGateway was not Ready within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return o, ctx.Err()
		case <-time.After(Poll):
		}
	}
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
