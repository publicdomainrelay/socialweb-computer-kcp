package kcpclients

import (
	"errors"
	"fmt"
	"sync"

	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

type Set struct {
	mu sync.RWMutex

	base *rest.Config

	configs map[string]*rest.Config
}

var _ component.KCPClients = (*Set)(nil)

func New(cfg *rest.Config) (*Set, error) {
	base, err := kcpConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Set{base: base, configs: map[string]*rest.Config{}}, nil
}

func FromStore(store *kcpstore.Store) (*Set, error) {
	if store == nil {
		return nil, errors.New("kcpclients: the kcp clients need a kcp store")
	}
	return New(store.Config())
}

func FromKubeconfig(kubeconfig string) (*Set, error) {
	if kubeconfig == "" {
		return nil, errors.New("kcpclients: the kcp clients need a kubeconfig")
	}
	cfg, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kcpclients: the kcp front rest config: %w", err)
	}
	return New(cfg)
}

func (s *Set) Set(workspace string, cfg *rest.Config) error {
	if s == nil {
		return errors.New("kcpclients: the kcp client set is nil")
	}
	normalized, err := kcpConfig(cfg)
	if err != nil {
		return err
	}
	workspace = kcpWorkspace(workspace)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.configs == nil {
		s.configs = map[string]*rest.Config{}
	}
	s.configs[workspace] = normalized
	return nil
}

func (s *Set) Client(workspace string) (component.KCPClient, error) {
	if s == nil {
		return nil, errors.New("kcpclients: the kcp client set is nil")
	}
	workspace = kcpWorkspace(workspace)
	s.mu.RLock()
	cfg := s.configs[workspace]
	base := s.base
	s.mu.RUnlock()
	if cfg == nil {
		cfg = base
	}
	if cfg == nil {
		return nil, errors.New("kcpclients: the kcp client set has no rest config")
	}
	return &kcpClient{workspace: workspace, cfg: rest.CopyConfig(cfg)}, nil
}

type kcpClient struct {
	workspace string

	cfg *rest.Config
}

func (c *kcpClient) Workspace() string { return c.workspace }

func (c *kcpClient) RESTConfig() *rest.Config { return rest.CopyConfig(c.cfg) }

func kcpConfig(cfg *rest.Config) (*rest.Config, error) {
	if cfg == nil {
		return nil, errors.New("kcpclients: the kcp clients need a rest config")
	}
	out := rest.CopyConfig(cfg)
	out.Host = ref.BaseHost(out.Host)
	if out.Host == "" {
		return nil, errors.New("kcpclients: the kcp rest config has no host")
	}
	return out, nil
}

func kcpWorkspace(workspace string) string {
	if workspace == "" {
		return "root"
	}
	return workspace
}
