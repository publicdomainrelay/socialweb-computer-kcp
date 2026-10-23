package kcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const clusterLinks = "clusterlinks"

func (c *Client) WriteClusterLink(ctx context.Context, logicalCluster string, link v1alpha1.ClusterLink) error {
	r, err := c.resource(logicalCluster, socialwebResource)
	if err != nil {
		return err
	}
	return WriteClusterLinkTo(ctx, r, link)
}

func WriteClusterLinkTo(ctx context.Context, r rest.Interface, link v1alpha1.ClusterLink) error {
	switch {
	case link.Name == "":
		return errors.New("kcpclient: WriteClusterLink needs a link name")
	case link.Spec.ClusterRef.Name == "":
		return errors.New("kcpclient: WriteClusterLink needs the cluster the link fronts")
	case link.Spec.Service == "":
		return errors.New("kcpclient: WriteClusterLink needs the service the link forwards to")
	case link.Spec.TargetPort <= 0:
		return fmt.Errorf("kcpclient: WriteClusterLink %s needs the loopback port it forwards to", link.Name)
	case link.Status.Phase == "":
		return fmt.Errorf("kcpclient: WriteClusterLink %s needs a phase; the schema's phase is an enum with no empty member", link.Name)
	}

	var existing v1alpha1.ClusterLink
	err := r.Get().Resource(clusterLinks).Name(link.Name).Do(ctx).Into(&existing)
	switch {
	case apierrors.IsNotFound(err):
		return createClusterLink(ctx, r, link)
	case err != nil:
		return fmt.Errorf("kcpclient: read clusterlink %s: %w", link.Name, err)
	}
	if existing.Spec != link.Spec {
		body, err := json.Marshal(map[string]any{"spec": link.Spec})
		if err != nil {
			return fmt.Errorf("kcpclient: clusterlink %s: marshal: %w", link.Name, err)
		}
		if err := r.Patch(types.MergePatchType).Resource(clusterLinks).Name(link.Name).
			Body(body).Do(ctx).Error(); err != nil {
			return fmt.Errorf("kcpclient: patch clusterlink %s spec: %w", link.Name, err)
		}
	}
	if existing.Status == link.Status {
		return nil
	}
	return patchClusterLinkStatus(ctx, r, link.Name, link.Status)
}

func createClusterLink(ctx context.Context, r rest.Interface, link v1alpha1.ClusterLink) error {
	created := v1alpha1.ClusterLink{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "ClusterLink",
		},
		ObjectMeta: metav1.ObjectMeta{Name: link.Name},
		Spec:       link.Spec,
	}
	body, err := json.Marshal(created)
	if err != nil {
		return fmt.Errorf("kcpclient: clusterlink %s: marshal: %w", link.Name, err)
	}
	if err := r.Post().Resource(clusterLinks).Body(body).Do(ctx).Error(); err != nil {
		return fmt.Errorf("kcpclient: create clusterlink %s: %w", link.Name, err)
	}
	if err := patchClusterLinkStatus(ctx, r, link.Name, link.Status); err != nil {
		return fmt.Errorf("kcpclient: set the status of the clusterlink %s it just created: %w -- the "+
			"schema declares a status subresource, so a status posted inline is DISCARDED", link.Name, err)
	}
	return nil
}

func (c *Client) WriteClusterLinkStatus(ctx context.Context, logicalCluster, name string, status v1alpha1.ClusterLinkStatus) (bool, error) {
	r, err := c.resource(logicalCluster, socialwebResource)
	if err != nil {
		return false, err
	}
	err = patchClusterLinkStatus(ctx, r, name, status)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func clusterLinkStatusPatch(status v1alpha1.ClusterLinkStatus) ([]byte, error) {
	field := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	return json.Marshal(map[string]any{"status": map[string]any{
		"phase":      field(string(status.Phase)),
		"endpointId": field(status.EndpointID),
		"did":        field(status.Did),
		"ticket":     field(status.Ticket),
		"message":    field(status.Message),
	}})
}

func patchClusterLinkStatus(ctx context.Context, r rest.Interface, name string, status v1alpha1.ClusterLinkStatus) error {
	if name == "" {
		return errors.New("kcpclient: a clusterlink status write needs a link name")
	}
	body, err := clusterLinkStatusPatch(status)
	if err != nil {
		return fmt.Errorf("kcpclient: clusterlink %s: marshal: %w", name, err)
	}
	return r.Patch(types.MergePatchType).SubResource("status").Resource(clusterLinks).Name(name).
		Body(body).Do(ctx).Error()
}

func (c *Client) ReadClusterLink(ctx context.Context, logicalCluster, name string) (*v1alpha1.ClusterLink, error) {
	r, err := c.resource(logicalCluster, socialwebResource)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, errors.New("kcpclient: ReadClusterLink needs a link name")
	}
	var link v1alpha1.ClusterLink
	err = r.Get().Resource(clusterLinks).Name(name).Do(ctx).Into(&link)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("kcpclient: read clusterlink %s: %w", name, err)
	}
	return &link, nil
}

func (c *Client) DeleteClusterLink(ctx context.Context, logicalCluster, name string) error {
	r, err := c.resource(logicalCluster, socialwebResource)
	if err != nil {
		return err
	}
	if name == "" {
		return errors.New("kcpclient: DeleteClusterLink needs a link name")
	}
	err = r.Delete().Resource(clusterLinks).Name(name).Do(ctx).Error()
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("kcpclient: delete clusterlink %s: %w", name, err)
	}
	return nil
}
