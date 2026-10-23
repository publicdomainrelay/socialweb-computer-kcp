package requester

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"
)

func meshdPinOf(meshd *v1alpha1.ComputeContractMeshd) *cloudinit.MeshdPin {
	if meshd == nil || meshd.DownloadBase == "" {
		return nil
	}
	out := &cloudinit.MeshdPin{DownloadBase: meshd.DownloadBase, Sha256: map[string]string{}}
	for _, pin := range meshd.Pins {
		out.Sha256[pin.Arch] = pin.Sha256
	}
	return out
}

func nodeAgentPinFromJoin(join *v1alpha1.ComputeContractNodeJoin) *cloudinit.NodeAgentPin {
	if join == nil || join.NodeAgent == nil || join.NodeAgent.DownloadBase == "" {
		return nil
	}
	out := &cloudinit.NodeAgentPin{DownloadBase: join.NodeAgent.DownloadBase, Sha256: map[string]string{}}
	for _, pin := range join.NodeAgent.Pins {
		out.Sha256[pin.Arch] = pin.Sha256
	}
	return out
}

func (r *contractRunner) ensureSecrets(ctx context.Context, contract v1alpha1.ComputeContract) error {
	if contract.Spec.SecretsRef == nil || r.secrets != nil {
		return nil
	}
	entries, err := r.svc.readSecretsBundle(ctx, *contract.Spec.SecretsRef)
	if err != nil {
		return err
	}
	url, err := r.svc.ingressExposureURL(ctx)
	if err != nil {
		return err
	}
	state := newSecretsState(r.name, r.svc.log)
	state.addEntries(entries...)
	r.svc.registerSecrets(r.name, state)
	r.mu.Lock()
	r.secrets = state
	r.secretsURL = url
	r.mu.Unlock()
	if grant := contract.Status.Grant; grant != nil && contract.Spec.NodeJoin != nil {
		audience := secretsAudience(r.svc.callbackDID())
		state.install(grant.Issuer, audience, grant.Subject, newIssuerVerifier(grant.Issuer, r.svc.cfg.HTTPClient))
		doc, err := json.Marshal(nodeClaimDocument(contract.Spec.NodeJoin.ClusterRef.Name, grant.Role, grant.Issuer))
		if err != nil {
			return err
		}
		state.addEntries(secretEntry{Path: cloudinit.NodeClaimDocPath, Value: string(doc) + "\n"})
		record := nodeRBACRecord(grant.Role, grant.Issuer, grant.Subject, audience, grant.Audience, true)
		key := secretsRBACKey(r.name)
		if r.svc.repo != nil {
			if _, err := r.svc.repo.PutRecord(ctx, r.svc.cfg.RepoDID, FedproxyRBACNSID, key, record); err != nil {
				return fmt.Errorf("requester: rewriting the contract's RBAC record: %w", err)
			}
			r.mu.Lock()
			r.rbacRKey = key
			r.mu.Unlock()
		}
	}
	return nil
}

func (r *contractRunner) contractModules(ctx context.Context, userData string, contract v1alpha1.ComputeContract) (string, error) {
	modules := []any{}
	values := cloudinit.Context{}
	if contract.Spec.NodeJoin != nil {
		opts := cloudinit.K3sJoinOptions{Meshd: meshdPinOf(contract.Spec.NodeJoin.Meshd),
			NodeAgent: nodeAgentPinFromJoin(contract.Spec.NodeJoin)}
		modules = append(modules, "k3s", cloudinit.K3sJoinModule(opts))
	}
	if r.secrets != nil {
		modules = append(modules, "secrets")
		values.SecretsURL = r.secretsURLValue()
		values.SecretsRoute = secretsRouteOf(r.name)
		values.SecretsAud = secretsAudience(r.svc.callbackDID())
		values.SecretsAcceptPath = contractAcceptPath(contract)
	}
	if len(modules) == 0 {
		return userData, nil
	}
	return cloudinit.BuildUserData(cloudinit.BuildOptions{Base: userData, Modules: modules, Ctx: values})
}

func contractAcceptPath(contract v1alpha1.ComputeContract) string {
	if contract.Spec.NodeJoin != nil {
		return cloudinit.NodeSecretsDir + "/accept.json"
	}
	return "/root/secrets/publicdomainrelay.com/market/accept.json"
}

func (r *contractRunner) prepareWorkloadGrant(ctx context.Context, state *v1alpha1.NodeClaimContract, winner *BidRecord) error {
	if winner == nil || (r.nodeJoin == nil && r.secrets == nil) {
		return nil
	}
	reader := market.NewHTTPReader(r.svc.directory)
	reader.Client = r.svc.cfg.HTTPClient
	cfg, err := resolveBidConfig(ctx, reader, winner.Record)
	if err != nil {
		return err
	}
	vmName := state.VMName
	if vmName == "" {
		vmName = r.snapshot().Spec.VMName
	}
	subject := ""
	if r.nodeJoin != nil {
		grant, err := nodeGrant(winner.DID, r.svc.callbackDID(), vmName, r.nodeJoin.ClusterRef.Name, cfg)
		if err != nil {
			return err
		}
		r.mu.Lock()
		r.grant = grant
		r.mu.Unlock()
		subject = grant.Subject
	} else {
		subject, err = guestidentity.SubjectFor(winner.DID, r.svc.callbackDID(), vmName)
		if err != nil {
			return err
		}
	}
	if r.secrets == nil {
		return nil
	}
	audience := secretsAudience(r.svc.callbackDID())
	r.secrets.install(cfg.IssuerURI, audience, subject, newIssuerVerifier(cfg.IssuerURI, r.svc.cfg.HTTPClient))
	if r.nodeJoin != nil {
		doc, err := json.Marshal(nodeClaimDocument(r.nodeJoin.ClusterRef.Name, vmName, cfg.IssuerURI))
		if err != nil {
			return err
		}
		r.secrets.addEntries(secretEntry{Path: cloudinit.NodeClaimDocPath, Value: string(doc) + "\n"})
	}
	nodeAud := ""
	if r.nodeJoin != nil {
		nodeAud = cloudinit.NodeAudience(r.nodeJoin.ClusterRef.Name, vmName)
	}
	record := nodeRBACRecord(vmName, cfg.IssuerURI, subject, audience, nodeAud, r.nodeJoin != nil)
	key := secretsRBACKey(r.name)
	if r.svc.repo == nil {
		return errors.New("requester: the contract needs an RBAC record and the repo is not configured")
	}
	if _, err := r.svc.repo.PutRecord(ctx, r.svc.cfg.RepoDID, FedproxyRBACNSID, key, record); err != nil {
		return fmt.Errorf("requester: writing the contract's RBAC record: %w", err)
	}
	r.mu.Lock()
	r.rbacRKey = key
	r.mu.Unlock()
	return nil
}

func (r *contractRunner) cleanup() {
	r.mu.Lock()
	secrets := r.secrets
	r.mu.Unlock()
	if secrets != nil {
		r.svc.unregisterSecrets(r.name)
	}
}

func (r *contractRunner) deleteRBAC(ctx context.Context) {
	r.mu.Lock()
	key := r.rbacRKey
	r.mu.Unlock()
	if key == "" || r.svc.repo == nil {
		return
	}
	deleteCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := r.svc.repo.DeleteRecord(deleteCtx, r.svc.cfg.RepoDID, FedproxyRBACNSID, key); err != nil && !isNotFound(err) {
		r.log().Warn("requester_rbac_delete_failed", "rkey", key, "err", err)
	}
	r.mu.Lock()
	r.rbacRKey = ""
	r.mu.Unlock()
}

func (r *contractRunner) secretsFetched() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.secretsFetchedAt
}

func (r *contractRunner) setSecretsFetched(at string) {
	if at == "" {
		return
	}
	r.mu.Lock()
	r.secretsFetchedAt = at
	r.mu.Unlock()
}

func (r *contractRunner) secretsURLValue() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.secretsURL
}

func (r *contractRunner) grantValue() *v1alpha1.NodeClaimGrant {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.grant == nil {
		return nil
	}
	grant := *r.grant
	return &grant
}
