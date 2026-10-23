package eso

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

const (
	ControllerUser = "system:socialweb:external-secrets"

	ControllerGroup = "system:socialweb:external-secrets"

	ControllerClusterRole = "external-secrets-controller"

	ControllerBinding = "socialweb-external-secrets"

	WebhookDNSName = "localhost"

	WebhookCertName = "tls.crt"

	WebhookKeyName = "tls.key"

	WebhookCAName = "ca.crt"

	WebhookLookahead = 30 * 24 * time.Hour

	WebhookRenewBefore = 365 * 24 * time.Hour

	EstablishTimeout = 60 * time.Second
)

var keptClusterRoles = map[string]bool{
	"external-secrets-controller":      true,
	"external-secrets-view":            true,
	"external-secrets-edit":            true,
	"external-secrets-servicebindings": true,
}

var bundleResources = map[string]schema.GroupVersionResource{
	"CustomResourceDefinition":       {Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"},
	"ValidatingWebhookConfiguration": {Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingwebhookconfigurations"},
	"ClusterRole":                    {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
	"ClusterRoleBinding":             {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
	"Role":                           {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"},
	"RoleBinding":                    {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
	"Deployment":                     {Group: "apps", Version: "v1", Resource: "deployments"},
	"ServiceAccount":                 {Group: "", Version: "v1", Resource: "serviceaccounts"},
	"Service":                        {Group: "", Version: "v1", Resource: "services"},
	"Secret":                         {Group: "", Version: "v1", Resource: "secrets"},
}

var namespacedBundleKinds = map[string]bool{
	"Role": true, "RoleBinding": true, "Deployment": true, "ServiceAccount": true, "Service": true, "Secret": true,
}

var ErrBundleKind = errors.New(
	"eso: the External Secrets bundle carries a kind the beside install does not place. Every " +
		"object in the pinned bundle is either kept in the tenant (the CRDs and the controller's " +
		"ClusterRoles), re-registered by loopback URL (the validating webhooks), or retired because " +
		"what it served now runs beside the control plane; an object in none of the three would be " +
		"silently dropped on a bundle upgrade, so it is refused by name instead")

var ErrNoWebhookPath = errors.New(
	"eso: a validating webhook in the bundle names no service path, so there is no path to put " +
		"behind the loopback URL and the apiserver would call the webhook server's root")

type Split struct {
	Kept []unstructured.Unstructured

	Webhooks []unstructured.Unstructured

	Retired []unstructured.Unstructured
}

type BesideOptions struct {
	BundlePath string

	LegacyNamespace string

	WebhookURL string

	WebhookCAPEM []byte

	FieldManager string
}

func LoadBundle(path string) ([]unstructured.Unstructured, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eso: reading the External Secrets bundle %s: %w", path, err)
	}
	objs, err := decodeObjects(raw, path)
	if err != nil {
		return nil, err
	}
	if len(objs) == 0 {
		return nil, fmt.Errorf("eso: the External Secrets bundle %s holds no objects", path)
	}
	return objs, nil
}

func SplitBundle(objs []unstructured.Unstructured, namespace string) (Split, error) {
	if namespace == "" {
		return Split{}, errors.New("eso: splitting the bundle needs the namespace the in-tenant " +
			"install put its workloads in, which is where the migration deletes them from")
	}
	var out Split
	var unknown []string
	for _, u := range objs {
		u := *u.DeepCopy()
		kind := u.GetKind()
		if _, ok := bundleResources[kind]; !ok {
			unknown = append(unknown, kind+" "+u.GetName())
			continue
		}
		if namespacedBundleKinds[kind] {
			u.SetNamespace(namespace)
		}
		switch {
		case kind == "CustomResourceDefinition":
			out.Kept = append(out.Kept, u)
		case kind == "ClusterRole" && keptClusterRoles[u.GetName()]:
			out.Kept = append(out.Kept, u)
		case kind == "ValidatingWebhookConfiguration":
			out.Webhooks = append(out.Webhooks, u)
		default:
			out.Retired = append(out.Retired, u)
		}
	}
	if len(unknown) > 0 {
		return Split{}, fmt.Errorf("%w: %s", ErrBundleKind, strings.Join(unknown, ", "))
	}
	return out, nil
}

func WebhookConfigurations(webhooks []unstructured.Unstructured, base string, caPEM []byte) ([]unstructured.Unstructured, error) {
	if base == "" || len(caPEM) == 0 {
		return nil, errors.New("eso: registering the webhook by URL needs the loopback base URL " +
			"and the CA that signed the webhook's serving certificate")
	}
	base = strings.TrimRight(base, "/")
	bundle := base64.StdEncoding.EncodeToString(caPEM)
	out := make([]unstructured.Unstructured, 0, len(webhooks))
	for _, in := range webhooks {
		u := *in.DeepCopy()
		hooks, found, err := unstructured.NestedSlice(u.Object, "webhooks")
		if err != nil || !found || len(hooks) == 0 {
			return nil, fmt.Errorf("eso: the ValidatingWebhookConfiguration %s carries no webhooks: %v", u.GetName(), err)
		}
		for i, h := range hooks {
			hook, ok := h.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("eso: webhook %d of %s is %T", i, u.GetName(), h)
			}
			path, _, _ := unstructured.NestedString(hook, "clientConfig", "service", "path")
			if path == "" {
				return nil, fmt.Errorf("%w (%s/%v)", ErrNoWebhookPath, u.GetName(), hook["name"])
			}
			hook["clientConfig"] = map[string]any{"url": base + path, "caBundle": bundle}
			hook["failurePolicy"] = "Ignore"
			hooks[i] = hook
		}
		if err := unstructured.SetNestedSlice(u.Object, hooks, "webhooks"); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

func ControllerBindingObject() unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRoleBinding",
		"metadata":   map[string]any{"name": ControllerBinding},
		"roleRef": map[string]any{
			"apiGroup": "rbac.authorization.k8s.io",
			"kind":     "ClusterRole",
			"name":     ControllerClusterRole,
		},
		"subjects": []any{map[string]any{
			"apiGroup": "rbac.authorization.k8s.io",
			"kind":     "Group",
			"name":     ControllerGroup,
		}},
	}}
}

const applyParallel = 8

func runParallel(ctx context.Context, jobs []func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, applyParallel)
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
	)
	for _, job := range jobs {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if err := job(ctx); err != nil {
				once.Do(func() {
					first = err
					cancel()
				})
			}
		})
	}
	wg.Wait()
	return first
}

func InstallBeside(ctx context.Context, dyn dynamic.Interface, o BesideOptions) error {
	if o.BundlePath == "" || o.LegacyNamespace == "" || o.FieldManager == "" {
		return errors.New("eso: the beside install needs the pinned bundle, the namespace the " +
			"in-tenant install used, and a field manager to apply under")
	}
	objs, err := LoadBundle(o.BundlePath)
	if err != nil {
		return err
	}
	split, err := SplitBundle(objs, o.LegacyNamespace)
	if err != nil {
		return err
	}
	webhooks, err := WebhookConfigurations(split.Webhooks, o.WebhookURL, o.WebhookCAPEM)
	if err != nil {
		return err
	}
	if err := Retire(ctx, dyn, split.Retired); err != nil {
		return err
	}
	jobs := make([]func(context.Context) error, 0, len(split.Kept)+1+len(webhooks))
	for i := range split.Kept {
		u := &split.Kept[i]
		jobs = append(jobs, func(ctx context.Context) error { return applyBundleObject(ctx, dyn, u, o.FieldManager) })
	}
	binding := ControllerBindingObject()
	jobs = append(jobs, func(ctx context.Context) error { return applyBundleObject(ctx, dyn, &binding, o.FieldManager) })
	for i := range webhooks {
		u := &webhooks[i]
		jobs = append(jobs, func(ctx context.Context) error {
			if err := replaceServiceRegistration(ctx, dyn, u.GetName()); err != nil {
				return err
			}
			return applyBundleObject(ctx, dyn, u, o.FieldManager)
		})
	}
	if err := runParallel(ctx, jobs); err != nil {
		return err
	}
	var crds []string
	for _, u := range split.Kept {
		if u.GetKind() == "CustomResourceDefinition" {
			crds = append(crds, u.GetName())
		}
	}
	return waitEstablished(ctx, dyn, crds)
}

const (
	defaultRetirePoll    = time.Second
	defaultRetireTimeout = 2 * time.Minute
)

var (
	RetirePoll    = defaultRetirePoll
	RetireTimeout = defaultRetireTimeout
)

func Retire(ctx context.Context, dyn dynamic.Interface, retired []unstructured.Unstructured) error {
	foreground := metav1.DeletePropagationForeground
	var problems []error
	var deleted []dynamic.ResourceInterface
	var deletedNames []unstructured.Unstructured
	for _, u := range retired {
		gvr, ok := bundleResources[u.GetKind()]
		if !ok {
			problems = append(problems, fmt.Errorf("%w: %s %s", ErrBundleKind, u.GetKind(), u.GetName()))
			continue
		}
		var ri dynamic.ResourceInterface = dyn.Resource(gvr)
		if u.GetNamespace() != "" {
			ri = dyn.Resource(gvr).Namespace(u.GetNamespace())
		}
		err := ri.Delete(ctx, u.GetName(), metav1.DeleteOptions{PropagationPolicy: &foreground})
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			problems = append(problems, fmt.Errorf("eso: retiring the in-tenant %s %s: %w", u.GetKind(), keyName(u), err))
		default:
			deleted = append(deleted, ri)
			deletedNames = append(deletedNames, u)
		}
	}
	deadline := time.Now().Add(RetireTimeout)
	for i, ri := range deleted {
		u := deletedNames[i]
		for {
			_, err := ri.Get(ctx, u.GetName(), metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				break
			}
			if time.Now().After(deadline) {
				problems = append(problems, fmt.Errorf("eso: the in-tenant %s %s is still there %s after "+
					"its foreground delete; its pods would read as tenant demand once the baseline no "+
					"longer names it, so the install stops here and the next pass retries",
					u.GetKind(), keyName(u), RetireTimeout))
				break
			}
			select {
			case <-ctx.Done():
				return errors.Join(append(problems, ctx.Err())...)
			case <-time.After(RetirePoll):
			}
		}
	}
	return errors.Join(problems...)
}

func replaceServiceRegistration(ctx context.Context, dyn dynamic.Interface, name string) error {
	ri := dyn.Resource(bundleResources["ValidatingWebhookConfiguration"])
	live, err := ri.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("eso: reading the webhook registration %s: %w", name, err)
	}
	hooks, _, _ := unstructured.NestedSlice(live.Object, "webhooks")
	byService := false
	for _, h := range hooks {
		hook, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if _, found, _ := unstructured.NestedMap(hook, "clientConfig", "service"); found {
			byService = true
		}
	}
	if !byService {
		return nil
	}
	if err := ri.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("eso: deleting the webhook registration %s that still names a Service: %w", name, err)
	}
	return nil
}

func applyBundleObject(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured, manager string) error {
	gvr, ok := bundleResources[u.GetKind()]
	if !ok {
		return fmt.Errorf("%w: %s %s", ErrBundleKind, u.GetKind(), u.GetName())
	}
	body, err := json.Marshal(u.Object)
	if err != nil {
		return fmt.Errorf("eso: marshalling the %s %s: %w", u.GetKind(), u.GetName(), err)
	}
	var ri dynamic.ResourceInterface = dyn.Resource(gvr)
	if u.GetNamespace() != "" {
		ri = dyn.Resource(gvr).Namespace(u.GetNamespace())
	}
	if _, err := ri.Patch(ctx, u.GetName(), types.ApplyPatchType, body,
		metav1.PatchOptions{FieldManager: manager, Force: new(true)}); err != nil {
		return fmt.Errorf("eso: applying the %s %s: %w", u.GetKind(), u.GetName(), err)
	}
	return nil
}

func waitEstablished(ctx context.Context, dyn dynamic.Interface, names []string) error {
	ctx, cancel := context.WithTimeout(ctx, EstablishTimeout)
	defer cancel()
	ri := dyn.Resource(bundleResources["CustomResourceDefinition"])
	for _, name := range names {
		last := "not read yet"
		for {
			u, err := ri.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				last = err.Error()
			} else if established(u) {
				break
			} else {
				last = "the Established condition is not True"
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("eso: the CRD %s never became Established (%s): the controller beside "+
					"the control plane starts an informer per kind, and an informer for a kind the "+
					"apiserver does not serve fails the controller's start", name, last)
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	return nil
}

func established(u *unstructured.Unstructured) bool {
	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conditions {
		m, ok := c.(map[string]any)
		if ok && m["type"] == "Established" && m["status"] == "True" {
			return true
		}
	}
	return false
}

func keyName(u unstructured.Unstructured) string {
	if u.GetNamespace() != "" {
		return u.GetNamespace() + "/" + u.GetName()
	}
	return u.GetName()
}
