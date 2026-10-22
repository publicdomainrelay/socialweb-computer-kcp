package appctl

import (
	"context"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/statusstate"
)

func (ctl *Controller) ensureAppPdsStatus(ctx context.Context, handle string, sess PdsSession, plc bool) {
	if strings.TrimSpace(sess.Did) == "" {
		return
	}
	ws := ctl.c.workspace()
	pdsRef := ComponentKey(ComponentKindPds, PdsComponent)
	ctl.ensureStatusObject(ctx, ws, statusstate.AtprotoRepoGVR, "AtprotoRepo",
		statusObjectName("repo", sess.Did), v1alpha1.AtprotoRepoSpec{DID: sess.Did, PdsRef: &v1alpha1.RepoRef{Kind: ComponentKindPds, Name: PdsComponent}})
	accountName := statusObjectName("account", handle)
	ctl.ensureStatusObject(ctx, ws, statusstate.PdsAccountGVR, "PdsAccount",
		accountName, v1alpha1.PdsAccountSpec{DID: sess.Did, Handle: handle, PdsRef: pdsRef})
	if plc {
		ctl.ensureStatusObject(ctx, ws, statusstate.PlcIdentityGVR, "PlcIdentity",
			statusObjectName("plc", sess.Did), v1alpha1.PlcIdentitySpec{DID: sess.Did})
	}
	if strings.TrimSpace(sess.AccessJWT) != "" {
		ctl.ensureStatusObject(ctx, ws, statusstate.AtprotoSessionGVR, "AtprotoSession",
			statusObjectName("session", handle), v1alpha1.AtprotoSessionSpec{DID: sess.Did, AccountRef: accountName, PdsRef: pdsRef})
	}
}

func (ctl *Controller) ensureRelayStatus(ctx context.Context, relayRef string) {
	if strings.TrimSpace(relayRef) == "" {
		return
	}
	name := statusObjectName("relay", strings.TrimPrefix(relayRef, ComponentKindRelay+"/"))
	ctl.ensureStatusObject(ctx, ctl.c.workspace(), statusstate.RelayStatusGVR, "RelayStatus",
		name, v1alpha1.RelayStatusSpec{RelayRef: relayRef})
}

func (ctl *Controller) ensureVaultCredentialStatus(ctx context.Context, server, mount, path string) {
	server = strings.TrimSpace(server)
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	path = strings.Trim(strings.TrimSpace(path), "/")
	if server == "" || mount == "" || path == "" {
		return
	}
	ctl.ensureStatusObject(ctx, ctl.c.workspace(), statusstate.VaultCredentialGVR, "VaultCredential",
		statusObjectName("credential", server+"-"+path), v1alpha1.VaultCredentialSpec{Server: server, Mount: mount, Path: path})
}

func (ctl *Controller) ensureStatusObject(ctx context.Context, ws string, gvr schema.GroupVersionResource, kind, name string, spec any) {
	if name == "" {
		return
	}
	ri, err := ctl.client(ws, gvr)
	if err != nil {
		ctl.c.log().Warn("creating a status object", "kind", kind, "name", name, "err", err.Error())
		return
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupName + "/" + v1alpha1.Version,
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
	}}
	specMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	if err != nil {
		ctl.c.log().Warn("encoding a status object", "kind", kind, "name", name, "err", err.Error())
		return
	}
	if len(specMap) > 0 {
		obj.Object["spec"] = specMap
	}
	if _, err := ri.Create(ctx, obj, metav1.CreateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
		ctl.c.log().Warn("creating a status object", "kind", kind, "name", name, "err", err.Error())
	}
}

func statusObjectName(prefix, subject string) string {
	raw := strings.ToLower(prefix + "-" + subject)
	var b strings.Builder
	lastDash := false
	for _, r := range raw {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.') {
			r = '-'
		}
		if r == '-' {
			if lastDash {
				continue
			}
			lastDash = true
		} else {
			lastDash = false
		}
		b.WriteRune(r)
	}
	name := strings.Trim(b.String(), "-.")
	if name == "" {
		name = strings.Trim(prefix, "-.")
	}
	if len(name) > 253 {
		name = strings.TrimRight(name[:253], "-.")
	}
	return name
}
