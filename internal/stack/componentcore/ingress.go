package componentcore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
)

const ingressProxyKind = "IngressProxy"

type ingressRegistrarProvider interface {
	IngressRegistrar() exposure.IngressRegistrar
}

type ingressProviderNamer interface {
	IngressProvider() string
}

type ingressDispatcher struct {
	registry *component.Registry

	fallback exposure.IngressRegistrar

	fallbackName string

	log *slog.Logger

	mu sync.Mutex

	served map[string]string
}

func newIngressDispatcher(registry *component.Registry, fallback exposure.IngressRegistrar, fallbackName string, log *slog.Logger) *ingressDispatcher {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &ingressDispatcher{
		registry:     registry,
		fallback:     fallback,
		fallbackName: fallbackName,
		log:          log.With("component", "exposure-ingress"),
		served:       map[string]string{},
	}
}

func (d *ingressDispatcher) Register(ctx context.Context, spec exposure.IngressSpec) (exposure.IngressRegistration, error) {
	ref := strings.TrimSpace(spec.IngressRef)
	if ref != "" {
		if entry, ok := d.resolve(spec.Workspace, ref); ok {
			service := entry.Service
			if service == nil {
				return nil, fmt.Errorf("the IngressProxy component %s/%s exposes no ingress registrar", entry.Key.Workspace, entry.Key.Name)
			}
			provider, ok := service.(ingressRegistrarProvider)
			if !ok || provider.IngressRegistrar() == nil {
				return nil, fmt.Errorf("the IngressProxy component %s/%s exposes no ingress registrar", entry.Key.Workspace, entry.Key.Name)
			}
			name := "component"
			if named, ok := service.(ingressProviderNamer); ok {
				if value := strings.TrimSpace(named.IngressProvider()); value != "" {
					name = value
				}
			}
			registration, err := provider.IngressRegistrar().Register(ctx, spec)
			if err != nil {
				return nil, err
			}
			d.logServed(spec, entry.Key.Name, name)
			return registration, nil
		}
	}
	if d.fallback == nil {
		return nil, errors.New("no ingress registrar is configured")
	}
	name := d.fallbackName
	if named, ok := d.fallback.(ingressProviderNamer); ok {
		if value := strings.TrimSpace(named.IngressProvider()); value != "" {
			name = value
		}
	}
	registration, err := d.fallback.Register(ctx, spec)
	if err != nil {
		return nil, err
	}
	d.logServed(spec, ref, name)
	return registration, nil
}

func (d *ingressDispatcher) resolve(workspace, name string) (component.Entry, bool) {
	if d.registry == nil {
		return component.Entry{}, false
	}
	if entry, ok := d.registry.Lookup(workspace, ingressProxyKind, name); ok {
		return entry, true
	}
	for _, entry := range d.registry.List("") {
		if entry.Key.Kind == ingressProxyKind && entry.Key.Name == name {
			return entry, true
		}
	}
	return component.Entry{}, false
}

func (d *ingressDispatcher) logServed(spec exposure.IngressSpec, ingress, provider string) {
	key := spec.Workspace + "/" + spec.Exposure
	d.mu.Lock()
	if _, ok := d.served[key]; ok {
		d.mu.Unlock()
		return
	}
	d.served[key] = provider
	d.mu.Unlock()
	d.log.Info("the exposure is served by an ingress provider", "workspace", spec.Workspace,
		"exposure", spec.Exposure, "ingress", ingress, "provider", provider)
}
