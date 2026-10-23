package ccmmain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/cloudprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/tenantnode"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/foldmain"
)

func logTo(logger *slog.Logger) func(string, map[string]any) {
	return func(event string, fields map[string]any) {
		attrs := make([]any, 0, len(fields)*2)
		for k, v := range fields {
			attrs = append(attrs, k, v)
		}
		logger.Info(event, attrs...)
	}
}

func Main(ctx context.Context, inv foldmain.Invocation) int {
	if err := run(ctx, inv); err != nil {
		fmt.Fprintln(inv.Stderr, "socialweb-cloud-controller-manager:", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, inv foldmain.Invocation) error {
	cfg, err := load(inv.Env)
	if err != nil {
		return err
	}
	log := logTo(slog.New(slog.NewJSONHandler(inv.Stderr, nil)))

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	restCfg, err := clientcmd.BuildConfigFromFlags("", cfg.TenantKubeconfig)
	if err != nil {
		return fmt.Errorf(
			"socialweb-cloud-controller-manager: reading the minted tenant kubeconfig at %s: %w",
			cfg.TenantKubeconfig, err)
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf(
			"socialweb-cloud-controller-manager: building the client for the tenant cluster %s "+
				"from %s: %w", cfg.Cluster, cfg.TenantKubeconfig, err)
	}

	reconciler, err := cloudprovider.NewReconciler(cloudprovider.Options{
		Cluster:   cfg.Cluster,
		Instances: cloudprovider.NewFileInstances(cfg.InstancesFile),
		Nodes:     tenantnode.NewClientNodeWriter(client),
		Log:       log,
	})
	if err != nil {
		return err
	}
	changes, err := cloudprovider.WatchSnapshot(ctx, cfg.InstancesFile)
	if err != nil {
		return err
	}
	controller, err := tenantnode.New(tenantnode.Options{
		Reconciler:      reconciler,
		InstanceChanges: changes,
		Log:             log,
	}, client)
	if err != nil {
		return err
	}

	log("cloud controller manager: status", map[string]any{
		"phase":         "Starting",
		"cluster":       cfg.Cluster,
		"tenant":        cfg.TenantKubeconfig,
		"instancesFile": cfg.InstancesFile,
		"maxNodes":      cfg.MaxNodes,
		"podsPerNode":   cfg.PodsPerNode,
	})

	if err := controller.Run(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}
