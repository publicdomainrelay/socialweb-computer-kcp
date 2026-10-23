package openbaoctl

import (
	"context"
	"fmt"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func FileRef(ref string) string { return strings.TrimPrefix(ref, "file:") }

func ClientRefs(st v1alpha1.OpenBaoServerStatus, name string) (v1alpha1.OpenBaoServerClientRefs, error) {
	if st.Address == "" || st.Refs == nil {
		return v1alpha1.OpenBaoServerClientRefs{}, fmt.Errorf("the OpenBaoServer reports no address yet (phase %s)", st.Phase)
	}
	for _, c := range st.Clients {
		if c.Name == name && c.ClientCert != "" {
			return c, nil
		}
	}
	return v1alpha1.OpenBaoServerClientRefs{}, fmt.Errorf("the OpenBaoServer reports no client %s yet", name)
}

func ClientLogin(ctx context.Context, st v1alpha1.OpenBaoServerStatus, name string) (*openbao.Client, v1alpha1.OpenBaoServerClientRefs, error) {
	c, err := ClientRefs(st, name)
	if err != nil {
		return nil, c, err
	}
	srv, err := openbao.ServerOf(st.Address, "", st, name)
	if err != nil {
		return nil, c, err
	}
	logged, err := openbao.Login(ctx, srv)
	if err != nil {
		return nil, c, err
	}
	return logged, c, nil
}

func Synced(o *v1alpha1.OpenBaoServer) bool {
	if o == nil || o.Status.ObservedGeneration != o.Generation || o.Status.Phase != v1alpha1.OpenBaoServerPhaseReady {
		return false
	}
	c := meta.FindStatusCondition(o.Status.Conditions, ConditionSynced)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == o.Generation
}
