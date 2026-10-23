package childapplication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	networkLabel = "childapplication.socialweb.computer/network"
	appLabel     = "childapplication.socialweb.computer/app"

	childNameMax = 63
	childHashLen = 8
)

func (c *Controller) Ensure(ctx context.Context) error {
	ndyn, err := c.client(c.networkWorkspace)
	if err != nil {
		return err
	}
	networks, err := ndyn.Resource(MarketNetworkGVR).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return err
	}
	dyn, err := c.client(c.workspace)
	if err != nil {
		return err
	}
	ri := dyn.Resource(ChildApplicationGVR)
	existing, err := ri.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for i := range existing.Items {
		network, _, _ := unstructured.NestedString(existing.Items[i].Object, "spec", "networkRef", "name")
		app, _, _ := unstructured.NestedString(existing.Items[i].Object, "spec", "app")
		if network != "" && app != "" {
			have[childKey(network, app)] = true
		}
	}
	for i := range networks.Items {
		var network v1alpha1.MarketNetwork
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(networks.Items[i].Object, &network); err != nil {
			return fmt.Errorf("childapplication: reading the MarketNetwork %s: %w", networks.Items[i].GetName(), err)
		}
		profiles := map[string]string{}
		for _, app := range network.Spec.Apps {
			profiles[app.Name] = app.Profile
		}
		for _, appStatus := range network.Status.Apps {
			if appStatus.Name == "" {
				continue
			}
			key := childKey(network.Name, appStatus.Name)
			if have[key] {
				continue
			}
			if err := c.createChild(ctx, ri, network.Name, appStatus.Name, profiles[appStatus.Name]); err != nil {
				return err
			}
			have[key] = true
		}
	}
	return nil
}

func (c *Controller) createChild(ctx context.Context, ri dynamic.ResourceInterface, network, app, profile string) error {
	name := ChildName(network, app)
	child := &v1alpha1.ChildApplication{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ChildApplication"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{networkLabel: network, appLabel: app}},
		Spec:       v1alpha1.ChildApplicationSpec{NetworkRef: commonv1.LocalObjectRef{Name: network}, App: app, Profile: profile},
	}
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(child)
	if err != nil {
		return fmt.Errorf("childapplication: encoding the child application of %s/%s: %w", network, app, err)
	}
	_, err = ri.Create(ctx, &unstructured.Unstructured{Object: object}, metav1.CreateOptions{FieldManager: c.fieldManager})
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err) || apierrors.IsForbidden(err):
		return nil
	case apierrors.IsAlreadyExists(err):
		cur, err := ri.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		gotNetwork, _, _ := unstructured.NestedString(cur.Object, "spec", "networkRef", "name")
		gotApp, _, _ := unstructured.NestedString(cur.Object, "spec", "app")
		if gotNetwork == network && gotApp == app {
			return nil
		}
		return fmt.Errorf("childapplication: the name %s belongs to %s/%s", name, gotNetwork, gotApp)
	default:
		return fmt.Errorf("childapplication: creating the child application of %s/%s: %w", network, app, err)
	}
}

func ChildName(network, app string) string {
	sum := sha256.Sum256([]byte(network + "\x00" + app))
	suffix := hex.EncodeToString(sum[:])[:childHashLen]
	base := sanitizeName(network + "-" + app)
	limit := childNameMax - childHashLen - 1
	if len(base) > limit {
		base = strings.Trim(base[:limit], "-")
	}
	if base == "" {
		base = "child"
	}
	return base + "-" + suffix
}

func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func childKey(network, app string) string {
	return network + "\x00" + app
}
