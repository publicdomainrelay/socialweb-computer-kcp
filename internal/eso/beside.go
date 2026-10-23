package eso

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/johnandersen777/socialweb-computer-kcp/api/kcpgvr"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
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
	"CustomResourceDefinition":       kcpgvr.CustomResourceDefinitions,
	"ValidatingWebhookConfiguration": kcpgvr.ValidatingWebhookConfigurations,
	"ClusterRole":                    kcpgvr.ClusterRoles,
	"ClusterRoleBinding":             kcpgvr.ClusterRoleBindings,
	"Role":                           kcpgvr.Roles,
	"RoleBinding":                    kcpgvr.RoleBindings,
	"Deployment":                     kcpgvr.Deployments,
	"ServiceAccount":                 kcpgvr.ServiceAccounts,
	"Service":                        kcpgvr.Services,
	"Secret":                         kcpgvr.Secrets,
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
	Kept []kube.Doc

	Webhooks []kube.Doc

	Retired []kube.Doc
}

type BesideOptions struct {
	BundlePath string

	LegacyNamespace string

	WebhookURL string

	WebhookCAPEM []byte

	FieldManager string
}

func LoadBundle(path string) ([]kube.Doc, error) {
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

func SplitBundle(objs []kube.Doc, namespace string) (Split, error) {
	if namespace == "" {
		return Split{}, errors.New("eso: splitting the bundle needs the namespace the in-tenant " +
			"install put its workloads in, which is where the migration deletes them from")
	}
	var out Split
	var unknown []string
	for _, u := range objs {
		u := kube.Doc(kruntime.DeepCopyJSON(u))
		kind := kindOf(u)
		if _, ok := bundleResources[kind]; !ok {
			unknown = append(unknown, kind+" "+nameOf(u))
			continue
		}
		if namespacedBundleKinds[kind] {
			setNamespace(u, namespace)
		}
		switch {
		case kind == "CustomResourceDefinition":
			out.Kept = append(out.Kept, u)
		case kind == "ClusterRole" && keptClusterRoles[nameOf(u)]:
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

func WebhookConfigurations(webhooks []kube.Doc, base string, caPEM []byte) ([]kube.Doc, error) {
	if base == "" || len(caPEM) == 0 {
		return nil, errors.New("eso: registering the webhook by URL needs the loopback base URL " +
			"and the CA that signed the webhook's serving certificate")
	}
	base = strings.TrimRight(base, "/")
	bundle := base64.StdEncoding.EncodeToString(caPEM)
	out := make([]kube.Doc, 0, len(webhooks))
	for _, in := range webhooks {
		u := kube.Doc(kruntime.DeepCopyJSON(in))
		hooks, found, err := docSlice(u, "webhooks")
		if err != nil || !found || len(hooks) == 0 {
			return nil, fmt.Errorf("eso: the ValidatingWebhookConfiguration %s carries no webhooks: %v", nameOf(u), err)
		}
		for i, h := range hooks {
			hook, ok := h.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("eso: webhook %d of %s is %T", i, nameOf(u), h)
			}
			path := docString(hook, "clientConfig", "service", "path")
			if path == "" {
				return nil, fmt.Errorf("%w (%s/%v)", ErrNoWebhookPath, nameOf(u), hook["name"])
			}
			hook["clientConfig"] = map[string]any{"url": base + path, "caBundle": bundle}
			hook["failurePolicy"] = "Ignore"
		}
		out = append(out, u)
	}
	return out, nil
}

func ControllerBindingObject() kube.Doc {
	return kube.Doc{
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
	}
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

func InstallBeside(ctx context.Context, c *kube.Client, o BesideOptions) error {
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
	if err := Retire(ctx, c, split.Retired); err != nil {
		return err
	}
	jobs := make([]func(context.Context) error, 0, len(split.Kept)+1+len(webhooks))
	for _, u := range split.Kept {
		jobs = append(jobs, func(ctx context.Context) error { return applyBundleObject(ctx, c, u, o.FieldManager) })
	}
	binding := ControllerBindingObject()
	jobs = append(jobs, func(ctx context.Context) error { return applyBundleObject(ctx, c, binding, o.FieldManager) })
	for _, u := range webhooks {
		jobs = append(jobs, func(ctx context.Context) error {
			if err := replaceServiceRegistration(ctx, c, nameOf(u)); err != nil {
				return err
			}
			return applyBundleObject(ctx, c, u, o.FieldManager)
		})
	}
	if err := runParallel(ctx, jobs); err != nil {
		return err
	}
	var crds []string
	for _, u := range split.Kept {
		if kindOf(u) == "CustomResourceDefinition" {
			crds = append(crds, nameOf(u))
		}
	}
	return waitEstablished(ctx, c, crds)
}

const defaultRetireTimeout = 2 * time.Minute

var RetireTimeout = defaultRetireTimeout

func Retire(ctx context.Context, c *kube.Client, retired []kube.Doc) error {
	foreground := metav1.DeletePropagationForeground
	var problems []error
	var deleted []kube.Resource
	var deletedNames []kube.Doc
	for _, u := range retired {
		gvr, ok := bundleResources[kindOf(u)]
		if !ok {
			problems = append(problems, fmt.Errorf("%w: %s %s", ErrBundleKind, kindOf(u), nameOf(u)))
			continue
		}
		r := c.Resource(gvr).In(namespaceOf(u))
		err := r.Delete(ctx, nameOf(u), metav1.DeleteOptions{PropagationPolicy: &foreground})
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			problems = append(problems, fmt.Errorf("eso: retiring the in-tenant %s %s: %w", kindOf(u), keyName(u), err))
		default:
			deleted = append(deleted, r)
			deletedNames = append(deletedNames, u)
		}
	}
	rctx, cancel := context.WithTimeout(ctx, RetireTimeout)
	defer cancel()
	for i, r := range deleted {
		u := deletedNames[i]
		if _, err := kube.Await(rctx, r, nameOf(u), 0, kube.Absent[kube.Doc]); err != nil {
			if _, gerr := kube.Get[kube.Doc](ctx, r, nameOf(u)); apierrors.IsNotFound(gerr) {
				continue
			}
			if ctx.Err() != nil {
				return errors.Join(append(problems, ctx.Err())...)
			}
			problems = append(problems, fmt.Errorf("eso: the in-tenant %s %s is still there %s after "+
				"its foreground delete; its pods would read as tenant demand once the baseline no "+
				"longer names it, so the install stops here and the next pass retries",
				kindOf(u), keyName(u), RetireTimeout))
		}
	}
	return errors.Join(problems...)
}

func replaceServiceRegistration(ctx context.Context, c *kube.Client, name string) error {
	r := c.Resource(bundleResources["ValidatingWebhookConfiguration"])
	live, err := kube.Get[kube.Doc](ctx, r, name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("eso: reading the webhook registration %s: %w", name, err)
	}
	hooks, _, _ := docSlice(*live, "webhooks")
	byService := false
	for _, h := range hooks {
		hook, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if svc, found, _ := field(hook, "clientConfig", "service"); found {
			if _, ok := svc.(map[string]any); ok {
				byService = true
			}
		}
	}
	if !byService {
		return nil
	}
	if err := r.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("eso: deleting the webhook registration %s that still names a Service: %w", name, err)
	}
	return nil
}

func applyBundleObject(ctx context.Context, c *kube.Client, u kube.Doc, manager string) error {
	gvr, ok := bundleResources[kindOf(u)]
	if !ok {
		return fmt.Errorf("%w: %s %s", ErrBundleKind, kindOf(u), nameOf(u))
	}
	if _, err := kube.Apply[kube.Doc](ctx, c.Resource(gvr).In(namespaceOf(u)), u,
		kube.ApplyOptions{FieldManager: manager, Force: true}); err != nil {
		return fmt.Errorf("eso: applying the %s %s: %w", kindOf(u), nameOf(u), err)
	}
	return nil
}

func waitEstablished(ctx context.Context, c *kube.Client, names []string) error {
	ctx, cancel := context.WithTimeout(ctx, EstablishTimeout)
	defer cancel()
	r := c.Resource(bundleResources["CustomResourceDefinition"])
	for _, name := range names {
		last := "not read yet"
		if _, err := kube.Await(ctx, r, name, 0, func(u *kube.Doc) (bool, error) {
			if u == nil {
				last = "the CRD is not there"
				return false, nil
			}
			last = "the Established condition is not True"
			return established(*u), nil
		}); err != nil {
			return fmt.Errorf("eso: the CRD %s never became Established (%s): the controller beside "+
				"the control plane starts an informer per kind, and an informer for a kind the "+
				"apiserver does not serve fails the controller's start", name, last)
		}
	}
	return nil
}

func established(u kube.Doc) bool {
	conditions, _, _ := docSlice(u, "status", "conditions")
	for _, c := range conditions {
		m, ok := c.(map[string]any)
		if ok && m["type"] == "Established" && m["status"] == "True" {
			return true
		}
	}
	return false
}

func keyName(u kube.Doc) string {
	if namespaceOf(u) != "" {
		return namespaceOf(u) + "/" + nameOf(u)
	}
	return nameOf(u)
}
