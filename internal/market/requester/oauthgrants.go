package requester

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/workloadoauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/workloadauth"
)

const (
	oauthGrantsFile = "oauth-grants.json"

	oauthGrantsGiveUp = 2 * time.Minute
)

func GrantRole(grant v1alpha1.RequesterOAuthGrant, account, service string) workloadoauth.Role {
	role := workloadoauth.Role{
		Name:    strings.TrimSpace(grant.Role),
		Account: account,
		Service: service,
		Issuer:  strings.TrimRight(strings.TrimSpace(grant.Issuer), "/"),
		Subject: strings.TrimSpace(grant.Subject),
	}
	for _, c := range grant.Clients {
		role.Clients = append(role.Clients, workloadoauth.Client{ID: strings.TrimSpace(c.ClientID), RedirectURI: strings.TrimSpace(c.RedirectURI), Scopes: c.Scopes})
	}
	return role
}

func (q *Requester) oauthGrantsPath() string {
	dir := q.cfg.dataDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, oauthGrantsFile)
}

func (q *Requester) writtenOAuthGrants() []string {
	path := q.oauthGrantsPath()
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var roles []string
	_ = json.Unmarshal(raw, &roles)
	return roles
}

func (q *Requester) saveOAuthGrants(roles []string) error {
	path := q.oauthGrantsPath()
	if path == "" {
		return nil
	}
	raw, err := json.Marshal(roles)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (q *Requester) syncOAuthGrants(ctx context.Context) {
	grants := q.cfg.Spec.OAuthGrants
	held := q.writtenOAuthGrants()
	if len(grants) == 0 && len(held) == 0 {
		return
	}
	delay := time.Second
	deadline := time.Now().Add(oauthGrantsGiveUp)
	for {
		err := q.syncOAuthGrantsOnce(ctx, grants, held)
		if err == nil || ctx.Err() != nil {
			return
		}
		if time.Now().After(deadline) {
			q.log.Warn("requester_oauth_grants_failed", "err", err.Error())
			return
		}
		q.log.Info("requester_oauth_grants_retry", "err", err.Error(), "retryIn", delay.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func (q *Requester) syncOAuthGrantsOnce(ctx context.Context, grants []v1alpha1.RequesterOAuthGrant, held []string) error {
	var written []string
	if len(grants) > 0 {
		if !q.cfg.LocalSigner || q.cfg.Spec.PdsRef == nil || q.cfg.Spec.PdsRef.Name == "" || q.pdsURL == "" {
			q.log.Warn("requester_oauth_grants_refused", "err", "spec.oauthGrants needs the requester's account on its own PDS (identity mode localRepo)")
			return nil
		}
		service, err := q.pdsServiceDID(ctx)
		if err != nil {
			return err
		}
		for _, grant := range grants {
			record, err := workloadoauth.Record(GrantRole(grant, q.repoDID, service), q.now())
			if err != nil {
				q.log.Warn("requester_oauth_grant_refused", "role", grant.Role, "err", err.Error())
				continue
			}
			if _, err := q.repo.PutRecord(ctx, q.repoDID, workloadauth.RBACCollection, grant.Role, record); err != nil {
				return err
			}
			written = append(written, grant.Role)
			q.log.Info("requester_oauth_grant_written", "role", grant.Role, "issuer", grant.Issuer, "subject", grant.Subject, "clients", len(grant.Clients))
		}
	}
	for _, role := range held {
		if slices.Contains(written, role) {
			continue
		}
		if err := q.repo.DeleteRecord(ctx, q.repoDID, workloadauth.RBACCollection, role); err != nil && !oauthRecordGone(err) {
			return err
		}
		q.log.Info("requester_oauth_grant_deleted", "role", role)
	}
	return q.saveOAuthGrants(written)
}
