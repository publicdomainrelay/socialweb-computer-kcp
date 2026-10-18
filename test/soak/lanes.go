//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/gatewaykcp"
)

var (
	clusterGVR   = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "clusters"}
	workspaceGVR = schema.GroupVersionResource{Group: "tenancy.kcp.io", Version: "v1alpha1", Resource: "workspaces"}
)

func pathClient(kubeconfig, path string) (dynamic.Interface, error) {
	cfg, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	c := rest.CopyConfig(cfg)
	c.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + path
	c.Timeout = 30 * time.Second
	return dynamic.NewForConfig(c)
}

func deleteTenantCluster(ctx context.Context, adminKubeconfig, did, name string) error {
	home, err := pathClient(adminKubeconfig, kcpclient.HomePath(did, name))
	if err != nil {
		return err
	}
	if err := home.Resource(clusterGVR).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	for deadline := time.Now().Add(10 * time.Minute); ; {
		_, err := home.Resource(clusterGVR).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cluster %s still present after 10m", name)
		}
		if !sleepCtx(ctx, 3*time.Second) {
			return ctx.Err()
		}
	}
	acct, err := pathClient(adminKubeconfig, kcpclient.AccountPath(did))
	if err != nil {
		return err
	}
	if err := acct.Resource(workspaceGVR).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func tenantLane(ctx context.Context, lane int, gatewayDir, did, base, admin string, c *counters, hold time.Duration, logf func(string)) {
	members := filepath.Join(gatewayDir, "kcp", gatewaykcp.MemberKubeconfig)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	bad := 0
	for seq := 0; ctx.Err() == nil; seq++ {
		name := fmt.Sprintf("l%dc%d", lane, seq)
		c.tenantCycles.Add(1)
		err := tenantRound(ctx, members, base, did, name, gatewayDir, hold, c, quiet)
		if derr := deleteTenantCluster(context.WithoutCancel(ctx), admin, did, name); derr != nil {
			logf(fmt.Sprintf("lane %d: delete %s: %v", lane, name, derr))
		} else {
			c.clustersDeleted.Add(1)
		}
		if err != nil {
			c.tenantFailed.Add(1)
			bad++
			logf(fmt.Sprintf("lane %d: %s: %v", lane, name, err))
			wait := time.Duration(bad) * time.Minute
			if wait > 5*time.Minute {
				wait = 5 * time.Minute
			}
			sleepCtx(ctx, wait)
			continue
		}
		bad = 0
	}
}

func tenantRound(ctx context.Context, members, base, did, name, gatewayDir string, hold time.Duration, c *counters, log *slog.Logger) error {
	if err := gatewaykcp.BootstrapCluster(ctx, members, base, did, name, log); err != nil {
		return err
	}
	c.clustersCreated.Add(1)
	var kube *kubernetes.Clientset
	for deadline := time.Now().Add(5 * time.Minute); ; {
		k, err := openTenantKube(gatewayDir, name)
		if err == nil {
			if _, err = k.Discovery().RESTClient().Get().AbsPath("/readyz").DoRaw(ctx); err == nil {
				kube = k
				break
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cluster %s not ready within 5m: %v", name, err)
		}
		if !sleepCtx(ctx, 3*time.Second) {
			return ctx.Err()
		}
	}
	out, in, err := tenantCycle(ctx, kube, 0, hold, 8*time.Minute, 6*time.Minute)
	if out {
		c.tenantOut.Add(1)
	}
	if in {
		c.tenantIn.Add(1)
	}
	return err
}
