package eventstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	eventsapi "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

var AtprotoRepoGVR = v1alpha1.AtprotoRepoGVR

type fallbackDirectory []identity.Directory

func (d fallbackDirectory) lookup(fn func(identity.Directory) (*identity.Identity, error)) (*identity.Identity, error) {
	var last error
	for _, dir := range d {
		ident, err := fn(dir)
		if err == nil {
			return ident, nil
		}
		last = err
	}
	if last == nil {
		last = identity.ErrDIDNotFound
	}
	return nil, last
}

func (d fallbackDirectory) LookupHandle(ctx context.Context, handle syntax.Handle) (*identity.Identity, error) {
	return d.lookup(func(dir identity.Directory) (*identity.Identity, error) {
		return dir.LookupHandle(ctx, handle)
	})
}

func (d fallbackDirectory) LookupDID(ctx context.Context, did syntax.DID) (*identity.Identity, error) {
	return d.lookup(func(dir identity.Directory) (*identity.Identity, error) {
		return dir.LookupDID(ctx, did)
	})
}

func (d fallbackDirectory) Lookup(ctx context.Context, atid syntax.AtIdentifier) (*identity.Identity, error) {
	return d.lookup(func(dir identity.Directory) (*identity.Identity, error) {
		return dir.Lookup(ctx, atid)
	})
}

func (d fallbackDirectory) Purge(ctx context.Context, atid syntax.AtIdentifier) error {
	var last error
	for _, dir := range d {
		if err := dir.Purge(ctx, atid); err != nil {
			last = err
		}
	}
	return last
}

func normalizeStreamURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		return "http://" + raw
	}
	return raw
}

func parseStreamURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(normalizeStreamURL(raw))
	if err != nil {
		return nil, fmt.Errorf("eventstream: %q: %w", raw, err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "ws", "wss":
	default:
		return nil, fmt.Errorf("eventstream: %q is not an http, https, ws or wss URL", raw)
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return nil, fmt.Errorf("eventstream: %q has no host", raw)
	}
	return parsed, nil
}

type watchSourcePlan struct {
	Name string

	Key string

	Source eventsapi.EventStreamSource
}

type watchRepo struct {
	Did string

	PDSRef *eventsapi.EventStreamRef
}

func EventStreamNameForWatchSource(watchName, sourceKey string) string {
	if strings.HasPrefix(sourceKey, "pds/") {
		return EventStreamNameForWatch(watchName)
	}
	sum := sha256.Sum256([]byte(sourceKey))
	suffix := hex.EncodeToString(sum[:6])
	candidate := EventStreamNamePrefix + watchName + "-" + suffix
	if len(candidate) <= maxEventStreamName {
		return candidate
	}
	keep := maxEventStreamName - len(suffix) - 1
	return candidate[:keep] + "-" + suffix
}

func relaySourceKey(rawURL string) string {
	return "relay/" + strings.TrimSpace(rawURL)
}

func jetstreamSourceKey(rawURL string) string {
	return "jetstream/" + strings.TrimSpace(rawURL)
}

func pdsSourceKey(name string) string {
	return "pds/" + strings.TrimSpace(name)
}

func (c *Controller) planWatchSources(ctx context.Context, dyn dynamic.Interface, watchName string, spec recordWatchSpec) ([]watchSourcePlan, error) {
	repo, err := c.resolveWatchRepo(ctx, dyn, spec)
	if err != nil {
		return nil, err
	}
	collections := []string(nil)
	if collection := strings.TrimSpace(spec.Collection); collection != "" {
		collections = []string{collection}
	}
	dids := []string(nil)
	if repo.Did != "" {
		dids = []string{repo.Did}
	}
	var plans []watchSourcePlan
	seen := map[string]bool{}
	add := func(key string, source eventsapi.EventStreamSource) {
		if seen[key] {
			return
		}
		seen[key] = true
		plans = append(plans, watchSourcePlan{
			Name:   EventStreamNameForWatchSource(watchName, key),
			Key:    key,
			Source: source,
		})
	}
	for _, raw := range spec.Source.RelayUrls {
		raw = normalizeStreamURL(raw)
		if raw == "" {
			continue
		}
		add(relaySourceKey(raw), eventsapi.EventStreamSource{
			Kind:        eventsapi.EventStreamSourceRelay,
			RelayURLs:   []string{raw},
			Collections: collections,
			DIDs:        dids,
		})
	}
	for _, raw := range spec.Source.JetstreamUrls {
		raw = normalizeStreamURL(raw)
		if raw == "" {
			continue
		}
		add(jetstreamSourceKey(raw), eventsapi.EventStreamSource{
			Kind:          eventsapi.EventStreamSourceJetstream,
			JetstreamURLs: []string{raw},
			Collections:   collections,
			DIDs:          dids,
		})
	}
	if len(plans) > 0 {
		return plans, nil
	}
	if repo.PDSRef == nil {
		return nil, errors.New("eventstream: the record watch has no pds source and no relay or jetstream source")
	}
	add(pdsSourceKey(repo.PDSRef.Name), eventsapi.EventStreamSource{
		Kind:        eventsapi.EventStreamSourcePDS,
		PDSRef:      repo.PDSRef,
		Collections: collections,
		DIDs:        dids,
	})
	return plans, nil
}

func (c *Controller) resolveWatchRepo(ctx context.Context, dyn dynamic.Interface, spec recordWatchSpec) (watchRepo, error) {
	if spec.RepoRef != nil {
		return c.resolveWatchRepoRef(ctx, dyn, spec.RepoRef)
	}
	repo := strings.TrimSpace(spec.Repo)
	if repo == "" {
		if len(spec.Source.RelayUrls) > 0 || len(spec.Source.JetstreamUrls) > 0 {
			return watchRepo{}, nil
		}
		return watchRepo{}, errors.New("eventstream: spec.repo or spec.repoRef is required")
	}
	if strings.HasPrefix(repo, "did:") {
		return watchRepo{Did: repo}, nil
	}
	return watchRepo{PDSRef: &eventsapi.EventStreamRef{Workspace: c.opts.Workspace, Kind: "Pds", Name: repo}}, nil
}

func (c *Controller) resolveWatchRepoRef(ctx context.Context, dyn dynamic.Interface, ref *recordRepoRef) (watchRepo, error) {
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return watchRepo{}, errors.New("eventstream: spec.repoRef needs a name")
	}
	workspace := strings.TrimSpace(ref.Workspace)
	if workspace == "" {
		workspace = c.opts.Workspace
	}
	switch strings.ToLower(strings.TrimSpace(ref.Kind)) {
	case "", "pds", "remotepds", "remote":
		kind := "Pds"
		if strings.Contains(strings.ToLower(ref.Kind), "remote") {
			kind = "RemotePDS"
		}
		return watchRepo{PDSRef: &eventsapi.EventStreamRef{Workspace: workspace, Kind: kind, Name: name}}, nil
	case "atprotorepo":
		client := dyn
		if workspace != c.opts.Workspace {
			var err error
			client, err = c.opts.Dynamic(workspace)
			if err != nil {
				return watchRepo{}, err
			}
		}
		object, err := client.Resource(AtprotoRepoGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return watchRepo{}, fmt.Errorf("eventstream: reading AtprotoRepo %s: %w", name, err)
		}
		return watchRepoFromAtprotoRepo(object)
	default:
		return watchRepo{}, fmt.Errorf("eventstream: unknown repoRef kind %q", ref.Kind)
	}
}

func watchRepoFromAtprotoRepo(object *unstructured.Unstructured) (watchRepo, error) {
	raw, err := json.Marshal(object.Object)
	if err != nil {
		return watchRepo{}, err
	}
	var repo struct {
		Spec struct {
			DID    string `json:"did"`
			PdsRef *struct {
				Workspace string `json:"workspace,omitempty"`
				Kind      string `json:"kind,omitempty"`
				Name      string `json:"name"`
			} `json:"pdsRef,omitempty"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &repo); err != nil {
		return watchRepo{}, err
	}
	out := watchRepo{Did: strings.TrimSpace(repo.Spec.DID)}
	if repo.Spec.PdsRef != nil && strings.TrimSpace(repo.Spec.PdsRef.Name) != "" {
		kind := strings.TrimSpace(repo.Spec.PdsRef.Kind)
		if kind == "" {
			kind = "Pds"
		}
		out.PDSRef = &eventsapi.EventStreamRef{
			Workspace: strings.TrimSpace(repo.Spec.PdsRef.Workspace),
			Kind:      kind,
			Name:      strings.TrimSpace(repo.Spec.PdsRef.Name),
		}
	}
	return out, nil
}
