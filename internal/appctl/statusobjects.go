package appctl

import (
	"context"
	"reflect"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/statusstate"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

func (ctl *Controller) ensureAppPdsStatus(ctx context.Context, handle string, sess PdsSession, sessionPath string, plc bool) {
	if strings.TrimSpace(sess.Did) == "" {
		return
	}
	ws := ctl.c.workspace()
	pdsRef := commonv1.ObjectRef{Kind: ComponentKindPds, Name: PdsComponent}
	authPath := strings.TrimSpace(sessionPath)
	if after, ok := strings.CutPrefix(authPath, "openbao:"); ok {
		authPath = after
	} else if _, rest, ok := strings.Cut(authPath, ":"); ok {
		authPath = rest
	}
	authRef := &commonv1.SecretRef{Path: strings.Trim(authPath, "/")}
	ctl.ensureRepoObjects(ctx, ws, statusObjectName("repo", sess.Did), "LocalRepo", v1alpha1.LocalRepoGVR,
		v1alpha1.LocalRepoSpec{DID: sess.Did, PdsRef: pdsRef, AuthRef: authRef})
	accountName := statusObjectName("account", handle)
	ctl.ensureStatusObject(ctx, ws, statusstate.PdsAccountGVR, "PdsAccount",
		accountName, v1alpha1.PdsAccountSpec{DID: sess.Did, Handle: handle, PdsRef: pdsRef})
	if plc {
		ctl.ensureStatusObject(ctx, ws, statusstate.PlcIdentityGVR, "PlcIdentity",
			statusObjectName("plc", sess.Did), v1alpha1.PlcIdentitySpec{DID: sess.Did})
	}
	if strings.TrimSpace(sess.AccessJWT) != "" {
		ctl.ensureStatusObject(ctx, ws, statusstate.AtprotoSessionGVR, "AtprotoSession",
			statusObjectName("session", handle), v1alpha1.AtprotoSessionSpec{DID: sess.Did, AccountRef: commonv1.LocalObjectRef{Name: accountName},
				PdsRef: pdsRef, SessionID: sessionPath})
	}
}

func (ctl *Controller) ensureRepoObjects(ctx context.Context, ws, name, backendKind string, backendGVR schema.GroupVersionResource, backendSpec any) {
	ctl.ensureStatusObject(ctx, ws, backendGVR, backendKind, name, backendSpec)
	ctl.ensureStatusObject(ctx, ws, statusstate.AtprotoRepoGVR, "AtprotoRepo", name,
		v1alpha1.AtprotoRepoSpec{BackendRef: commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: backendKind, Name: name}})
}

func (ctl *Controller) ensureRelayStatus(ctx context.Context, relayRef string) {
	if strings.TrimSpace(relayRef) == "" {
		return
	}
	name := statusObjectName("relay", strings.TrimPrefix(relayRef, ComponentKindRelay+"/"))
	ctl.ensureStatusObject(ctx, ctl.c.workspace(), statusstate.RelayStatusGVR, "RelayStatus",
		name, v1alpha1.RelayStatusSpec{RelayRef: commonv1.ObjectRef{Kind: ComponentKindRelay, Name: strings.TrimPrefix(relayRef, ComponentKindRelay+"/")}})
}

func (ctl *Controller) ensureVaultCredentialStatus(ctx context.Context, server, mount, path string) {
	server = strings.TrimSpace(server)
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	path = strings.Trim(strings.TrimSpace(path), "/")
	if server == "" || mount == "" || path == "" {
		return
	}
	ctl.ensureStatusObject(ctx, ctl.c.workspace(), statusstate.VaultCredentialGVR, "VaultCredential",
		statusObjectName("credential", server+"-"+path), v1alpha1.VaultCredentialSpec{SecretRef: commonv1.SecretRef{Vault: v1alpha1.VaultServerRef(server), Mount: mount, Path: path}})
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
	existing, err := ri.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := ri.Create(ctx, obj, metav1.CreateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
			ctl.c.log().Warn("creating a status object", "kind", kind, "name", name, "err", err.Error())
		}
		return
	}
	if err != nil {
		ctl.c.log().Warn("reading a status object", "kind", kind, "name", name, "err", err.Error())
		return
	}
	if len(specMap) == 0 {
		return
	}
	current, _ := existing.Object["spec"].(map[string]any)
	if reflect.DeepEqual(current, specMap) {
		return
	}
	existing.Object["spec"] = specMap
	if _, err := ri.Update(ctx, existing, metav1.UpdateOptions{FieldManager: FieldManager}); err != nil {
		ctl.c.log().Warn("updating a status object", "kind", kind, "name", name, "err", err.Error())
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
