package statesync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

var ErrNotFound = errors.New("statesync: the remote object does not exist")

var ErrRejected = errors.New("statesync: the target rejected the update")

type Request struct {
	Key string

	Payload json.RawMessage

	Sink v1alpha1.StateSyncSink

	Source *v1alpha1.StateSyncSource

	UID string
}

type Observation struct {
	Exists bool

	Ref string

	CID string

	Hash string

	At string

	Meta map[string]string

	Cursor *int64
}

type Adapter interface {
	Name() string

	Observe(ctx context.Context, request Request) (Observation, error)

	Apply(ctx context.Context, request Request) (Observation, error)

	Delete(ctx context.Context, request Request) error
}

type Registry struct {
	mu sync.RWMutex

	adapters map[string]Adapter
}

func NewRegistry() *Registry {
	return &Registry{adapters: map[string]Adapter{}}
}

func (r *Registry) Register(adapter Adapter) error {
	if r == nil {
		return errors.New("statesync: a nil registry cannot register adapters")
	}
	if adapter == nil {
		return errors.New("statesync: a nil adapter cannot be registered")
	}
	name := normalizeAdapterName(adapter.Name())
	if name == "" {
		return errors.New("statesync: an adapter needs a name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.adapters == nil {
		r.adapters = map[string]Adapter{}
	}
	if _, ok := r.adapters[name]; ok {
		return fmt.Errorf("statesync: an adapter named %q is already registered", name)
	}
	r.adapters[name] = adapter
	return nil
}

func (r *Registry) Lookup(name string) (Adapter, error) {
	if r == nil {
		return nil, errors.New("statesync: a nil registry holds no adapters")
	}
	normalized := normalizeAdapterName(name)
	if normalized == "" {
		return nil, errors.New("statesync: an adapter name is required")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	adapter, ok := r.adapters[normalized]
	if !ok {
		return nil, fmt.Errorf("statesync: no adapter named %q is registered", name)
	}
	return adapter, nil
}

func normalizeAdapterName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func InboxObservation(request Request, ref string) Observation {
	sum := sha256.Sum256(request.Payload)
	return Observation{
		Exists: true,
		Ref:    ref,
		Hash:   "sha256:" + hex.EncodeToString(sum[:]),
		At:     time.Now().UTC().Format(time.RFC3339),
	}
}
