//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vmruntime"
)

var marketNetworkGVR = schema.GroupVersionResource{Group: "socialweb.computer", Version: "v1alpha1", Resource: "marketnetworks"}

const (
	marketNetworkName = "network"
	stoppedPrefix     = "stopped:"
)

func (n *Network) bidderName(i int) string {
	return fmt.Sprintf("bidder-%d", i+1)
}

func (n *Network) networkClient() (dynamic.ResourceInterface, error) {
	kc, err := n.read(n.Up.AdminKubeconfig)
	if err != nil {
		return nil, err
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kc)
	if err != nil {
		return nil, err
	}
	cfg.Host = strings.TrimSuffix(strings.SplitN(cfg.Host, "/clusters/", 2)[0], "/") + "/clusters/root"
	cfg.Timeout = 15 * time.Second
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return dyn.Resource(marketNetworkGVR), nil
}

func (n *Network) SetBidderStopped(ctx context.Context, i int, stopped bool) error {
	ri, err := n.networkClient()
	if err != nil {
		return err
	}
	name := n.bidderName(i)
	for range 10 {
		u, err := ri.Get(ctx, marketNetworkName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading the MarketNetwork: %w", err)
		}
		apps, _, err := unstructured.NestedSlice(u.Object, "spec", "apps")
		if err != nil {
			return err
		}
		found := false
		for j, a := range apps {
			m, ok := a.(map[string]any)
			if !ok || m["name"] != name {
				continue
			}
			if stopped {
				m["stopped"] = true
			} else {
				delete(m, "stopped")
			}
			apps[j] = m
			found = true
		}
		if !found {
			return fmt.Errorf("the MarketNetwork has no application %s", name)
		}
		if err := unstructured.SetNestedSlice(u.Object, apps, "spec", "apps"); err != nil {
			return err
		}
		if _, err := ri.Update(ctx, u, metav1.UpdateOptions{}); err == nil {
			return nil
		} else if !apierrors.IsConflict(err) {
			return fmt.Errorf("updating the MarketNetwork: %w", err)
		}
	}
	return fmt.Errorf("the MarketNetwork stayed in conflict")
}

func (n *Network) WaitBidderStopped(i int, timeout time.Duration) error {
	name := n.bidderName(i)
	deadline := time.Now().Add(timeout)
	for {
		if a, ok := n.App(name); ok && strings.HasPrefix(a.Message, stoppedPrefix) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not stop inside %s", name, timeout)
		}
		time.Sleep(time.Second)
	}
}

func (n *Network) WaitBidderReady(i int, timeout time.Duration) error {
	name := n.bidderName(i)
	deadline := time.Now().Add(timeout)
	var last string
	for {
		if a, ok := n.App(name); ok {
			last = fmt.Sprintf("phase %s: %s", a.Phase, a.Message)
			if a.Phase == v1alpha1.ApplicationPhaseReady && a.Did != "" {
				for _, c := range a.Components {
					if c.Kind == "Bidder" && c.Name == "bidder" && c.Ready {
						return nil
					}
					if c.Kind == "Bidder" && c.Name == "bidder" {
						last = fmt.Sprintf("Bidder component phase %s, ready %t", c.Phase, c.Ready)
					}
				}
			}
		}
		if err := n.health(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s was not Ready with a ready Bidder component inside %s (%s)", name, timeout, last)
		}
		time.Sleep(time.Second)
	}
}

func (n *Network) BidderDid(i int) (string, error) {
	a, ok := n.App(n.bidderName(i))
	if !ok || a.Did == "" {
		return "", fmt.Errorf("the MarketNetwork reports no DID for %s", n.bidderName(i))
	}
	return a.Did, nil
}

func (n *Network) bidderUnits(i int) string {
	r := n.h.As(context.Background(), "systemctl", "--user", "list-units", "--all", "--plain", "--no-legend", "--no-pager",
		vmruntime.UnitPrefix(n.Handles[i])+"*")
	return strings.TrimSpace(r.Stdout)
}
