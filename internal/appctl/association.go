package appctl

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	AssociateConfirmNSID = "com.publicdomainrelay.temp.requester.associateConfirm"
	BadgeBlueKeysNSID    = "com.publicdomainrelay.temp.badgeBlueKeys"
	BidderAssociate      = "bidder_associate"
	AssociationWatch     = "association"
)

func xrpcCall(ctx context.Context, method, rawURL, caFile, bearer string, body any) (map[string]any, error) {
	tr := &http.Transport{}
	if caFile != "" && strings.HasPrefix(rawURL, "https://") {
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("the CA bundle %s does not parse", caFile)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second, Transport: tr}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]any
	json.Unmarshal(raw, &out)
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("%s %s: %d %s", method, rawURL, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return out, nil
}

func (ctl *Controller) associateNetworkOperator(ctx context.Context, spec v1alpha1.MarketBidderSpec) error {
	id, err := ctl.bidderIdentity(ctx)
	if err != nil || !claimOpen(id) || spec.Identity.Handle == "" {
		return err
	}
	password, _, err := ctl.bidderPassword(ctx)
	if err != nil {
		return err
	}
	operator, err := writeClaim(ctx, id, spec.Identity.PdsUrl, spec.Network.CaBundleRef, spec.Identity.Handle, password, "bidder-")
	if err != nil || operator == "" {
		return err
	}
	ctl.c.log().Info("the market network operator account wrote its claim record naming the bidder's own DID", "operator", operator,
		"handle", spec.Identity.Handle, "bidderDid", id.Status.Did)
	return nil
}

func claimOpen(id *v1alpha1.MarketIdentity) bool {
	if id == nil || id.Status.Phase != v1alpha1.MarketIdentityPhaseAwaitingScan || id.Status.Did == "" || id.Status.PendingAction == nil {
		return false
	}
	for _, c := range id.Status.Conditions {
		if c.Type == v1alpha1.ConditionAssociated && c.Reason == "Revoked" {
			return false
		}
	}
	return true
}

func claimOf(rawURL string) (nonce, service string) {
	_, frag, _ := strings.Cut(rawURL, "#")
	q, _ := url.ParseQuery(frag)
	return q.Get("n"), q.Get("s")
}

func writeClaim(ctx context.Context, id *v1alpha1.MarketIdentity, operatorPds, ca, identifier, password, namePrefix string) (string, error) {
	nonce, service := claimOf(id.Status.PendingAction.Url)
	if nonce == "" || service == "" {
		return "", nil
	}
	did := id.Status.Did
	pds := strings.TrimSuffix(operatorPds, "/")
	sess, err := xrpcCall(ctx, http.MethodPost, pds+"/xrpc/com.atproto.server.createSession", ca, "",
		map[string]string{"identifier": identifier, "password": password})
	if err != nil {
		return "", err
	}
	operator, _ := sess["did"].(string)
	access, _ := sess["accessJwt"].(string)
	rec := map[string]any{"$type": BadgeBlueKeysNSID, "keyId": did, "challenge": operator, "service": service, "nonce": nonce,
		"name": namePrefix + did[len(did)-8:], "createdAt": time.Now().UTC().Format(time.RFC3339)}
	if _, err := xrpcCall(ctx, http.MethodPost, pds+"/xrpc/com.atproto.repo.putRecord", ca, access,
		map[string]any{"repo": operator, "collection": BadgeBlueKeysNSID, "rkey": service + "-" + strings.ReplaceAll(did, ":", "-"), "record": rec}); err != nil {
		return "", err
	}
	return operator, nil
}

func (ctl *Controller) identityIn(ctx context.Context, ws, name string) (*v1alpha1.MarketIdentity, error) {
	ri, err := ctl.client(ws, MarketIdentityGVR)
	if err != nil {
		return nil, err
	}
	u, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var id v1alpha1.MarketIdentity
	if err := fromUnstructured(u, &id); err != nil {
		return nil, err
	}
	return &id, nil
}

func (ctl *Controller) associateGatewayOperator(ctx context.Context, id *v1alpha1.MarketIdentity, sessionsFile string, n *v1alpha1.MarketNetworkValue) error {
	if !claimOpen(id) || n == nil {
		return nil
	}
	b, err := os.ReadFile(sessionsFile)
	if err != nil {
		return err
	}
	var sessions map[string]ownerSession
	if err := json.Unmarshal(b, &sessions); err != nil {
		return fmt.Errorf("the owner sessions file %s: %w", sessionsFile, err)
	}
	owners := make([]string, 0, len(sessions))
	for did := range sessions {
		owners = append(owners, did)
	}
	if len(owners) == 0 {
		return fmt.Errorf("the owner sessions file %s holds no session", sessionsFile)
	}
	slices.Sort(owners)
	s := sessions[owners[0]]
	operator, err := writeClaim(ctx, id, s.Service, n.CaBundleRef, s.Identifier, s.Password, "requester-")
	if err != nil || operator == "" {
		return err
	}
	ctl.c.log().Info("the market network operator account wrote its claim record naming the gateway requester's own DID", "operator", operator,
		"handle", s.Identifier, "requesterDid", id.Status.Did)
	return nil
}
