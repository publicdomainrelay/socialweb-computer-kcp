package compose

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	pdssvc "github.com/johnandersen777/socialweb-computer-kcp/internal/services/pds"
)

const (
	ConditionPdsIngress = "PdsIngress"

	PdsIngressReasonReady               = "Ready"
	PdsIngressReasonNotRequired         = "NotRequired"
	PdsIngressReasonProviderMissing     = "IngressProviderMissing"
	PdsIngressReasonProviderUnsupported = "IngressProviderUnsupported"
	PdsIngressReasonProviderNotReady    = "IngressProviderNotReady"
	PdsIngressReasonNotReady            = "IngressNotReady"
	PdsIngressReasonURLInvalid          = "IngressURLInvalid"
	PdsIngressReasonStableHost          = "StableHostMismatch"
)

func PdsSessionPath(name string) string {
	return "identity/" + name + "/pds-session"
}

func PdsHandle(workspace, suffix, explicit string) string {
	if handle := strings.TrimSpace(explicit); handle != "" {
		return strings.ToLower(handle)
	}
	var b strings.Builder
	for _, r := range strings.ToLower(workspace) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	label := cmp.Or(strings.Trim(b.String(), "-"), "app")
	return label + "." + suffix + ".local"
}

func appPdsFallbackURL(network *v1alpha1.MarketNetworkValue, hostnames []string) string {
	if network != nil {
		if host := HostOf(network.GuestTLSURL()); host != "" {
			return "https://" + host
		}
		if host := HostOf(network.IngressProxyHost); host != "" {
			return "https://" + host
		}
	}
	if host := FirstHost(hostnames); host != "" {
		return "http://" + host
	}
	return ""
}

func EnsurePDSKeyFiles(stateDir string) error {
	session := filepath.Join(stateDir, pdssvc.SessionSecretFile)
	if _, err := os.Stat(session); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return err
		}
		if err := WriteSecret(session, []byte(hex.EncodeToString(buf)+"\n")); err != nil {
			return err
		}
	}
	rotation := filepath.Join(stateDir, pdssvc.RotationKeyFile)
	if _, err := os.Stat(rotation); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		key, err := atcrypto.GeneratePrivateKeyK256()
		if err != nil {
			return err
		}
		if err := WriteSecret(rotation, []byte(key.Multibase()+"\n")); err != nil {
			return err
		}
	}
	return nil
}

func AppPdsSpec(stateRoot string, network *v1alpha1.MarketNetworkValue, handle string, vault *commonv1.ObjectRef,
	hostnames []string, publicURL string, requireIngress bool, ingressRef *commonv1.ObjectRef) (v1alpha1.PdsSpec, error) {
	plcURL := ""
	if network != nil {
		plcURL = strings.TrimSpace(network.PLCDirectoryURL)
	}
	if plcURL == "" {
		return v1alpha1.PdsSpec{}, errors.New("compose: the app-owned Pds needs the network value's plcDirectoryUrl; no PLC URL is available")
	}
	stateDir := ComponentCRStateDir(stateRoot, ComponentKindPds, PdsComponent)
	if err := EnsurePDSKeyFiles(stateDir); err != nil {
		return v1alpha1.PdsSpec{}, fmt.Errorf("compose: the app-owned Pds keys: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, pdssvc.RepoKeyDir), 0o700); err != nil {
		return v1alpha1.PdsSpec{}, fmt.Errorf("compose: the app-owned Pds repo key directory: %w", err)
	}
	publicURL = strings.TrimRight(strings.TrimSpace(publicURL), "/")
	if publicURL == "" && !requireIngress {
		publicURL = appPdsFallbackURL(network, hostnames)
	}
	stableHost := ""
	if publicURL != "" {
		base, host, ok := SchemeHostURL(publicURL)
		if !ok {
			return v1alpha1.PdsSpec{}, fmt.Errorf("compose: the app-owned Pds public URL %q is not a scheme, host and optional port", publicURL)
		}
		publicURL, stableHost = base, host
	}
	var oauth *v1alpha1.PdsOAuth
	if publicURL != "" || !requireIngress {
		oauth = &v1alpha1.PdsOAuth{Issuer: publicURL, AllowLocalhostClients: true}
	}
	return v1alpha1.PdsSpec{
		ComponentSpec:  NewComponentSpec(stateDir, network),
		Identity:       &v1alpha1.ComponentIdentity{Handle: handle},
		VaultRef:       vault,
		Hostnames:      hostnames,
		Mode:           v1alpha1.PdsModeFull,
		PlcURL:         plcURL,
		PublicBaseURL:  publicURL,
		RequireIngress: requireIngress,
		IngressRef:     ingressRef,
		PublicURL:      publicURL,
		StableHost:     stableHost,
		OAuth:          oauth,
	}, nil
}

func DesiredPds(desired []Desired) (Desired, bool) {
	for _, d := range desired {
		if d.Kind == ComponentKindPds && d.Name == PdsComponent {
			return d, true
		}
	}
	return Desired{}, false
}

func WithoutPdsNamed(desired []Desired, names ...string) []Desired {
	out := make([]Desired, 0, len(desired))
	for _, d := range desired {
		if d.Kind == ComponentKindPds && slices.Contains(names, d.Name) {
			continue
		}
		out = append(out, d)
	}
	return out
}

func WithoutPds(desired []Desired) []Desired {
	return WithoutPdsNamed(desired, PdsComponent)
}

func PdsApplySet(desired []Desired, pdsName, ingressRef string) []Desired {
	prefix := ServiceNameFor(ComponentKindPds, pdsName)
	out := make([]Desired, 0, 5)
	for _, d := range desired {
		switch {
		case d.Kind == ComponentKindPds && d.Name == pdsName:
			out = append(out, d)
		case d.Kind == ComponentKindService && d.Name == prefix:
			out = append(out, d)
		case d.Kind == ComponentKindExposure && strings.HasPrefix(d.Name, prefix+"-"):
			out = append(out, d)
		case d.Kind == ComponentKindIngressProxy && ingressRef != "" && d.Name == ingressRef:
			out = append(out, d)
		}
	}
	return out
}

func PdsIngressExposureName(pdsName string) string {
	return ServiceNameFor(ComponentKindPds, pdsName) + ExposureIngressSuffix
}

func SchemeHostURL(raw string) (string, string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", false
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", "", false
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host, u.Host, true
}

type PdsIngressError struct {
	Reason  string
	Message string
}

func (e *PdsIngressError) Error() string {
	return e.Reason + ": " + e.Message
}

func pdsIngressErr(reason, format string, args ...any) error {
	return &PdsIngressError{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

func PdsIngressReason(err error) string {
	if e, ok := err.(*PdsIngressError); ok {
		return e.Reason
	}
	return PdsIngressReasonNotReady
}

func appIngressConfig(spec *v1alpha1.IngressConfig, network *v1alpha1.MarketNetworkValue) (*v1alpha1.IngressConfig, bool) {
	if spec != nil {
		return spec, false
	}
	if network != nil && network.Ingress != nil {
		return network.Ingress, true
	}
	return nil, false
}

func pdsIngressRef(cfg *v1alpha1.IngressConfig, fromNetwork bool) *commonv1.ObjectRef {
	if cfg != nil && cfg.IngressRef != nil {
		if name := strings.TrimSpace(cfg.IngressRef.Name); name != "" {
			return ComponentRefFor(ComponentKindIngressProxy, name)
		}
	}
	if fromNetwork {
		return ComponentRefFor(ComponentKindIngressProxy, NetworkIngressName)
	}
	return ComponentRefFor(ComponentKindIngressProxy, AppIngressName)
}

func PdsAppIngress(spec *v1alpha1.IngressConfig, network *v1alpha1.MarketNetworkValue, production bool) (bool, *commonv1.ObjectRef, *v1alpha1.IngressConfig, error) {
	cfg, fromNetwork := appIngressConfig(spec, network)
	if IngressProviderFor(cfg) == v1alpha1.IngressProviderIroh {
		return true, nil, nil, pdsIngressErr(PdsIngressReasonProviderUnsupported,
			"the Pds cannot use ingress provider %s until a later plan", v1alpha1.IngressProviderIroh)
	}
	if IngressProviderFor(cfg) == "" {
		if cfg != nil || production {
			return true, nil, nil, pdsIngressErr(PdsIngressReasonProviderMissing,
				"no Pds ingress provider is selected; set spec.ingress.xrpc.ingressProxyHost or spec.ingress.opentunnel")
		}
		return false, nil, nil, nil
	}
	exposure := cfg
	if fromNetwork {
		exposure = nil
	}
	return true, pdsIngressRef(cfg, fromNetwork), exposure, nil
}

func NetworkIngressSelection(spec *v1alpha1.IngressConfig) error {
	if spec == nil {
		return nil
	}
	switch IngressProviderFor(spec) {
	case v1alpha1.IngressProviderIroh:
		return pdsIngressErr(PdsIngressReasonProviderUnsupported,
			"the network Pds cannot use ingress provider %s until a later plan", v1alpha1.IngressProviderIroh)
	case "":
		if strings.TrimSpace(spec.Provider) != "" {
			return pdsIngressErr(PdsIngressReasonProviderMissing, "the network Pds ingress provider %q is not known", spec.Provider)
		}
	}
	return nil
}

func (c *Composer) exposureAddress(ctx context.Context, ws, name string) string {
	o, err := Get[exposurev1alpha1.Exposure](ctx, c, ws, ExposureGVR, name)
	if err != nil {
		return ""
	}
	return o.Status.Address
}

func (c *Composer) PdsSpec(ctx context.Context, ws, pdsName string) (v1alpha1.PdsSpec, bool, error) {
	o, err := Get[v1alpha1.Pds](ctx, c, ws, PdsGVR, pdsName)
	if apierrors.IsNotFound(err) {
		return v1alpha1.PdsSpec{}, false, nil
	}
	if err != nil {
		return v1alpha1.PdsSpec{}, false, err
	}
	return o.Spec, true, nil
}

func (c *Composer) PdsIngressURL(ctx context.Context, ws, pdsName string) (string, error) {
	raw := c.exposureAddress(ctx, ws, ServiceNameFor(ComponentKindPds, pdsName)+ExposureIngressSuffix)
	if raw == "" {
		return "", pdsIngressErr(PdsIngressReasonNotReady, "the ingress Exposure %s reports no public URL yet", PdsIngressExposureName(pdsName))
	}
	base, host, ok := SchemeHostURL(raw)
	if !ok {
		return "", pdsIngressErr(PdsIngressReasonURLInvalid,
			"the ingress Exposure %s reports %q, which is not a scheme, host and optional port", PdsIngressExposureName(pdsName), raw)
	}
	spec, found, err := c.PdsSpec(ctx, ws, pdsName)
	if err != nil {
		return "", err
	}
	if stable := strings.TrimSpace(spec.StableHost); found && stable != "" && !strings.EqualFold(stable, host) {
		return "", pdsIngressErr(PdsIngressReasonStableHost,
			"the Pds public host was %s and the ingress now returns %s; the DID service endpoint is not rewritten", stable, host)
	}
	return base, nil
}

func (c *Composer) PdsPublicURL(ctx context.Context, ws, pdsName string, requireIngress bool) (string, error) {
	if requireIngress {
		return c.PdsIngressURL(ctx, ws, pdsName)
	}
	if raw := c.exposureAddress(ctx, ws, ServiceNameFor(ComponentKindPds, pdsName)+ExposureIngressSuffix); raw != "" {
		if base, _, ok := SchemeHostURL(raw); ok {
			return base, nil
		}
	}
	raw := c.exposureAddress(ctx, ws, ServiceNameFor(ComponentKindPds, pdsName)+ExposureLocalSuffix)
	if raw == "" {
		return "", nil
	}
	if base, _, ok := SchemeHostURL(raw); ok {
		return base, nil
	}
	return strings.TrimRight(strings.TrimSpace(raw), "/"), nil
}

func (c *Composer) PdsPublicURLApplied(ctx context.Context, ws, pdsName string) (bool, error) {
	spec, found, err := c.PdsSpec(ctx, ws, pdsName)
	if err != nil || !found {
		return false, err
	}
	return strings.TrimSpace(spec.PublicBaseURL) != "", nil
}

func (s Snapshot) PdsApplied(pdsName string) bool {
	view, ok := s.View(ComponentKindPds, pdsName)
	return ok && view.Status.Ready && view.Status.ObservedGeneration >= view.Generation
}

func (s Snapshot) PdsIngressReady(pdsName string, ref *commonv1.ObjectRef) error {
	if ref != nil {
		if view, ok := s.View(ComponentKindIngressProxy, ref.Name); ok && !view.Status.Ready {
			return pdsIngressErr(PdsIngressReasonProviderNotReady, "the IngressProxy %s is not ready", ref.Name)
		}
	}
	if !s.Ready(ComponentKindExposure, PdsIngressExposureName(pdsName)) {
		return pdsIngressErr(PdsIngressReasonNotReady, "the ingress Exposure %s is not ready", PdsIngressExposureName(pdsName))
	}
	return nil
}

func ComponentRefName(ref *commonv1.ObjectRef) string {
	if ref == nil {
		return ""
	}
	return strings.TrimSpace(ref.Name)
}

func PdsIngressConditionType(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == PdsComponent {
		return ConditionPdsIngress
	}
	return ConditionPdsIngress + strings.ToUpper(name[:1]) + name[1:]
}

func SetPdsIngressCondition(conds *[]metav1.Condition, generation int64, name string, requireIngress bool, err error) {
	conditionType := PdsIngressConditionType(name)
	if !requireIngress {
		meta.SetStatusCondition(conds, metav1.Condition{Type: conditionType, Status: metav1.ConditionTrue,
			Reason: PdsIngressReasonNotRequired, Message: "the Pds does not require an ingress", ObservedGeneration: generation})
		return
	}
	if err == nil {
		meta.SetStatusCondition(conds, metav1.Condition{Type: conditionType, Status: metav1.ConditionTrue,
			Reason: PdsIngressReasonReady, Message: "the Pds ingress is ready", ObservedGeneration: generation})
		return
	}
	meta.SetStatusCondition(conds, metav1.Condition{Type: conditionType, Status: metav1.ConditionFalse,
		Reason: PdsIngressReason(err), Message: err.Error(), ObservedGeneration: generation})
}

func PdsIngressPhase(err error) v1alpha1.ApplicationPhase {
	if PdsIngressReason(err) == PdsIngressReasonStableHost {
		return v1alpha1.ApplicationPhaseFailed
	}
	return v1alpha1.ApplicationPhaseProvisioning
}

type PdsAccount struct {
	Vault string

	Mount string

	Path string

	Handle string

	Workspace string

	Pds string
}

func (a PdsAccount) pds() string {
	if a.Pds == "" {
		return PdsComponent
	}
	return a.Pds
}

func (a PdsAccount) credentials() commonv1.SecretRef {
	return commonv1.SecretRef{Vault: v1alpha1.VaultServerRef(a.Vault), Mount: a.Mount, Path: a.Path, Key: "password"}
}

func PdsAccountLinks(r Root, a PdsAccount) []Link {
	pdsRef := AppRef(ComponentKindPds, a.pds(), workspaceOr(a.Workspace, r.Workspace))
	return []Link{RootAccountLink(r.Ref(), pdsRef), RootVaultLink(r, a.Vault, a.Mount+"/identity/*")}
}

func (c *Composer) EnsurePdsAccount(ctx context.Context, r Root, a PdsAccount) (string, error) {
	ws := workspaceOr(a.Workspace, r.Workspace)
	kv, err := c.KV(ctx, r, a.Vault, a.Mount+"/identity/*")
	if err != nil {
		return "", err
	}
	if err := c.ApplyVaultCredential(ctx, r, a.Vault, a.Mount, a.Path); err != nil {
		return "", err
	}
	data, err := readKV(ctx, kv, a.Mount, a.Path)
	if err != nil {
		return "", err
	}
	pdsRef := AppRef(ComponentKindPds, a.pds(), ws)
	scope := []string{ScopeAll}
	accounts, err := bus.Resolve[pds.Accounts](ctx, c.cfg.Bus, r.Ref(), pdsRef, bus.Capability{Name: pds.CapabilityAccount, Scope: scope})
	if err != nil {
		return "", err
	}
	if did, _ := data["did"].(string); did != "" {
		if acct, err := accounts.Account(ctx, did); err == nil && acct.DID == did {
			return did, nil
		}
	}
	password, _ := data["password"].(string)
	if password == "" {
		if password, err = NewPassword(); err != nil {
			return "", err
		}
		data["password"] = password
		if err := kv.Write(ctx, a.Mount, a.Path, data); err != nil {
			return "", err
		}
	}
	sessions, err := bus.Resolve[pds.Sessions](ctx, c.cfg.Bus, r.Ref(), pdsRef, bus.Capability{Name: pds.CapabilitySession, Scope: scope})
	if err != nil {
		return "", err
	}
	did := ""
	if sess, err := sessions.SignIn(ctx, pds.SessionRequest{Identifier: a.Handle, Credentials: a.credentials()}); err == nil {
		did = sess.DID
	} else {
		domain := a.Handle[strings.Index(a.Handle, ".")+1:]
		acct, err := accounts.CreateAccount(ctx, pds.AccountRequest{Handle: a.Handle, Email: a.Handle + "@" + domain, Credentials: a.credentials()})
		if err != nil {
			return "", err
		}
		did = acct.DID
	}
	if did == "" {
		return "", fmt.Errorf("compose: the Pds reports no did for %s", a.Handle)
	}
	data["did"] = did
	if err := kv.Write(ctx, a.Mount, a.Path, data); err != nil {
		return "", err
	}
	c.log.Info("the application's PDS account is ready; its password stays in its own vault", "vault", a.Vault, "path", a.Path, "did", did)
	return did, nil
}

func SyncRequesterRepoKey(stateRoot, did, requesterDir string) (bool, error) {
	sum := sha256.Sum256([]byte(did))
	keyPath := filepath.Join(ComponentCRStateDir(stateRoot, ComponentKindPds, PdsComponent), pdssvc.RepoKeyDir, hex.EncodeToString(sum[:])+".key")
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return false, fmt.Errorf("the app Pds repo signing key of %s: %w", did, err)
	}
	lines := strings.SplitN(strings.TrimSpace(string(raw)), "\n", 2)
	if len(lines) != 2 || strings.TrimSpace(lines[0]) != did {
		return false, fmt.Errorf("the app Pds repo signing key file of %s is invalid", did)
	}
	key, err := atcrypto.ParsePrivateMultibase(strings.TrimSpace(lines[1]))
	if err != nil {
		return false, fmt.Errorf("the app Pds repo signing key of %s: %w", did, err)
	}
	payload := map[string]string{"kty": "EC", "crv": "secp256k1", "use": "sig", "alg": "ES256K", "d": base64.RawURLEncoding.EncodeToString(key.Bytes())}
	out, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return false, err
	}
	out = append(out, '\n')
	if err := os.MkdirAll(requesterDir, 0o700); err != nil {
		return false, err
	}
	changed := false
	for _, name := range []string{"repo-key.json", "attestation-key.json"} {
		path := filepath.Join(requesterDir, name)
		if have, readErr := os.ReadFile(path); readErr == nil && string(have) == string(out) {
			continue
		}
		if err := WriteFileAtomic(path, out, 0o600); err != nil {
			return changed, fmt.Errorf("writing the requester's %s: %w", name, err)
		}
		changed = true
	}
	return changed, nil
}

func (c *Composer) PdsEndpoint(ctx context.Context, ws, name string) string {
	v, found, err := c.GetComponent(ctx, ws, ComponentKindPds, name)
	if err != nil || !found {
		return ""
	}
	e, ok := ComponentEndpoint(v, "http")
	if !ok {
		return ""
	}
	return EndpointURL(e)
}
