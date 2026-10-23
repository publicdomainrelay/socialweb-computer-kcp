package clusterprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/apiserverfront"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

const (
	FrontControlName = "front.sock"
	FrontStateName   = "front.json"
	frontSocketDir   = "fronts"
)

func FrontControlPath(stateDir, dataDir, cluster string) string {
	p := filepath.Join(dataDir, FrontControlName)
	if apiserverfront.CheckSocketPath(p) == nil {
		return p
	}
	sum := sha256.Sum256([]byte(stateDir + "\x00" + cluster))
	name := hex.EncodeToString(sum[:])[:16] + ".sock"
	if run := os.Getenv("XDG_RUNTIME_DIR"); run != "" {
		if q := filepath.Join(run, "swc", name); apiserverfront.CheckSocketPath(q) == nil {
			return q
		}
	}
	return filepath.Join(stateDir, frontSocketDir, name)
}

func FrontStatePath(dataDir string) string { return filepath.Join(dataDir, FrontStateName) }

func SlotBackend(spec clusterstack.Spec) string {
	return net.JoinHostPort(spec.BindAddress, strconv.Itoa(spec.K3sSecurePort))
}

func waitFront(ctx context.Context, control string) error {
	last := "no answer"
	c := apiserverfront.Client{Path: control}
	return poll(ctx, func(ctx context.Context) (bool, error) {
		st, err := c.Status(ctx)
		if err != nil {
			last = err.Error()
			return false, nil
		}
		if st.Address == "" {
			last = "the front reported no listen address"
			return false, nil
		}
		return true, nil
	}, func() error { return &errNotReady{what: "the apiserver front at " + control, last: last} })
}

func waitSlotUp(ctx context.Context, spec clusterstack.Spec) error {
	if err := waitK3s(ctx, spec.BindAddress, spec.K3sSecurePort); err != nil {
		return err
	}
	return waitK3sControllers(ctx, spec.ServerAdminKubeconfig())
}

func WaitSlot(ctx context.Context, spec clusterstack.Spec) error {
	if err := waitK3s(ctx, spec.BindAddress, spec.K3sSecurePort); err != nil {
		return err
	}
	if err := waitK3sReadyz(ctx, spec.ServerAdminKubeconfig()); err != nil {
		return err
	}
	return waitK3sControllers(ctx, spec.ServerAdminKubeconfig())
}

func slotClient(spec clusterstack.Spec) (kubernetes.Interface, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", spec.ServerAdminKubeconfig())
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(kcpserver.Throughput(cfg))
}

func CheckAdvertisedEndpoints(ctx context.Context, spec clusterstack.Spec) error {
	core, err := slotClient(spec)
	if err != nil {
		return err
	}
	want := net.JoinHostPort(spec.AdvertiseAddress, strconv.Itoa(spec.AdvertisePort))
	last := "no answer"
	err = awaitTyped(ctx, apiserverSlices(core), &discoveryv1.EndpointSlice{}, 0, func(slices []*discoveryv1.EndpointSlice) (bool, error) {
		got := endpointsOf(slices)
		if len(got) == 1 && got[0] == want {
			return true, nil
		}
		last = fmt.Sprintf("the kubernetes EndpointSlice names %v", got)
		return false, nil
	})
	if err != nil && ctx.Err() != nil {
		return &errNotReady{what: "the kubernetes EndpointSlice of " + spec.Cluster + " naming only " + want +
			", the advertise address and the front port; a slot listen port there is a port a guest cannot reach after the next swap",
			last: last}
	}
	return err
}

var apiserverSliceSelector = discoveryv1.LabelServiceName + "=kubernetes"

func apiserverSlices(core kubernetes.Interface) cache.ListerWatcher {
	return &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (kruntime.Object, error) {
			o.LabelSelector = apiserverSliceSelector
			return core.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault).List(ctx, o)
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			o.LabelSelector = apiserverSliceSelector
			return core.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault).Watch(ctx, o)
		},
	}
}

func AdvertisedEndpoints(ctx context.Context, core kubernetes.Interface) ([]string, error) {
	list, err := core.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault).List(ctx,
		metav1.ListOptions{LabelSelector: apiserverSliceSelector})
	if err != nil {
		return nil, err
	}
	items := make([]*discoveryv1.EndpointSlice, 0, len(list.Items))
	for i := range list.Items {
		items = append(items, &list.Items[i])
	}
	return endpointsOf(items), nil
}

func endpointsOf(slices []*discoveryv1.EndpointSlice) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range slices {
		for _, p := range s.Ports {
			if p.Port == nil {
				continue
			}
			for _, e := range s.Endpoints {
				for _, a := range e.Addresses {
					hp := net.JoinHostPort(a, strconv.Itoa(int(*p.Port)))
					if !seen[hp] {
						seen[hp] = true
						out = append(out, hp)
					}
				}
			}
		}
	}
	return out
}
