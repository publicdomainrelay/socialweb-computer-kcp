//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
)

var componentLinkGVR = schema.GroupVersionResource{Group: "exposure.socialweb.computer", Version: "v1alpha1", Resource: "componentlinks"}

func TestComponentLinkRegistryEnforcesWorkspaceAccess(t *testing.T) {
	reg := component.NewHandlerRegistry()
	defer reg.Close()

	beta := component.Key{Workspace: "root:beta", Kind: "Pds", Name: "pds"}
	alphaPds := component.Key{Workspace: "root:alpha", Kind: "Pds", Name: "pds"}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if err := reg.RegisterHTTP(beta, handler, "http"); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterStream(beta, func(ctx context.Context, conn net.Conn) error {
		_, err := io.Copy(io.Discard, conn)
		return err
	}, "http"); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterHTTP(alphaPds, handler, "http"); err != nil {
		t.Fatal(err)
	}

	address := kcp.ServiceFQDN("pds", "default", beta.Workspace, kcp.DefaultServiceDomain)
	alpha := component.Key{Workspace: "root:alpha", Kind: "Bidder", Name: "bidder"}
	same := component.Key{Workspace: "root:beta", Kind: "Bidder", Name: "bidder"}

	call := func(ctx context.Context) (int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/", nil)
		if err != nil {
			t.Fatalf("building the handler request: %v", err)
		}
		resp, err := reg.Client().Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			return 0, err
		}
		return resp.StatusCode, nil
	}

	status, err := call(context.Background())
	requireLinkDenied(t, "no caller", err)
	_ = status

	status, err = call(component.WithCaller(context.Background(), alpha))
	requireLinkDenied(t, "cross-workspace caller with no link", err)
	_ = status

	status, err = call(component.WithCaller(context.Background(), same))
	requireHandlerStatus(t, "same-workspace caller with no link", status, err, http.StatusNoContent)

	status, err = call(component.WithSystemCaller(context.Background()))
	requireHandlerStatus(t, "system caller", status, err, http.StatusNoContent)

	exact := component.AccessLink{FromWorkspace: "root:alpha", FromKind: "Bidder", FromName: "bidder",
		ToWorkspace: "root:beta", ToKind: "Pds", ToName: "pds"}
	reg.SetLinks([]component.AccessLink{exact})
	status, err = call(component.WithCaller(context.Background(), alpha))
	requireHandlerStatus(t, "cross-workspace caller with an unrestricted link", status, err, http.StatusNoContent)

	other := exact
	other.Handlers = []string{"other"}
	reg.SetLinks([]component.AccessLink{other})
	status, err = call(component.WithCaller(context.Background(), alpha))
	requireLinkDenied(t, "cross-workspace caller with a link for another handler", err)
	_ = status

	httpOnly := exact
	httpOnly.Handlers = []string{"http"}
	reg.SetLinks([]component.AccessLink{httpOnly})
	status, err = call(component.WithCaller(context.Background(), alpha))
	requireHandlerStatus(t, "cross-workspace caller with a link for the http handler", status, err, http.StatusNoContent)

	wildcard := component.AccessLink{FromWorkspace: "root:alpha", ToWorkspace: "root:beta"}
	reg.SetLinks([]component.AccessLink{wildcard})
	status, err = call(component.WithCaller(context.Background(), alpha))
	requireHandlerStatus(t, "cross-workspace caller with a workspace-only link", status, err, http.StatusNoContent)

	mismatch := wildcard
	mismatch.FromName = "someone-else"
	reg.SetLinks([]component.AccessLink{mismatch})
	status, err = call(component.WithCaller(context.Background(), alpha))
	requireLinkDenied(t, "cross-workspace caller with a mismatched from name", err)
	_ = status

	streamCtx := component.WithCaller(context.Background(), alpha)
	reg.SetLinks(nil)
	conn, err := reg.DialStream(streamCtx, beta, "http")
	if err == nil {
		conn.Close()
		t.Fatal("cross-workspace stream caller with no link: the stream was allowed")
	}
	requireLinkDenied(t, "cross-workspace stream caller with no link", err)

	reg.SetLinks([]component.AccessLink{exact})
	conn, err = reg.DialStream(streamCtx, beta, "http")
	if err != nil {
		t.Fatalf("cross-workspace stream caller with a matching link: %v", err)
	}
	conn.Close()

	streamOther := exact
	streamOther.Handlers = []string{"other"}
	reg.SetLinks([]component.AccessLink{streamOther})
	conn, err = reg.DialStream(streamCtx, beta, "http")
	if err == nil {
		conn.Close()
		t.Fatal("cross-workspace stream caller with a link for another handler: the stream was allowed")
	}
	requireLinkDenied(t, "cross-workspace stream caller with a link for another handler", err)

	reg.SetLinks([]component.AccessLink{exact})
	live, err := reg.DialStream(streamCtx, beta, "http")
	if err != nil {
		t.Fatalf("dialing the live stream: %v", err)
	}
	reg.SetLinks(nil)
	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := live.Read(buf)
		readErr <- err
	}()
	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("SetLinks(nil) left the tracked stream connection readable")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SetLinks(nil) did not close the tracked stream connection")
	}
	status, err = call(component.WithCaller(context.Background(), alpha))
	requireLinkDenied(t, "cross-workspace HTTP caller after SetLinks(nil)", err)
	_ = status
	conn, err = reg.DialStream(streamCtx, beta, "http")
	if err == nil {
		conn.Close()
		t.Fatal("cross-workspace stream caller after SetLinks(nil): the stream was allowed")
	}
	requireLinkDenied(t, "cross-workspace stream caller after SetLinks(nil)", err)
}

func TestComponentLinksAreReconciledOnALiveMarketNetwork(t *testing.T) {
	n := StartNetwork(t, NetworkOptions{Bidders: 1, WithGateway: false, UpTimeout: 5 * time.Minute})

	st, err := n.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Network == nil {
		t.Fatalf("dev status reports no MarketNetwork: %s", st.NetworkError)
	}
	status := st.Network.Status

	workspaceSet := map[string]bool{}
	for _, app := range status.Apps {
		for _, c := range app.Components {
			if ws := strings.TrimSpace(c.Workspace); ws != "" {
				workspaceSet[ws] = true
			}
		}
	}
	if len(workspaceSet) == 0 {
		t.Fatalf("the MarketNetwork status names no application component workspace: %+v", status.Apps)
	}
	workspaces := make([]string, 0, len(workspaceSet))
	for ws := range workspaceSet {
		workspaces = append(workspaces, ws)
	}
	sort.Strings(workspaces)

	linksByWorkspace := map[string][]exposurev1alpha1.ComponentLink{}
	for _, ws := range workspaces {
		ri := n.componentLinkClient(t, ws).Resource(componentLinkGVR)
		linksByWorkspace[ws] = waitWorkspaceComponentLinks(t, n, ri, ws, 3*time.Minute)
	}

	bidderWorkspace, bidderName, policyEngine := bidderComponentOf(t, status)
	bidderLinks, ok := linksByWorkspace[bidderWorkspace]
	if !ok {
		t.Fatalf("the bidder workspace %s is not in the application component workspaces %v", bidderWorkspace, workspaces)
	}
	var toPds, toPolicyEngine bool
	for _, link := range bidderLinks {
		if strings.EqualFold(link.Spec.From.Kind, "Bidder") && strings.EqualFold(link.Spec.From.Name, bidderName) &&
			strings.EqualFold(link.Spec.To.Kind, "Pds") {
			toPds = true
		}
		if strings.EqualFold(link.Spec.To.Kind, "PolicyEngine") {
			toPolicyEngine = true
		}
	}
	if !toPds {
		t.Fatalf("the bidder workspace %s has no ComponentLink from Bidder/%s to a Pds: %+v", bidderWorkspace, bidderName, bidderLinks)
	}
	if policyEngine != "" && !toPolicyEngine {
		t.Fatalf("the bidder workspace %s has a PolicyEngine/%s but no ComponentLink to a PolicyEngine: %+v", bidderWorkspace, policyEngine, bidderLinks)
	}

	var pick exposurev1alpha1.ComponentLink
	found := false
	for i := range bidderLinks {
		link := bidderLinks[i]
		if strings.EqualFold(link.Spec.From.Kind, "Bidder") && strings.EqualFold(link.Spec.From.Name, bidderName) &&
			strings.EqualFold(link.Spec.To.Kind, "Pds") {
			pick, found = link, true
			break
		}
	}
	if !found {
		t.Fatalf("the bidder workspace %s has no ComponentLink from Bidder/%s to a Pds", bidderWorkspace, bidderName)
	}

	ctx := context.Background()
	bidderRI := n.componentLinkClient(t, bidderWorkspace).Resource(componentLinkGVR)
	if err := bidderRI.Delete(ctx, pick.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("deleting the ComponentLink %s: %v", pick.Name, err)
	}
	waitRecreatedComponentLink(t, n, bidderRI, pick.Name, pick.UID, 3*time.Minute)

	target, ok := pdsTargetOutside(status, bidderWorkspace)
	if !ok {
		t.Fatalf("the MarketNetwork status names no Pds component outside the bidder workspace %s: network %+v, apps %+v",
			bidderWorkspace, status.Components, status.Apps)
	}
	targetRI := n.componentLinkClient(t, target.Workspace).Resource(componentLinkGVR)
	manualName := "manual-cross-workspace-" + randHex(4)
	spec := exposurev1alpha1.ComponentLinkSpec{
		From: exposurev1alpha1.ComponentLinkEndpoint{Workspace: bidderWorkspace, Kind: "Bidder", Name: bidderName},
		To:   exposurev1alpha1.ComponentLinkEndpoint{Workspace: target.Workspace, Kind: target.Kind, Name: target.Name},
	}
	specObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	if err != nil {
		t.Fatalf("converting the manual ComponentLink spec: %v", err)
	}
	manual := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": exposurev1alpha1.GroupVersion.String(),
		"kind":       "ComponentLink",
		"metadata":   map[string]any{"name": manualName},
		"spec":       specObject,
	}}
	if _, err := targetRI.Create(ctx, manual, metav1.CreateOptions{FieldManager: "componentlink-integration-test"}); err != nil {
		t.Fatalf("creating the manual cross-workspace ComponentLink %s in %s: %v", manualName, target.Workspace, err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := targetRI.Delete(cctx, manualName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("deleting the manual ComponentLink %s: %v", manualName, err)
		}
	})
	waitReadyComponentLink(t, n, targetRI, manualName, 3*time.Minute)

	if err := targetRI.Delete(ctx, manualName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("deleting the manual ComponentLink %s: %v", manualName, err)
	}
	waitGoneComponentLink(t, n, targetRI, manualName, 2*time.Minute)
}

func requireLinkDenied(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: the call was allowed; want a ComponentLink denial", what)
	}
	if !errors.Is(err, component.ErrLinkDenied) {
		t.Fatalf("%s: the error is %v; want component.ErrLinkDenied", what, err)
	}
	var denied *component.LinkDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("%s: the error is %v; want a *component.LinkDeniedError", what, err)
	}
}

func requireHandlerStatus(t *testing.T, what string, status int, err error, want int) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if status != want {
		t.Fatalf("%s: the handler answered %d; want %d", what, status, want)
	}
}

func (n *Network) componentLinkClient(t testing.TB, workspace string) dynamic.Interface {
	t.Helper()
	kc, err := n.read(n.Up.AdminKubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kc)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Host = strings.TrimSuffix(strings.SplitN(cfg.Host, "/clusters/", 2)[0], "/") + "/clusters/" + url.PathEscape(workspace)
	cfg.Timeout = 15 * time.Second
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return dyn
}

func getComponentLink(ctx context.Context, ri dynamic.ResourceInterface, name string) (exposurev1alpha1.ComponentLink, bool, error) {
	u, err := ri.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return exposurev1alpha1.ComponentLink{}, false, nil
	}
	if err != nil {
		return exposurev1alpha1.ComponentLink{}, false, err
	}
	var link exposurev1alpha1.ComponentLink
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &link); err != nil {
		return exposurev1alpha1.ComponentLink{}, false, err
	}
	return link, true, nil
}

func listComponentLinks(ctx context.Context, ri dynamic.ResourceInterface) ([]exposurev1alpha1.ComponentLink, error) {
	list, err := ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]exposurev1alpha1.ComponentLink, 0, len(list.Items))
	for i := range list.Items {
		var link exposurev1alpha1.ComponentLink
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &link); err != nil {
			return nil, fmt.Errorf("decoding the ComponentLink %s: %w", list.Items[i].GetName(), err)
		}
		out = append(out, link)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func waitWorkspaceComponentLinks(t *testing.T, n *Network, ri dynamic.ResourceInterface, workspace string, timeout time.Duration) []exposurev1alpha1.ComponentLink {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		if err := n.health(); err != nil {
			t.Fatalf("the network health before reading the ComponentLinks of %s: %v", workspace, err)
		}
		links, err := listComponentLinks(context.Background(), ri)
		if err != nil {
			last = err.Error()
		} else if settled, message := componentLinksSettled(workspace, links); settled {
			return links
		} else {
			last = message
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ComponentLinks of %s did not settle within %s: %s", workspace, timeout, last)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func componentLinksSettled(workspace string, links []exposurev1alpha1.ComponentLink) (bool, string) {
	for _, link := range links {
		if link.Spec.From.Workspace != workspace || link.Spec.To.Workspace != workspace {
			return false, fmt.Sprintf("the ComponentLink %s spans %s -> %s, not %s",
				link.Name, link.Spec.From.Workspace, link.Spec.To.Workspace, workspace)
		}
		if link.Status.ObservedGeneration != 0 && link.Generation != 0 && link.Status.ObservedGeneration != link.Generation {
			return false, fmt.Sprintf("the ComponentLink %s observed generation %d of %d",
				link.Name, link.Status.ObservedGeneration, link.Generation)
		}
		if !link.Status.Ready || link.Status.Phase != exposurev1alpha1.ComponentLinkReady {
			return false, fmt.Sprintf("the ComponentLink %s is %s ready=%t: %s",
				link.Name, link.Status.Phase, link.Status.Ready, link.Status.Message)
		}
	}
	return true, ""
}

func bidderComponentOf(t *testing.T, status v1alpha1.MarketNetworkStatus) (string, string, string) {
	t.Helper()
	workspace, name, policyEngine := "", "", ""
	for _, app := range status.Apps {
		if app.Kind != v1alpha1.ApplicationKindMarketBidder {
			continue
		}
		for _, c := range app.Components {
			if strings.EqualFold(c.Kind, "Bidder") && name == "" {
				name = c.Name
				workspace = strings.TrimSpace(c.Workspace)
				if workspace == "" {
					workspace = strings.TrimSpace(app.Workspace)
				}
			}
			if strings.EqualFold(c.Kind, "PolicyEngine") && policyEngine == "" {
				policyEngine = c.Name
			}
		}
	}
	if workspace == "" || name == "" {
		t.Fatalf("the MarketNetwork status names no Bidder component: %+v", status.Apps)
	}
	return workspace, name, policyEngine
}

func pdsTargetOutside(status v1alpha1.MarketNetworkStatus, workspace string) (v1alpha1.ApplicationComponent, bool) {
	for _, c := range status.Components {
		if strings.EqualFold(c.Kind, "Pds") && strings.TrimSpace(c.Workspace) != "" && c.Workspace != workspace {
			return c, true
		}
	}
	for _, app := range status.Apps {
		for _, c := range app.Components {
			if strings.EqualFold(c.Kind, "Pds") && strings.TrimSpace(c.Workspace) != "" && c.Workspace != workspace {
				return c, true
			}
		}
	}
	return v1alpha1.ApplicationComponent{}, false
}

func waitRecreatedComponentLink(t *testing.T, n *Network, ri dynamic.ResourceInterface, name string, oldUID types.UID, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		if err := n.health(); err != nil {
			t.Fatalf("the network health while waiting for the ComponentLink %s: %v", name, err)
		}
		link, found, err := getComponentLink(context.Background(), ri, name)
		switch {
		case err != nil:
			last = err.Error()
		case !found:
			last = "the ComponentLink is gone"
		case link.DeletionTimestamp != nil:
			last = "the deleted ComponentLink is still in its finalizer"
		case link.UID == oldUID:
			last = "the same ComponentLink is still there"
		case !link.Status.Ready || link.Status.Phase != exposurev1alpha1.ComponentLinkReady:
			last = fmt.Sprintf("the new ComponentLink is %s ready=%t: %s", link.Status.Phase, link.Status.Ready, link.Status.Message)
		default:
			last = ""
		}
		if last == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ComponentLink %s was not recreated and Ready within %s: %s", name, timeout, last)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func waitReadyComponentLink(t *testing.T, n *Network, ri dynamic.ResourceInterface, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		if err := n.health(); err != nil {
			t.Fatalf("the network health while waiting for the ComponentLink %s: %v", name, err)
		}
		link, found, err := getComponentLink(context.Background(), ri, name)
		switch {
		case err != nil:
			last = err.Error()
		case !found:
			last = "the ComponentLink is gone"
		case link.Status.Phase == exposurev1alpha1.ComponentLinkFailed:
			t.Fatalf("the ComponentLink %s is Failed: %s", name, link.Status.Message)
		case !link.Status.Ready || link.Status.Phase != exposurev1alpha1.ComponentLinkReady:
			last = fmt.Sprintf("the ComponentLink is %s ready=%t: %s", link.Status.Phase, link.Status.Ready, link.Status.Message)
		default:
			last = ""
		}
		if last == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ComponentLink %s was not Ready within %s: %s", name, timeout, last)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func waitGoneComponentLink(t *testing.T, n *Network, ri dynamic.ResourceInterface, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		if err := n.health(); err != nil {
			t.Fatalf("the network health while waiting for the ComponentLink %s to go: %v", name, err)
		}
		_, found, err := getComponentLink(context.Background(), ri, name)
		switch {
		case err != nil:
			last = err.Error()
		case found:
			last = "the ComponentLink is still there"
		default:
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ComponentLink %s did not go away within %s: %s", name, timeout, last)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
