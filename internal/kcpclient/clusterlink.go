package kcpclient

import (
	"context"
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
	found := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kcpclient: read clusterlink %s: %w", link.Name, err)
	}
	if !found || existing.Spec != link.Spec {
		obj := v1alpha1.ClusterLink{TypeMeta: clusterLinkType, ObjectMeta: metav1.ObjectMeta{Name: link.Name}, Spec: link.Spec}
		if err := applyDoc(ctx, r.Patch(types.ApplyPatchType).Resource(clusterLinks).Name(link.Name), fieldManagerClusterProvider, true, &obj, false); err != nil {
			return fmt.Errorf("kcpclient: apply clusterlink %s spec: %w", link.Name, err)
		}
	}
	if found && existing.Status == link.Status {
		return nil
	}
	return applyClusterLinkStatus(ctx, r, link.Name, link.Status)
}

var clusterLinkType = metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ClusterLink"}

func (c *Client) WriteClusterLinkStatus(ctx context.Context, logicalCluster, name string, status v1alpha1.ClusterLinkStatus) (bool, error) {
	r, err := c.resource(logicalCluster, socialwebResource)
	if err != nil {
		return false, err
	}
	err = applyClusterLinkStatus(ctx, r, name, status)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func applyClusterLinkStatus(ctx context.Context, r rest.Interface, name string, status v1alpha1.ClusterLinkStatus) error {
	if name == "" {
		return errors.New("kcpclient: a clusterlink status write needs a link name")
	}
	obj := v1alpha1.ClusterLink{TypeMeta: clusterLinkType, ObjectMeta: metav1.ObjectMeta{Name: name}, Status: status}
	return applyDoc(ctx, r.Patch(types.ApplyPatchType).SubResource("status").Resource(clusterLinks).Name(name),
		fieldManagerClusterProvider, true, &obj, true)
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
