package runtime

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/config"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/providers/multi"

	"github.com/johnandersen777/socialweb-computer-kcp/api/kcpgvr"
	"github.com/kcp-dev/multicluster-provider/apiexport"
	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
)

const (
	FieldManagerPrefix = "socialweb.computer/"

	DefaultExportWorkspace = "root:socialweb-provider"

	clusterSeparator = "#"

	pathAnnotation = "kcp.io/path"
)

type Config struct {
	Rest *rest.Config

	ExportWorkspace string

	AddToScheme []func(*kruntime.Scheme) error

	Workspaces func(workspace string) bool

	Log *slog.Logger
}

type Manager struct {
	base *rest.Config

	exportWorkspace string

	workspaces func(workspace string) bool

	scheme *kruntime.Scheme

	log *slog.Logger

	logr logr.Logger

	providers *multi.Provider

	mgr mcmanager.Manager

	mu sync.Mutex

	exports map[string]bool

	ctx context.Context

	exportsWG sync.WaitGroup

	paths sync.Map
}

var setLogger sync.Once

func NewManager(cfg Config) (*Manager, error) {
	if cfg.Rest == nil {
		return nil, errors.New("runtime: a kcp rest config is required")
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("component", "controller-manager")
	exportWorkspace := strings.TrimSpace(cfg.ExportWorkspace)
	exportWorkspace = cmp.Or(exportWorkspace, DefaultExportWorkspace)
	base := rest.CopyConfig(cfg.Rest)
	host, _, _ := strings.Cut(strings.TrimSuffix(base.Host, "/"), "/clusters/")
	base.Host = host
	scheme := kruntime.NewScheme()
	adders := append([]func(*kruntime.Scheme) error{clientgoscheme.AddToScheme, apisv1alpha1.AddToScheme}, cfg.AddToScheme...)
	for _, add := range adders {
		if add == nil {
			continue
		}
		if err := add(scheme); err != nil {
			return nil, fmt.Errorf("runtime: building the scheme: %w", err)
		}
	}
	logger := logr.FromSlogHandler(log.Handler())
	setLogger.Do(func() { ctrllog.SetLogger(logger) })
	providers := multi.New(multi.Options{Separator: clusterSeparator})
	mgr, err := mcmanager.New(clusterConfig(base, exportWorkspace), providers, manager.Options{
		Scheme:     scheme,
		Logger:     logger,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		return nil, fmt.Errorf("runtime: the multicluster manager: %w", err)
	}
	return &Manager{base: base, exportWorkspace: exportWorkspace, workspaces: cfg.Workspaces, scheme: scheme, log: log, logr: logger,
		providers: providers, mgr: mgr, exports: map[string]bool{}}, nil
}

func (m *Manager) AddToScheme(adders ...func(*kruntime.Scheme) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, add := range adders {
		if add == nil {
			continue
		}
		if err := add(m.scheme); err != nil {
			return fmt.Errorf("runtime: extending the scheme: %w", err)
		}
	}
	return nil
}

func (m *Manager) Start(ctx context.Context) error {
	exportCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.log.Info("the controller manager starts", "exportWorkspace", m.exportWorkspace)
	m.mu.Lock()
	m.ctx = exportCtx
	for name := range m.exports {
		m.exportsWG.Go(func() { m.serveExport(exportCtx, name) })
	}
	m.mu.Unlock()
	err := m.mgr.Start(ctx)
	m.mu.Lock()
	m.ctx = nil
	m.mu.Unlock()
	cancel()
	m.exportsWG.Wait()
	return err
}

func (m *Manager) addExport(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.exports[name] {
		return
	}
	m.exports[name] = true
	if ctx := m.ctx; ctx != nil {
		m.exportsWG.Go(func() { m.serveExport(ctx, name) })
	}
}

func (m *Manager) serveExport(ctx context.Context, name string) {
	logger := m.logr.WithValues("export", name)
	retry := newBackoff(DefaultBackoffBase, DefaultBackoffMax)
	for failures := 0; ctx.Err() == nil; failures++ {
		p, err := apiexport.New(clusterConfig(m.base, m.exportWorkspace), name, apiexport.Options{Scheme: m.scheme, Log: &logger})
		if err == nil {
			err = m.providers.AddProvider(name, p)
		}
		if err == nil {
			m.log.Info("the controller manager serves an APIExport", "export", name, "endpointSlice", m.exportWorkspace+":"+name)
			return
		}
		retryIn := retry.delay(failures)
		m.log.Warn("the APIExport provider does not start yet; it retries with backoff", "export", name,
			"workspace", m.exportWorkspace, "failures", failures+1, "retryIn", retryIn.String(), "err", err.Error())
		timer := time.NewTimer(retryIn)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (m *Manager) path(ctx context.Context, cluster string) string {
	if v, ok := m.paths.Load(cluster); ok {
		return v.(string)
	}
	dyn, err := dynamic.NewForConfig(clusterConfig(m.base, cluster))
	if err != nil {
		return cluster
	}
	lc, err := dyn.Resource(kcpgvr.LogicalClusters).Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return cluster
	}
	path := strings.TrimSpace(lc.GetAnnotations()[pathAnnotation])
	if path == "" {
		return cluster
	}
	m.paths.Store(cluster, path)
	return path
}

func clusterConfig(base *rest.Config, cluster string) *rest.Config {
	out := rest.CopyConfig(base)
	out.Host = strings.TrimSuffix(base.Host, "/") + "/clusters/" + cluster
	return out
}

func splitCluster(name string) (string, string) {
	export, cluster, ok := strings.Cut(name, clusterSeparator)
	if !ok {
		return "", name
	}
	return export, cluster
}
