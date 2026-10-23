package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	clientfeatures "k8s.io/client-go/features"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/config"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/providers/multi"

	"github.com/kcp-dev/multicluster-provider/apiexport"
	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
)

const (
	FieldManagerPrefix = "socialweb.computer/"

	DefaultExportWorkspace = "root:socialweb-provider"

	clusterSeparator = "#"

	pathAnnotation = "kcp.io/path"
)

var logicalClusterGVR = schema.GroupVersionResource{Group: "core.kcp.io", Version: "v1alpha1", Resource: "logicalclusters"}

type Config struct {
	Rest *rest.Config

	ExportWorkspace string

	AddToScheme []func(*kruntime.Scheme) error

	Log *slog.Logger
}

type Manager struct {
	base *rest.Config

	exportWorkspace string

	scheme *kruntime.Scheme

	log *slog.Logger

	logr logr.Logger

	providers *multi.Provider

	mgr mcmanager.Manager

	mu sync.Mutex

	exports map[string]bool

	paths sync.Map
}

var (
	setLogger sync.Once

	setGates sync.Once
)

type noWatchList struct {
	clientfeatures.Gates
}

func (g noWatchList) Enabled(key clientfeatures.Feature) bool {
	if key == clientfeatures.WatchListClient {
		return false
	}
	return g.Gates.Enabled(key)
}

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
	if exportWorkspace == "" {
		exportWorkspace = DefaultExportWorkspace
	}
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
	setGates.Do(func() {
		if clientfeatures.FeatureGates().Enabled(clientfeatures.WatchListClient) {
			clientfeatures.ReplaceFeatureGates(noWatchList{Gates: clientfeatures.FeatureGates()})
		}
	})
	providers := multi.New(multi.Options{Separator: clusterSeparator})
	mgr, err := mcmanager.New(clusterConfig(base, exportWorkspace), providers, manager.Options{
		Scheme:     scheme,
		Logger:     logger,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		return nil, fmt.Errorf("runtime: the multicluster manager: %w", err)
	}
	return &Manager{base: base, exportWorkspace: exportWorkspace, scheme: scheme, log: log, logr: logger,
		providers: providers, mgr: mgr, exports: map[string]bool{}}, nil
}

func (m *Manager) Start(ctx context.Context) error {
	m.log.Info("the controller manager starts", "exportWorkspace", m.exportWorkspace)
	return m.mgr.Start(ctx)
}

func (m *Manager) addExport(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.exports[name] {
		return nil
	}
	logger := m.logr.WithValues("export", name)
	p, err := apiexport.New(clusterConfig(m.base, m.exportWorkspace), name, apiexport.Options{Scheme: m.scheme, Log: &logger})
	if err != nil {
		return fmt.Errorf("runtime: the provider of APIExport %s: %w", name, err)
	}
	if err := m.providers.AddProvider(name, p); err != nil {
		return fmt.Errorf("runtime: adding the provider of APIExport %s: %w", name, err)
	}
	m.exports[name] = true
	m.log.Info("the controller manager serves an APIExport", "export", name, "endpointSlice", m.exportWorkspace+":"+name)
	return nil
}

func (m *Manager) path(ctx context.Context, cluster string) string {
	if v, ok := m.paths.Load(cluster); ok {
		return v.(string)
	}
	dyn, err := dynamic.NewForConfig(clusterConfig(m.base, cluster))
	if err != nil {
		return cluster
	}
	lc, err := dyn.Resource(logicalClusterGVR).Get(ctx, "cluster", metav1.GetOptions{})
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
