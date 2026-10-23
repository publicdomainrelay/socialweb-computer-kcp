package nodemarket

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type NodeSecret struct {
	Token string

	NodePassword string

	ServerCA string

	ApiserverTicket string

	AdvertiseAddress string

	ApiserverPort int
}

type NodeLogin struct {
	Mount string

	Role string

	Policy string
}

type NodeSecrets interface {
	EnsurePolicy(ctx context.Context, node string) error

	WriteSecret(ctx context.Context, node string, secret NodeSecret) error

	EnsureLogin(ctx context.Context, node string, grant v1alpha1.NodeClaimGrant) (NodeLogin, error)

	Revoke(ctx context.Context, node string) error
}

type NodeSecretsReconciler interface {
	Reconcile(ctx context.Context, live map[string]*v1alpha1.NodeClaimGrant) error
}

type NodeSecretsDeclared interface {
	DeclaredNodes(ctx context.Context) (map[string]bool, error)
}

type Links interface {
	ReadClusterLink(ctx context.Context, logicalCluster, name string) (*v1alpha1.ClusterLink, error)
}

const RevokeTimeout = 10 * time.Second

func ApiserverLinkName(cluster string) string {
	return cluster + "-" + string(v1alpha1.ClusterLinkApiserver)
}

func OpenBaoLinkName(cluster string) string {
	return cluster + "-" + string(v1alpha1.ClusterLinkOpenBao)
}

var ErrLinksNotListening = errors.New("nodemarket: a market node is rented only once both of its " +
	"cluster's links listen, because the guest is handed the OpenBao link's ticket at boot and reads " +
	"the apiserver link's ticket from OpenBao; a guest rented before either exists can only fail, and " +
	"the next pass tries again")

func (h *Host) linksNotListening(ctx context.Context, target Cluster) string {
	if h.opts.Links == nil {
		return "no ClusterLink reader was supplied"
	}
	for _, name := range []string{ApiserverLinkName(h.opts.Cluster), OpenBaoLinkName(h.opts.Cluster)} {
		link, err := h.opts.Links.ReadClusterLink(ctx, target.Workspace, name)
		switch {
		case err != nil:
			return name + ": reading the link failed: " + err.Error()
		case link == nil:
			return name + ": the link does not exist yet"
		case link.Status.Phase != v1alpha1.ClusterLinkListening || link.Status.Ticket == "":
			return fmt.Sprintf("%s: the link reads %q with %d bytes of ticket", name,
				link.Status.Phase, len(link.Status.Ticket))
		}
	}
	return ""
}

func PinnedJoinToken(serverCA, token string) string {
	sum := sha256.Sum256([]byte(serverCA))
	return "K10" + hex.EncodeToString(sum[:]) + "::server:" + strings.TrimSpace(token)
}

func (h *Host) writeSecretsLocked(ctx context.Context, target Cluster) error {
	if h.opts.Secrets == nil {
		return nil
	}
	if !h.policyWritten {
		if err := h.opts.Secrets.EnsurePolicy(ctx, h.opts.NodeName); err != nil {
			return fmt.Errorf("nodemarket: writing the read policy for node %s of %s into the cluster "+
				"OpenBao: %w -- the guest logs in to that policy and nothing else, so a node rented "+
				"without it is one that can never read its own join material", h.opts.NodeName,
				h.opts.Cluster, err)
		}
		h.policyWritten = true
	}
	if h.secretWritten {
		return nil
	}
	ticket, why := h.apiserverTicket(ctx, target)
	if ticket == "" {
		if why == h.heldBack {
			return nil
		}
		h.heldBack = why
		h.log().Info("nodemarket: the node's secret is held back until its cluster's apiserver link "+
			"is listening", "cluster", h.opts.Cluster, "node", h.opts.NodeName,
			"link", ApiserverLinkName(h.opts.Cluster), "reason", why,
			"next", "each later pass reads the link again and writes the secret once it has a ticket; "+
				"the guest's oneshot finds no secret until then and retries")
		return nil
	}
	secret, err := h.readJoinMaterial()
	if err != nil {
		return err
	}
	secret.ApiserverTicket = ticket
	if err := h.opts.Secrets.WriteSecret(ctx, h.opts.NodeName, secret); err != nil {
		return fmt.Errorf("nodemarket: writing the join secret for node %s of %s into the cluster "+
			"OpenBao: %w", h.opts.NodeName, h.opts.Cluster, err)
	}
	h.secretWritten = true
	h.heldBack = ""
	h.log().Info("nodemarket: the node's join secret is in the cluster OpenBao",
		"cluster", h.opts.Cluster, "node", h.opts.NodeName,
		"fields", "token,nodePassword,serverCA,apiserverTicket,advertiseAddress,apiserverPort")
	return nil
}

func (h *Host) apiserverTicket(ctx context.Context, target Cluster) (string, string) {
	if h.opts.Links == nil {
		return "", "no ClusterLink reader was supplied"
	}
	link, err := h.opts.Links.ReadClusterLink(ctx, target.Workspace, ApiserverLinkName(h.opts.Cluster))
	if err != nil {
		return "", "reading the link failed: " + err.Error()
	}
	if link == nil {
		return "", "the link does not exist yet"
	}
	if link.Status.Phase != v1alpha1.ClusterLinkListening || link.Status.Ticket == "" {
		return "", fmt.Sprintf("the link reads %q with %d bytes of ticket", link.Status.Phase,
			len(link.Status.Ticket))
	}
	return link.Status.Ticket, ""
}

func (h *Host) readJoinMaterial() (NodeSecret, error) {
	read := func(label, path string) (string, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("nodemarket: reading the %s of node %s of %s at %s: %w", label,
				h.opts.NodeName, h.opts.Cluster, path, err)
		}
		return string(raw), nil
	}
	token, err := read("k3s token", h.opts.TokenFile)
	if err != nil {
		return NodeSecret{}, err
	}
	serverCA, err := read("server CA", h.opts.ServerCAFile)
	if err != nil {
		return NodeSecret{}, err
	}
	password, err := read("node password", NodePasswordPath(h.opts.DataDir))
	if err != nil {
		return NodeSecret{}, err
	}
	return NodeSecret{
		Token:            PinnedJoinToken(serverCA, token),
		NodePassword:     strings.TrimSpace(password),
		ServerCA:         serverCA,
		AdvertiseAddress: h.opts.AdvertiseAddress,
		ApiserverPort:    h.opts.ApiserverPort,
	}, nil
}

func (h *Host) SyncSecrets(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.opts.Secrets == nil || !h.provisioned {
		return nil
	}
	target, err := h.opts.Resolve(ctx)
	if err != nil {
		return fmt.Errorf("nodemarket: the workspace the claim for node %s of %s lives in: %w",
			h.opts.NodeName, h.opts.Cluster, err)
	}
	if err := h.writeSecretsLocked(ctx, target); err != nil {
		return err
	}
	claim, err := h.opts.Claims.ReadNodeClaim(ctx, target.Workspace, h.opts.NodeName)
	if err != nil {
		return fmt.Errorf("nodemarket: reading the claim for node %s of %s for its grant: %w",
			h.opts.NodeName, h.opts.Cluster, err)
	}
	if claim == nil || claim.Status.Grant == nil || EndedClaimPhase(claim.Status.Phase) {
		return nil
	}
	grant := *claim.Status.Grant
	grant.Role = ""
	if grant.Issuer == "" || grant.Subject == "" || grant.Audience == "" {
		return fmt.Errorf("nodemarket: the claim for node %s of %s carries a grant missing its "+
			"issuer, subject or audience (%q, %q, %q), and a role bound to an empty value is a role "+
			"any token of that issuer satisfies", h.opts.NodeName, h.opts.Cluster, grant.Issuer,
			grant.Subject, grant.Audience)
	}
	if h.granted != nil && *h.granted == grant {
		return nil
	}
	login, err := h.opts.Secrets.EnsureLogin(ctx, h.opts.NodeName, grant)
	if err != nil {
		return fmt.Errorf("nodemarket: the JWT login for node %s of %s in the cluster OpenBao: %w",
			h.opts.NodeName, h.opts.Cluster, err)
	}
	h.granted = &grant
	h.log().Info("nodemarket: the node can log in to the cluster OpenBao with its workload token",
		"cluster", h.opts.Cluster, "node", h.opts.NodeName, "mount", login.Mount, "role", login.Role,
		"policy", login.Policy, "issuer", grant.Issuer, "subject", grant.Subject,
		"audience", grant.Audience)
	return nil
}

func (h *Host) revokeSecretsLocked(ctx context.Context) {
	h.secretWritten = false
	h.policyWritten = false
	h.granted = nil
	h.provisioned = false
	if h.opts.Secrets == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, RevokeTimeout)
	defer cancel()
	if err := h.opts.Secrets.Revoke(ctx, h.opts.NodeName); err != nil {
		h.log().Warn("nodemarket: the node's role, policy or secret could not all be removed from "+
			"the cluster OpenBao; the release goes on, because the machine is what costs and the "+
			"role binds one workload subject the released machine no longer runs",
			"cluster", h.opts.Cluster, "node", h.opts.NodeName, "error", err.Error())
		return
	}
	h.log().Info("nodemarket: removed the node's role, policy and secret from the cluster OpenBao",
		"cluster", h.opts.Cluster, "node", h.opts.NodeName)
}
