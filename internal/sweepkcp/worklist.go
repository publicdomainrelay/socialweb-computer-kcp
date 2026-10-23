package sweepkcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	kcpv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
)

var (
	socialwebResource = v1alpha1.GroupVersion
	tenancyResource   = kcpv1alpha1.SchemeGroupVersion
)

const SweepExemptLabel = "socialweb.computer/sweep"

const SweepExemptValue = "exempt"

type Options struct {
	Host string

	RestConfig *rest.Config

	Transport http.RoundTripper
}

type Worklist struct {
	cfg    *rest.Config
	http   *http.Client
	codecs runtime.NegotiatedSerializer
}

func NewWorklist(opts Options) (*Worklist, error) {
	if opts.Host == "" {
		return nil, errors.New("sweepkcp: Host is required")
	}
	cfg := &rest.Config{}
	if opts.RestConfig != nil {
		cfg = rest.CopyConfig(opts.RestConfig)
	}
	cfg.Host = strings.TrimSuffix(opts.Host, "/")
	cfg.ContentType = "application/json"
	cfg.AcceptContentTypes = "application/json"
	if opts.Transport != nil {
		cfg.Transport = opts.Transport
	}
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("sweepkcp: %w", err)
	}
	scheme, err := newScheme()
	if err != nil {
		return nil, err
	}
	return &Worklist{cfg: cfg, http: httpClient, codecs: serializer.NewCodecFactory(scheme).WithoutConversion()}, nil
}

func (w *Worklist) DIDs(ctx context.Context) ([]string, error) {
	names, err := w.accountWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		dids, err := w.accountDIDs(ctx, kcpclient.RootWorkspace+":"+name)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, did := range dids {
			if did == "" || seen[did] {
				continue
			}
			seen[did] = true
			out = append(out, did)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (w *Worklist) accountWorkspaces(ctx context.Context) ([]string, error) {
	r, err := w.resource(kcpclient.RootWorkspace, tenancyResource)
	if err != nil {
		return nil, err
	}
	var list kcpv1alpha1.WorkspaceList
	if err := r.Get().Resource("workspaces").Do(ctx).Into(&list); err != nil {
		return nil, fmt.Errorf("sweepkcp: list workspaces in %s: %w", kcpclient.RootWorkspace, err)
	}
	names := make([]string, 0, len(list.Items))
	for _, ws := range list.Items {
		if ws.Spec.Mount == nil {
			names = append(names, ws.Name)
		}
	}
	return names, nil
}

func (w *Worklist) accountDIDs(ctx context.Context, logicalCluster string) ([]string, error) {
	r, err := w.resource(logicalCluster, socialwebResource)
	if err != nil {
		return nil, err
	}
	var list v1alpha1.AtprotoIdentityList
	if err := r.Get().Resource("atprotoidentities").Do(ctx).Into(&list); err != nil {
		return nil, fmt.Errorf("sweepkcp: list identities in %s: %w", logicalCluster, err)
	}
	out := make([]string, 0, len(list.Items))
	for _, rec := range list.Items {
		if rec.Spec.Scope != v1alpha1.IdentityScopeAccount {
			continue
		}
		if rec.Labels[SweepExemptLabel] == SweepExemptValue {
			continue
		}
		out = append(out, rec.Spec.DID)
	}
	return out, nil
}

func (w *Worklist) resource(logicalCluster string, gv schema.GroupVersion) (rest.Interface, error) {
	cfg := rest.CopyConfig(w.cfg)
	cfg.GroupVersion = &gv
	cfg.APIPath = "/clusters/" + logicalCluster + "/apis"
	cfg.NegotiatedSerializer = w.codecs
	r, err := rest.RESTClientForConfigAndClient(cfg, w.http)
	if err != nil {
		return nil, fmt.Errorf("sweepkcp: %s: %w", logicalCluster, err)
	}
	return r, nil
}

func newScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("sweepkcp: %w", err)
	}
	if err := kcpv1alpha1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("sweepkcp: %w", err)
	}
	metav1.AddToGroupVersion(s, schema.GroupVersion{Version: "v1"})
	return s, nil
}
