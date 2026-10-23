package kcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const marketIdentities = "marketidentities"

const (
	approvedField = "approvedAssociations"

	rejectedField = "rejectedAssociations"
)

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
	return m.decide(ctx, name, approvedField, callerDid)
}

func (m *MarketIdentities) RejectAssociation(ctx context.Context, name, callerDid string) error {
	return m.decide(ctx, name, rejectedField, callerDid)
}

func (m *MarketIdentities) decide(ctx context.Context, name, field, did string) error {
	did = strings.TrimSpace(did)
	if did == "" {
		return fmt.Errorf("kcpclient: marketidentity %s: an empty did cannot be added to spec.%s", name, field)
	}
	obj, err := m.GetMarketIdentity(ctx, name)
	if err != nil {
		return err
	}
	owned := ownedSetItems(obj.ManagedFields, v1alpha1.FieldManagerMarketIdentityApproval, approvedField, rejectedField)
	if slices.Contains(owned[field], did) {
		return nil
	}
	owned[field] = append(owned[field], did)
	spec := map[string]any{}
	for _, f := range []string{approvedField, rejectedField} {
		if len(owned[f]) > 0 {
			spec[f] = owned[f]
		}
	}
	doc := map[string]any{"apiVersion": v1alpha1.GroupVersion.String(), "kind": "MarketIdentity",
		"metadata": map[string]any{"name": name}, "spec": spec}
	req := m.r.Patch(types.ApplyPatchType).Resource(marketIdentities).Name(name)
	if err := applyDoc(ctx, req, v1alpha1.FieldManagerMarketIdentityApproval, false, doc, false); err != nil {
		return fmt.Errorf("kcpclient: marketidentity %s: apply spec.%s: %w", name, field, err)
	}
	return nil
}

func ownedSetItems(entries []metav1.ManagedFieldsEntry, manager string, fields ...string) map[string][]string {
	out := map[string][]string{}
	for _, e := range entries {
		if e.Manager != manager || e.Operation != metav1.ManagedFieldsOperationApply || e.Subresource != "" || e.FieldsV1 == nil {
			continue
		}
		var set map[string]map[string]map[string]any
		if json.Unmarshal(e.FieldsV1.Raw, &set) != nil {
			continue
		}
		for _, f := range fields {
			for key := range set["f:spec"]["f:"+f] {
				raw, ok := strings.CutPrefix(key, "v:")
				if !ok {
					continue
				}
				var item string
				if json.Unmarshal([]byte(raw), &item) == nil && item != "" && !slices.Contains(out[f], item) {
					out[f] = append(out[f], item)
				}
			}
		}
	}
	for _, f := range fields {
		slices.Sort(out[f])
	}
	return out
}
