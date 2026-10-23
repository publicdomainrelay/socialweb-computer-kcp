package appctl

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	BidderVaultServer = "bidder"
	BidderVaultClient = "bidder"
	identityKeysPath  = "identity/bidder/keys"
	RepoKeyField      = "repoKeyHex"
	passwordBytes     = 24
)

var appPdsPublicURLs sync.Map

type PdsSession struct {
	Did string

	AccessJWT string

	RefreshJWT string
}

func pdsSessionPath(name string) string {
	return "identity/" + name + "/pds-session"
}

func pdsAccessTokenFor(name string) *commonv1.SecretRef {
	return &commonv1.SecretRef{Path: pdsSessionPath(name)}
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := m[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func pdsAccessTokenLive(token string) bool {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Expires int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Expires == 0 {
		return false
	}
	return time.Now().Add(30*time.Second).Unix() < claims.Expires
}

func pdsSessionOf(m map[string]any) PdsSession {
	return PdsSession{
		Did:        firstString(m, "did"),
		AccessJWT:  firstString(m, "accessJwt", "accessToken", "access_token", "token"),
		RefreshJWT: firstString(m, "refreshJwt", "refreshToken", "refresh_token"),
	}
}

func pdsHandle(workspace, suffix, explicit string) string {
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
	label := strings.Trim(b.String(), "-")
	if label == "" {
		label = "app"
	}
	return label + "." + suffix + ".local"
}

func appPdsFallbackURL(network *v1alpha1.MarketNetworkValue, hostnames []string) string {
	if network != nil {
		if host := hostOf(network.GuestTLSURL()); host != "" {
			return "https://" + host
		}
		if host := hostOf(network.IngressProxyHost); host != "" {
			return "https://" + host
		}
	}
	if host := firstHost(hostnames); host != "" {
		return "http://" + host
	}
	return ""
}

func (ctl *Controller) pdsApplied(ctx context.Context, owner string) (bool, error) {
	snap, err := ctl.componentSnapshot(ctx, owner)
	if err != nil {
		return false, err
	}
	view, ok := snap.view(ComponentKindPds, PdsComponent)
	if !ok || !view.Status.Ready {
		return false, nil
	}
	return view.Status.ObservedGeneration >= view.Generation, nil
}

func (ctl *Controller) pdsExposureURLOf(ctx context.Context, ws, name, suffix string) string {
	key := ws + "/" + name + suffix
	ri, err := ctl.client(ws, ExposureGVR)
	if err != nil {
		return ""
	}
	u, err := ri.Get(ctx, serviceNameFor(ComponentKindPds, name)+suffix, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		appPdsPublicURLs.Delete(key)
		return ""
	}
	if err != nil {
		value, _ := appPdsPublicURLs.Load(key)
		raw, _ := value.(string)
		return raw
	}
	var o exposurev1alpha1.Exposure
	if err := fromUnstructured(u, &o); err == nil && o.Status.Address != "" {
		appPdsPublicURLs.Store(key, o.Status.Address)
		return o.Status.Address
	}
	value, _ := appPdsPublicURLs.Load(key)
	raw, _ := value.(string)
	return raw
}

func appPdsSpec(stateRoot string, network *v1alpha1.MarketNetworkValue, handle string, vault *commonv1.ObjectRef,
	hostnames []string, suspend bool, publicURL string, requireIngress bool, ingressRef *commonv1.ObjectRef) (v1alpha1.PdsSpec, error) {
	plcURL := ""
	if network != nil {
		plcURL = strings.TrimSpace(network.PLCDirectoryURL)
	}
	if plcURL == "" {
		return v1alpha1.PdsSpec{}, errors.New("appctl: the app-owned Pds needs the network value's plcDirectoryUrl; no PLC URL is available")
	}
	stateDir := ComponentCRStateDir(stateRoot, ComponentKindPds, PdsComponent)
	if err := ensurePDSKeyFiles(stateDir); err != nil {
		return v1alpha1.PdsSpec{}, fmt.Errorf("appctl: the app-owned Pds keys: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, pdsRepoKeyDir), 0o700); err != nil {
		return v1alpha1.PdsSpec{}, fmt.Errorf("appctl: the app-owned Pds repo key directory: %w", err)
	}
	publicURL = strings.TrimRight(strings.TrimSpace(publicURL), "/")
	if publicURL == "" && !requireIngress {
		publicURL = appPdsFallbackURL(network, hostnames)
	}
	stableHost := ""
	if publicURL != "" {
		base, host, ok := schemeHostURL(publicURL)
		if !ok {
			return v1alpha1.PdsSpec{}, fmt.Errorf("appctl: the app-owned Pds public URL %q is not a scheme, host and optional port", publicURL)
		}
		publicURL, stableHost = base, host
	}
	var oauth *v1alpha1.PdsOAuth
	if publicURL != "" || !requireIngress {
		oauth = &v1alpha1.PdsOAuth{Issuer: publicURL, AllowLocalhostClients: true}
	}
	return v1alpha1.PdsSpec{
		ComponentSpec:  NewComponentSpec(stateDir, network, suspend),
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

func (ctl *Controller) pdsEndpointIn(ctx context.Context, ws, name string) string {
	v, found, err := ctl.GetComponentIn(ctx, ws, ComponentKindPds, name)
	if err != nil || !found {
		return ""
	}
	e, ok := componentEndpoint(v, "http")
	if !ok {
		e, ok = componentEndpoint(v, "")
	}
	if !ok {
		return ""
	}
	return endpointURL(e)
}

func (ctl *Controller) ensurePdsAccountInVault(ctx context.Context, server, clientName, path, pdsURL, caPEM, handle string) (PdsSession, error) {
	client, _, c, err := ctl.vaultClient(ctx, server, clientName)
	if err != nil {
		return PdsSession{}, err
	}
	defer client.RevokeSelf(context.Background())
	ctl.ensureVaultCredentialStatus(ctx, server, c.KVMount, path)
	kvPath := c.KVMount + "/data/" + path
	data, err := client.GetKV(ctx, kvPath)
	var re *openbao.ResponseError
	if err != nil && !(errors.As(err, &re) && re.Status == http.StatusNotFound) {
		return PdsSession{}, err
	}
	if data == nil {
		data = map[string]any{}
	}
	if sess := pdsSessionOf(data); sess.Did != "" && sess.AccessJWT != "" && pdsAccessTokenLive(sess.AccessJWT) {
		if ctl.pdsSessionUsable(ctx, pdsURL, caPEM, sess) {
			return sess, nil
		}
		ctl.c.log().Info("the application's stored PDS session is no longer accepted; signing in again", "did", sess.Did)
	}
	password := firstString(data, "password")
	if password == "" {
		buf := make([]byte, passwordBytes)
		if _, err := rand.Read(buf); err != nil {
			return PdsSession{}, err
		}
		password = hex.EncodeToString(buf)
	}
	sess, err := ctl.ensurePdsAccountSession(ctx, pdsURL, caPEM, handle, password)
	if err != nil {
		return PdsSession{}, err
	}
	data["password"] = password
	data["did"] = sess.Did
	data["accessToken"] = sess.AccessJWT
	data["accessJwt"] = sess.AccessJWT
	data["refreshToken"] = sess.RefreshJWT
	data["refreshJwt"] = sess.RefreshJWT
	if err := client.PutKV(ctx, kvPath, data); err != nil {
		return PdsSession{}, err
	}
	ctl.c.log().Info("the application's PDS session is in its own OpenBao", "server", server, "path", path, "did", sess.Did)
	return sess, nil
}

func (ctl *Controller) pdsSessionUsable(ctx context.Context, pdsURL, caPEM string, sess PdsSession) bool {
	if strings.TrimSpace(sess.Did) == "" || strings.TrimSpace(sess.AccessJWT) == "" {
		return false
	}
	pds, err := ctl.pdsForToken(pdsURL, caPEM, sess.AccessJWT)
	if err != nil {
		return false
	}
	got, err := pds.GetSession(ctx)
	return err == nil && got.DID.String() == sess.Did
}

func (ctl *Controller) syncRequesterRepoKey(stateRoot, did string) (bool, error) {
	sum := sha256.Sum256([]byte(did))
	keyPath := filepath.Join(ComponentCRStateDir(stateRoot, ComponentKindPds, PdsComponent), pdsRepoKeyDir,
		hex.EncodeToString(sum[:])+".key")
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
	payload := map[string]string{
		"kty": "EC",
		"crv": "secp256k1",
		"use": "sig",
		"alg": "ES256K",
		"d":   base64.RawURLEncoding.EncodeToString(key.Bytes()),
	}
	out, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return false, err
	}
	out = append(out, '\n')
	dir := ComponentCRStateDir(stateRoot, ComponentKindRequester, RequesterComponent)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	changed := false
	for _, name := range []string{"repo-key.json", "attestation-key.json"} {
		path := filepath.Join(dir, name)
		if have, readErr := os.ReadFile(path); readErr == nil && string(have) == string(out) {
			continue
		}
		if err := writeFileAtomic(path, out, 0o600); err != nil {
			return changed, fmt.Errorf("writing the requester's %s: %w", name, err)
		}
		changed = true
	}
	return changed, nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func (ctl *Controller) recordPdsIdentity(ctx context.Context, ws, name, handle, pdsURL, did string, association v1alpha1.AssociationMode) error {
	ri, err := ctl.client(ws, MarketIdentityGVR)
	if err != nil {
		return err
	}
	for range 5 {
		u, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if u.GetDeletionTimestamp() != nil {
			return nil
		}
		var st v1alpha1.MarketIdentityStatus
		if m, ok, err := unstructured.NestedMap(u.Object, "status"); err != nil {
			return err
		} else if ok {
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &st); err != nil {
				return err
			}
		}
		generation := u.GetGeneration()
		revoked := false
		for _, c := range st.Conditions {
			if c.Type == v1alpha1.ConditionAssociated && c.Reason == "Revoked" {
				revoked = true
			}
		}
		st.Did = did
		st.Phase = v1alpha1.MarketIdentityPhaseReady
		st.ObservedGeneration = generation
		st.Message = ""
		if handle != "" {
			st.Handle = handle
		}
		if pdsURL != "" {
			st.PdsURL = pdsURL
		}
		approved, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "approvedAssociations")
		for _, did := range approved {
			if did != "" && !slices.Contains(st.Associations, did) {
				st.Associations = append(st.Associations, did)
			}
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue,
			Reason: "Ready", Message: "the app PDS account is signed in", ObservedGeneration: generation})
		associateWith, _, _ := unstructured.NestedString(u.Object, "spec", "associateWith")
		if strings.TrimSpace(associateWith) == "" {
			switch {
			case association == v1alpha1.AssociationSkip:
				meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: v1alpha1.ConditionAssociated, Status: metav1.ConditionTrue,
					Reason: "AssociationSkipped", Message: "the association is skipped", ObservedGeneration: generation})
			case len(st.Associations) > 0:
				meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: v1alpha1.ConditionAssociated, Status: metav1.ConditionTrue,
					Reason: "Associated", Message: "the operator association is approved", ObservedGeneration: generation})
			case revoked:
				meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: v1alpha1.ConditionAssociated, Status: metav1.ConditionFalse,
					Reason: "Revoked", Message: "the operator's verified claim record is gone", ObservedGeneration: generation})
			default:
				meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: v1alpha1.ConditionAssociated, Status: metav1.ConditionFalse,
					Reason: "AwaitingAssociation", Message: "the PDS is ready; the association is pending", ObservedGeneration: generation})
			}
		}
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&st)
		if err != nil {
			return err
		}
		u.Object["status"] = m
		_, err = ri.UpdateStatus(ctx, u, metav1.UpdateOptions{FieldManager: FieldManager})
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("appctl: the status of the MarketIdentity %s stayed in conflict", name)
}

var OpenBaoServerGVR = v1alpha1.OpenBaoServerGVR

func fileRef(ref string) string { return strings.TrimPrefix(ref, "file:") }

func (ctl *Controller) vaultClient(ctx context.Context, server, clientName string) (*openbao.Client, v1alpha1.OpenBaoServerStatus, v1alpha1.OpenBaoServerClientRefs, error) {
	ri, err := ctl.client(ctl.c.workspace(), OpenBaoServerGVR)
	if err != nil {
		return nil, v1alpha1.OpenBaoServerStatus{}, v1alpha1.OpenBaoServerClientRefs{}, err
	}
	return ServerClient(ctx, ri, server, clientName)
}

func ServerClient(ctx context.Context, ri dynamic.ResourceInterface, server, clientName string) (*openbao.Client, v1alpha1.OpenBaoServerStatus, v1alpha1.OpenBaoServerClientRefs, error) {
	u, err := ri.Get(ctx, server, metav1.GetOptions{})
	if err != nil {
		return nil, v1alpha1.OpenBaoServerStatus{}, v1alpha1.OpenBaoServerClientRefs{}, err
	}
	var o v1alpha1.OpenBaoServer
	if err := fromUnstructured(u, &o); err != nil {
		return nil, v1alpha1.OpenBaoServerStatus{}, v1alpha1.OpenBaoServerClientRefs{}, err
	}
	st := o.Status
	if st.Address == "" || st.Refs == nil {
		return nil, st, v1alpha1.OpenBaoServerClientRefs{}, fmt.Errorf("the OpenBaoServer %s reports no address yet (phase %s)", server, st.Phase)
	}
	var c v1alpha1.OpenBaoServerClientRefs
	for _, x := range st.Clients {
		if x.Name == clientName {
			c = x
		}
	}
	if c.ClientCert == "" {
		return nil, st, c, fmt.Errorf("the OpenBaoServer %s reports no client %s yet", server, clientName)
	}
	caPEM, err := os.ReadFile(fileRef(st.Refs.CA))
	if err != nil {
		return nil, st, c, err
	}
	pool, err := openbao.CertPoolFromPEM(caPEM)
	if err != nil {
		return nil, st, c, err
	}
	cert, err := tls.LoadX509KeyPair(fileRef(c.ClientCert), fileRef(c.ClientKey))
	if err != nil {
		return nil, st, c, err
	}
	client := openbao.New(st.Address, "", openbao.WithCACertPool(pool), openbao.WithClientCertificate(cert))
	token, err := client.LoginCert(ctx, c.CertAuthMount, c.CertAuthRole)
	if err != nil {
		return nil, st, c, err
	}
	return client.WithToken(token), st, c, nil
}

func (ctl *Controller) bidderPassword(ctx context.Context) (string, manifests.BidderVault, error) {
	client, _, c, err := ctl.vaultClient(ctx, BidderVaultServer, BidderVaultClient)
	if err != nil {
		return "", manifests.BidderVault{}, err
	}
	defer client.RevokeSelf(context.Background())
	path := c.KVMount + "/data/" + identityKeysPath
	data, err := client.GetKV(ctx, path)
	var re *openbao.ResponseError
	if err != nil && !(errors.As(err, &re) && re.Status == http.StatusNotFound) {
		return "", manifests.BidderVault{}, err
	}
	password, _ := data["password"].(string)
	if password == "" {
		buf := make([]byte, passwordBytes)
		if _, err := rand.Read(buf); err != nil {
			return "", manifests.BidderVault{}, err
		}
		password = hex.EncodeToString(buf)
		if data == nil {
			data = map[string]any{}
		}
		data["password"] = password
		if err := client.PutKV(ctx, path, data); err != nil {
			return "", manifests.BidderVault{}, err
		}
		ctl.c.log().Info("the bidder's account password is in its own OpenBao", "server", BidderVaultServer, "path", path)
	}
	return password, manifests.BidderVault{Server: BidderVaultServer, Client: BidderVaultClient}, nil
}

func (ctl *Controller) ensurePdsAccountSession(ctx context.Context, pdsURL, caPEM, handle, password string) (PdsSession, error) {
	domain := handle[strings.Index(handle, ".")+1:]
	pds, err := ctl.pdsFor(pdsURL, caPEM)
	if err != nil {
		return PdsSession{}, err
	}
	if sess, err := pds.CreateSession(ctx, handle, password); err == nil {
		return pdsSessionFromSession(sess), nil
	}
	sess, err := pds.CreateAccount(ctx, pdsclient.CreateAccountRequest{Identifier: handle, Password: password, Email: handle + "@" + domain})
	if err != nil {
		return PdsSession{}, err
	}
	return pdsSessionFromSession(sess), nil
}

func EnsurePdsAccountSession(ctx context.Context, pdsURL, caPEM, handle, password string) (PdsSession, error) {
	domain := handle[strings.Index(handle, ".")+1:]
	pds, err := pdsClientAt(pdsURL, caPEM)
	if err != nil {
		return PdsSession{}, err
	}
	if sess, err := pds.CreateSession(ctx, handle, password); err == nil {
		return pdsSessionFromSession(sess), nil
	}
	sess, err := pds.CreateAccount(ctx, pdsclient.CreateAccountRequest{Identifier: handle, Password: password, Email: handle + "@" + domain})
	if err != nil {
		return PdsSession{}, err
	}
	return pdsSessionFromSession(sess), nil
}

func pdsSessionFromSession(sess pdsclient.Session) PdsSession {
	return PdsSession{Did: sess.DID.String(), AccessJWT: sess.AccessJWT, RefreshJWT: sess.RefreshJWT}
}

func EnsurePdsAccount(ctx context.Context, pdsURL, caPEM, handle, password string) (string, error) {
	sess, err := EnsurePdsAccountSession(ctx, pdsURL, caPEM, handle, password)
	if err != nil {
		return "", err
	}
	return sess.Did, nil
}

func (ctl *Controller) bidderVault(ctx context.Context, spec v1alpha1.MarketBidderSpec, stateDir string) (manifests.BidderVault, error) {
	var vault manifests.BidderVault
	if spec.Identity.Mode == v1alpha1.MarketIdentityModePdsAccount || spec.Profile == BidderProfileNetwork {
		password, v, err := ctl.bidderPassword(ctx)
		if err != nil {
			return v, err
		}
		did, err := EnsurePdsAccount(ctx, spec.Identity.PdsURL, spec.Network.CABundle, spec.Identity.Handle, password)
		if err != nil {
			return v, err
		}
		ctl.c.log().Info("the bidder's PDS account", "handle", spec.Identity.Handle, "pds", spec.Identity.PdsURL, "did", did)
		vault = v
	} else {
		if _, _, _, err := ctl.vaultRefs(ctx, BidderVaultServer, BidderVaultClient); err != nil {
			return vault, err
		}
		vault = manifests.BidderVault{Server: BidderVaultServer, Client: BidderVaultClient}
	}
	if spec.Identity.Mode == v1alpha1.MarketIdentityModeLocalRepo {
		if err := ctl.ensureRepoKey(ctx, BidderVaultServer, BidderVaultClient, identityKeysPath); err != nil {
			return vault, err
		}
	}
	vault.KcpServer = strings.TrimSuffix(ctl.restCfg.Host, "/") + "/clusters/" + ctl.c.workspace()
	return vault, nil
}

var secp256k1N, _ = new(big.Int).SetString("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141", 16)

func NewRepoKeyHex() (string, error) {
	for {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		k := new(big.Int).SetBytes(buf)
		if k.Sign() > 0 && k.Cmp(secp256k1N) < 0 {
			return hex.EncodeToString(buf), nil
		}
	}
}

func (ctl *Controller) ensureRepoKey(ctx context.Context, server, clientName, keysPath string) error {
	client, _, c, err := ctl.vaultClient(ctx, server, clientName)
	if err != nil {
		return err
	}
	defer client.RevokeSelf(context.Background())
	ctl.ensureVaultCredentialStatus(ctx, server, c.KVMount, keysPath)
	path := c.KVMount + "/data/" + keysPath
	data, err := client.GetKV(ctx, path)
	var re *openbao.ResponseError
	if err != nil && !(errors.As(err, &re) && re.Status == http.StatusNotFound) {
		return err
	}
	if k, _ := data[RepoKeyField].(string); k != "" {
		return nil
	}
	key, err := NewRepoKeyHex()
	if err != nil {
		return err
	}
	if data == nil {
		data = map[string]any{}
	}
	data[RepoKeyField] = key
	if err := client.PutKV(ctx, path, data); err != nil {
		return err
	}
	ctl.c.log().Info("the application's repo key is in its own OpenBao", "server", server, "path", path)
	return nil
}

func (ctl *Controller) deleteVaults(ctx context.Context, owner string) (int, error) {
	ri, err := ctl.client(ctl.c.workspace(), OpenBaoServerGVR)
	if err != nil {
		return 0, err
	}
	list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: OwnerLabel + "=" + owner})
	if apierrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for _, o := range list.Items {
		if o.GetDeletionTimestamp() == nil {
			if err := ri.Delete(ctx, o.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return len(list.Items), err
			}
		}
	}
	return len(list.Items), nil
}
