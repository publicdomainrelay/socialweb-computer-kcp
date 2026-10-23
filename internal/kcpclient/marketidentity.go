package kcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const marketIdentities = "marketidentities"

const specListAttempts = 3

type MarketIdentities struct {
	r rest.Interface
}

func (c *Client) MarketIdentities(logicalCluster string) (*MarketIdentities, error) {
	r, err := c.resource(logicalCluster, socialwebResource)
	if err != nil {
		return nil, err
	}
	return &MarketIdentities{r: r}, nil
}

func (m *MarketIdentities) GetMarketIdentity(ctx context.Context, name string) (*v1alpha1.MarketIdentity, error) {
	if name == "" {
		return nil, errors.New("kcpclient: GetMarketIdentity needs a name")
	}
	var out v1alpha1.MarketIdentity
	if err := m.r.Get().Resource(marketIdentities).Name(name).Do(ctx).Into(&out); err != nil {
		return nil, fmt.Errorf("kcpclient: read marketidentity %s: %w", name, err)
	}
	return &out, nil
}

func (m *MarketIdentities) ApproveAssociation(ctx context.Context, name, callerDid string) error {
	return m.addToSpecList(ctx, name, "approvedAssociations", callerDid, func(s *v1alpha1.MarketIdentitySpec) []string { return s.ApprovedAssociations })
}

func (m *MarketIdentities) RejectAssociation(ctx context.Context, name, callerDid string) error {
	return m.addToSpecList(ctx, name, "rejectedAssociations", callerDid, func(s *v1alpha1.MarketIdentitySpec) []string { return s.RejectedAssociations })
}

func (m *MarketIdentities) addToSpecList(ctx context.Context, name, field, did string, current func(*v1alpha1.MarketIdentitySpec) []string) error {
	if did == "" {
		return fmt.Errorf("kcpclient: marketidentity %s: an empty did cannot be added to spec.%s", name, field)
	}
	for range specListAttempts {
		obj, err := m.GetMarketIdentity(ctx, name)
		if err != nil {
			return err
		}
		list := current(&obj.Spec)
		if slices.Contains(list, did) {
			return nil
		}
		body, err := json.Marshal([]map[string]any{
			{"op": "test", "path": "/metadata/resourceVersion", "value": obj.ResourceVersion},
			{"op": "add", "path": "/spec/" + field, "value": append(slices.Clone(list), did)},
		})
		if err != nil {
			return err
		}
		err = m.r.Patch(types.JSONPatchType).Resource(marketIdentities).Name(name).Body(body).Do(ctx).Error()
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) && !apierrors.IsInvalid(err) {
			return fmt.Errorf("kcpclient: marketidentity %s: patch spec.%s: %w", name, field, err)
		}
	}
	return fmt.Errorf("kcpclient: marketidentity %s: spec.%s kept changing under %d attempts", name, field, specListAttempts)
}
