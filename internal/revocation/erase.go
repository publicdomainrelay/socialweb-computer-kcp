package revocation

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type Eraser interface {
	DeleteServiceAccount(ctx context.Context, did, cluster string) error
}

var ErrNoEraser = errors.New(
	"revocation: the deny IS written, but no Eraser is wired: the ServiceAccount cannot be removed in this " +
		"build, so every cluster that still holds one stays RevocationPending")

func Erase(ctx context.Context, er Eraser, did string, effects []Effect) ([]Effect, error) {
	if er == nil {
		for _, e := range effects {
			if e.DeleteSA {
				return effects, ErrNoEraser
			}
		}
		return effects, nil
	}
	applied := make([]Effect, 0, len(effects))
	var errs []error
	for _, e := range effects {
		if e.DeleteSA {
			if err := er.DeleteServiceAccount(ctx, did, e.Cluster); err != nil {
				errs = append(errs, fmt.Errorf("cluster %s: %w", e.Cluster, err))
				applied = append(applied, e)
				continue
			}
			e.DeleteSA = false
			e.State = v1alpha1.IdentityRevoked
		}
		applied = append(applied, e)
	}
	return applied, errors.Join(errs...)
}

type SettleView struct {
	Name string

	Settled bool

	Serving bool
}

func Settle(views []SettleView) []Effect {
	out := make([]Effect, 0, len(views))
	for _, v := range views {
		switch {
		case v.Settled:
			out = append(out, Effect{Cluster: v.Name, State: v1alpha1.IdentityRevoked})
		case v.Serving:
			out = append(out, Effect{Cluster: v.Name, State: v1alpha1.IdentityRevocationPending, DeleteSA: true})
		default:
			out = append(out, Effect{Cluster: v.Name, State: v1alpha1.IdentityColdDeferred})
		}
	}
	return out
}

func AccountState(effects []Effect) v1alpha1.IdentityState {
	state := v1alpha1.IdentityRevoked
	for _, e := range effects {
		switch e.State {
		case v1alpha1.IdentityRevocationPending:
			return v1alpha1.IdentityRevocationPending
		case v1alpha1.IdentityColdDeferred:
			state = v1alpha1.IdentityColdDeferred
		}
	}
	return state
}

func Detail(effects []Effect) string {
	if len(effects) == 0 {
		return "the identity holds no cluster"
	}
	parts := make([]string, 0, len(effects))
	for _, e := range effects {
		parts = append(parts, e.Cluster+"="+string(e.State))
	}
	return strings.Join(parts, " ")
}

func Revoked(state v1alpha1.IdentityState) bool {
	switch state {
	case v1alpha1.IdentityRevoked, v1alpha1.IdentityRevocationPending, v1alpha1.IdentityColdDeferred:
		return true
	}
	return false
}
