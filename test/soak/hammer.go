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
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

func runHammer(args []string) int {
	fs := flag.NewFlagSet("hammer", flag.ExitOnError)
	instance := fs.String("instance", "auditp", "dev instance")
	stateDir := fs.String("state-dir", "/var/tmp/swcit/auditp", "state dir of the instance")
	workers := fs.Int("workers", 64, "parallel clients")
	duration := fs.Duration("duration", 5*time.Minute, "how long")
	fs.Parse(args)
	t, err := openTarget(*instance, *stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	base, err := kcpserver.FrontRestConfig(t.record.AdminKubeconfig)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cfg := rest.CopyConfig(base)
	cfg.Host = strings.TrimSuffix(base.Host, "/") + "/clusters/" + appctl.DefaultWorkspace
	cfg.QPS, cfg.Burst = 100000, 100000
	cfg.Timeout = 10 * time.Second
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	var n atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).List(ctx, metav1.ListOptions{Limit: 1})
				n.Add(1)
			}
		}()
	}
	wg.Wait()
	fmt.Println("requests", n.Load())
	return 0
}
