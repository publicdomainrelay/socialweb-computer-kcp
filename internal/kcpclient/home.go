package kcpclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	kcpv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/clusterhome"
)

func (c *Client) RemoveHome(ctx context.Context, home string) (bool, error) {
	if _, err := clusterhome.Key(home); err != nil {
		return false, err
	}
	i := strings.LastIndex(home, ":")
	parent, name := home[:i], home[i+1:]
	_, err := c.readCluster(ctx, home, name)
	switch {
	case err == nil:
		return false, nil
	case !apierrors.IsNotFound(err):
		return false, fmt.Errorf("kcpclient: the cluster of %s: %w", home, err)
	}
	claims, err := c.ListNodeClaims(ctx, home)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if len(claims) > 0 {
		return false, nil
	}
	if _, err := c.DropAgentHome(ctx, home); err != nil {
		return false, err
	}
	r, err := c.resource(parent, tenancyResource)
	if err != nil {
		return false, err
	}
	err = r.Delete().Resource("workspaces").Name(name).Do(ctx).Error()
	if err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("kcpclient: delete the home workspace %s: %w", home, err)
	}
	return true, nil
}

// ponytail: one list per account per sweep; a wildcard list of the home workspaces once accounts reach the thousands
func (c *Client) SweepHomes(ctx context.Context, live map[string]bool, grace time.Duration) ([]string, error) {
	if c == nil {
		return nil, nil
	}
	r, err := c.resource(clusterhome.RootWorkspace, tenancyResource)
	if err != nil {
		return nil, err
	}
	var accounts kcpv1alpha1.WorkspaceList
	if err := r.Get().Resource("workspaces").Do(ctx).Into(&accounts); err != nil {
		return nil, fmt.Errorf("kcpclient: list the account workspaces: %w", err)
	}
	var removed []string
	var errs []error
	for _, account := range accounts.Items {
		if account.Spec.Type == nil || account.Spec.Type.Name != AccountWorkspaceType || account.DeletionTimestamp != nil {
			continue
		}
		path := clusterhome.RootWorkspace + ":" + account.Name
		ar, err := c.resource(path, tenancyResource)
		if err != nil {
			return removed, err
		}
		var homes kcpv1alpha1.WorkspaceList
		if err := ar.Get().Resource("workspaces").Do(ctx).Into(&homes); err != nil {
			errs = append(errs, fmt.Errorf("kcpclient: list the homes in %s: %w", path, err))
			continue
		}
		for _, ws := range homes.Items {
			home := path + ":" + ws.Name
			key, err := clusterhome.Key(home)
			if err != nil || live[key] || ws.Spec.Type == nil || ws.Spec.Type.Name != HomeWorkspaceType ||
				ws.DeletionTimestamp != nil || time.Since(ws.CreationTimestamp.Time) < grace {
				continue
			}
			ok, err := c.RemoveHome(ctx, home)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", home, err))
			}
			if ok {
				removed = append(removed, home)
			}
		}
	}
	return removed, errors.Join(errs...)
}
