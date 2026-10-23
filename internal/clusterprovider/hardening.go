package clusterprovider

import (
	"context"
	"errors"
	"fmt"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/procgroup"
)

const (
	PSAEnforceLabel = "pod-security.kubernetes.io/enforce"
	PSAWarnLabel    = "pod-security.kubernetes.io/warn"

	PSAEnforceValue = "baseline"
	PSAWarnValue    = "restricted"

	DefaultServiceAccountName = "default"
)

var ErrNoHardeningCredential = errors.New(
	"clusterprovider: no client for the cluster's own apiserver was supplied, so its PSA labels " +
		"and its default ServiceAccount's automountServiceAccountToken=false can neither be " +
		"written nor read back; a cluster whose hardening is not in force is not one this " +
		"provider will hold at Ready")

var ErrHardeningNotInForce = errors.New(
	"clusterprovider: the cluster answered and its hardening is NOT in force")

var ErrHardeningRaced = errors.New(
	"clusterprovider: the default ServiceAccount appeared between the read and the write on " +
		"every attempt, so automountServiceAccountToken=false has nowhere to land")

var ErrNoHardeningSeam = errors.New(
	"clusterprovider: this Stack puts no k3s step in the group, so the hardening has no gate to " +
		"run behind and would SILENTLY NOT RUN -- a cluster hardened by nothing and a cluster " +
		"hardened by this step look identical from a green suite")

func hardenAfterK3s(steps []procgroup.Step, admin AdminClient, cluster, apiserver string) ([]procgroup.Step, error) {
	idx := -1
	for i, s := range steps {
		if s.Name == procgroup.ComponentK3s {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, ErrNoHardeningSeam
	}
	if admin == nil {
		return nil, ErrNoHardeningCredential
	}
	inner := steps[idx].Ready
	steps[idx].Ready = func(ctx context.Context, p procgroup.Member) error {
		if inner != nil {
			if err := inner(ctx, p); err != nil {
				return err
			}
		}
		core, err := admin(ctx, cluster, apiserver)
		if err != nil {
			return err
		}
		err = ErrNoHardeningCredential
		if core != nil {
			err = core.Harden(ctx)
		}
		if err != nil {
			return fmt.Errorf("clusterprovider: %s answered at %s and %w", cluster, apiserver, err)
		}
		return nil
	}
	return steps, nil
}
