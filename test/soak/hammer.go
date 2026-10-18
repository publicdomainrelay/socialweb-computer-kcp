//go:build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

func runHammer(args []string) int {
	fs := flag.NewFlagSet("hammer", flag.ExitOnError)
	instance := fs.String("instance", "auditp", "dev instance")
	stateDir := fs.String("state-dir", "/var/tmp/swcit/auditp", "state dir of the instance")
	workers := fs.Int("workers", 64, "parallel clients")
	duration := fs.Duration("duration", 5*time.Minute, "how long")
	app := fs.String("app", "", "child instance whose kcp is hammered (empty: the network's)")
	mount := fs.String("mount", "", "tenant cluster of the child gateway's bootstrap account whose mounted workspace is read")
	every := fs.Duration("every", 0, "pause after each request of a worker")
	fs.Parse(args)
	t, err := openTarget(*instance, *stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	kubeconfig, path := t.record.AdminKubeconfig, appctl.DefaultWorkspace
	if *app != "" {
		rec, err := t.childRecord(*app)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		kubeconfig = rec.AdminKubeconfig
		if *mount != "" {
			if rec.Gateway == nil {
				fmt.Fprintln(os.Stderr, *app, "is no gateway")
				return 1
			}
			path = kcpclient.MountPath(rec.Gateway.BootstrapAccountDID, *mount)
		}
	}
	base, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cfg := rest.CopyConfig(base)
	cfg.Host = strings.TrimSuffix(base.Host, "/") + "/clusters/" + path
	cfg.QPS, cfg.Burst = 100000, 100000
	cfg.Timeout = 10 * time.Second
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	var n, failed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if _, err := dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).List(ctx, metav1.ListOptions{Limit: 1}); err != nil && ctx.Err() == nil {
					if failed.Add(1)%100 == 1 {
						fmt.Fprintln(os.Stderr, "request:", err)
					}
				}
				n.Add(1)
				if *every > 0 {
					sleepCtx(ctx, *every)
				}
			}
		}()
	}
	wg.Wait()
	fmt.Println("requests", n.Load(), "failed", failed.Load())
	return 0
}
