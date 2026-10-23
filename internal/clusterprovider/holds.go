package clusterprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/apiserverfront"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/portallocator"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/procgroup"
)

const (
	AnnotationHeldPorts = "socialweb.computer/held-ports"
	HoldTimeout         = 15 * time.Second
)

func (cfg StackConfig) FrontStep(cluster string) (procgroup.Step, error) {
	dataDir, err := ClusterDataDir(cfg.StateDir, cluster)
	if err != nil {
		return procgroup.Step{}, err
	}
	return frontStep(clusterstack.Spec{
		DataDir:      dataDir,
		Cluster:      cluster,
		BindAddress:  cfg.BindAddress,
		FrontExec:    cfg.FrontExec,
		FrontControl: FrontControlPath(cfg.StateDir, dataDir, cluster),
		FrontState:   FrontStatePath(dataDir),
	})
}

func frontStep(spec clusterstack.Spec) (procgroup.Step, error) {
	argv, err := spec.FrontArgv()
	if err != nil {
		return procgroup.Step{}, err
	}
	log, err := componentLog(spec.DataDir, procgroup.ComponentApiserverFront)
	if err != nil {
		return procgroup.Step{}, err
	}
	control := spec.FrontControl
	return procgroup.Step{
		Name: procgroup.ComponentApiserverFront,
		Spec: procgroup.Spec{Argv: argv, Log: log},
		Ready: func(ctx context.Context, _ procgroup.Member) error {
			ctx, cancel := context.WithTimeout(ctx, GateTimeout)
			defer cancel()
			return waitFront(ctx, control)
		},
	}, nil
}

func (h *UnitsHost) Hold(key string, p portallocator.Purpose) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), HoldTimeout)
	defer cancel()
	if p == portallocator.PurposeK3sSecure {
		return h.frontPort(ctx, key)
	}
	ctl, err := h.FrontControl(key)
	if err != nil {
		return 0, err
	}
	port, err := (apiserverfront.Client{Path: ctl}).Hold(ctx, string(p))
	if err != nil {
		return 0, fmt.Errorf("clusterprovider: the apiserver front of %s holds no port for %s: %w", key, p, err)
	}
	if err := h.recordHold(ctx, key, p, port); err != nil {
		return 0, err
	}
	return port, nil
}

func (h *UnitsHost) frontPort(ctx context.Context, key string) (int, error) {
	member := MemberName(procgroup.ComponentApiserverFront)
	last := "no answer"
	for {
		o, err := h.member(ctx, key, member)
		switch {
		case err != nil:
			last = err.Error()
		case o == nil:
			return 0, fmt.Errorf("clusterprovider: %s has no apiserver front HostProcess, so nothing holds a port for its apiserver", key)
		case o.DeletionTimestamp != nil:
			return 0, fmt.Errorf("clusterprovider: the apiserver front of %s is being deleted", key)
		case o.Status.Phase == v1alpha1.HostProcessPhaseFailed || o.Status.Phase == v1alpha1.HostProcessPhaseLost ||
			o.Status.Phase == v1alpha1.HostProcessPhaseStopped:
			return 0, fmt.Errorf("clusterprovider: the apiserver front of %s is %s %s: %s", key, o.Status.Phase,
				o.Status.LostReason, o.Status.Message)
		default:
			if port, ok := tcpListenerPort(o.Status.Listeners); ok {
				return port, nil
			}
			last = fmt.Sprintf("phase %s, listeners %v", o.Status.Phase, o.Status.Listeners)
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("clusterprovider: the apiserver front of %s reported no listener into its HostProcess status (last: %s)", key, last)
		case <-time.After(unitPoll):
		}
	}
}

func tcpListenerPort(ls []v1alpha1.HostProcessListener) (int, bool) {
	for _, l := range ls {
		if l.Name != "tcp" {
			continue
		}
		_, p, err := net.SplitHostPort(l.Address)
		if err != nil {
			return 0, false
		}
		port, err := strconv.Atoi(p)
		if err != nil || port <= 0 {
			return 0, false
		}
		return port, true
	}
	return 0, false
}

func (h *UnitsHost) recordHold(ctx context.Context, key string, p portallocator.Purpose, port int) error {
	name := ObjectName(key, MemberName(procgroup.ComponentApiserverFront))
	for range 5 {
		u, err := h.hps.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("clusterprovider: recording hold %s of %s on %s: %w", p, key, name, err)
		}
		ann := u.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		held := map[string]int{}
		if raw := ann[AnnotationHeldPorts]; raw != "" {
			_ = json.Unmarshal([]byte(raw), &held)
		}
		if held[string(p)] == port {
			return nil
		}
		held[string(p)] = port
		b, err := json.Marshal(held)
		if err != nil {
			return err
		}
		ann[AnnotationHeldPorts] = string(b)
		u.SetAnnotations(ann)
		if _, err = h.hps.Update(ctx, u, metav1.UpdateOptions{}); err == nil || !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("clusterprovider: recording hold %s of %s on %s kept conflicting", p, key, name)
}
