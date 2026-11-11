package requester

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
)

const PublicPLCDirectoryURL = "https://plc.directory"

var errAgentWithoutSecrets = errors.New("an agent needs spec.secretsRef: the secrets bundle writes the agent's envFile and mesh key on the guest")

func LocalPdsSignsAtirc(plcURL, pdsURL string) (bool, string) {
	if strings.TrimRight(strings.ToLower(strings.TrimSpace(plcURL)), "/") != PublicPLCDirectoryURL {
		return false, fmt.Sprintf("the DID is on %q, not %s, so irc.fedfork.com cannot resolve it", plcURL, PublicPLCDirectoryURL)
	}
	parsed, err := url.Parse(strings.TrimSpace(pdsURL))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return false, fmt.Sprintf("the Pds public URL %q is not https", pdsURL)
	}
	host := parsed.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsGlobalUnicast() || ip.IsPrivate() {
			return false, fmt.Sprintf("the Pds public URL %q is a private address", pdsURL)
		}
		return true, ""
	}
	if h, err := syntax.ParseHandle(host); err != nil || !h.AllowedTLD() {
		return false, fmt.Sprintf("the Pds public URL %q names no public host", pdsURL)
	}
	return true, ""
}

func (q *Requester) checkAgentHandle(ctx context.Context) error {
	if q.cfg.Spec.AtircParentRef != nil {
		return nil
	}
	handle := strings.ToLower(strings.TrimSpace(q.handle))
	reason := ""
	switch h, err := syntax.ParseHandle(handle); {
	case handle == "":
		reason = "the requester identity has no handle"
	case err != nil:
		reason = fmt.Sprintf("%q is not a handle", handle)
	case !h.AllowedTLD():
		reason = fmt.Sprintf("%s is a .%s handle that nobody resolves", handle, h.TLD())
	default:
		reason = q.handleMismatch(ctx, h)
	}
	if reason == "" {
		return nil
	}
	return fmt.Errorf("the atirc agent needs a requester handle that resolves to %s: %s; start the requester with --atproto-handle <a domain you control> whose DNS TXT _atproto.<handle> or https://<handle>/.well-known/atproto-did names that DID", q.did, reason)
}

func (q *Requester) verifiedHandle(ctx context.Context) string {
	handle := strings.ToLower(strings.TrimSpace(q.handle))
	h, err := syntax.ParseHandle(handle)
	if handle == "" || err != nil || !h.AllowedTLD() || q.deps.Directory == nil || q.handleMismatch(ctx, h) != "" {
		return ""
	}
	return handle
}

func (q *Requester) handleMismatch(ctx context.Context, h syntax.Handle) string {
	if q.deps.Directory == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, atircCallTimeout)
	defer cancel()
	ident, err := q.deps.Directory.LookupHandle(ctx, h)
	switch {
	case err != nil:
		return fmt.Sprintf("%s does not resolve to it: %v", h, err)
	case ident.DID.String() != q.did:
		return fmt.Sprintf("%s resolves to %s", h, ident.DID)
	}
	return ""
}

func (r *contractRunner) ensureAgentBundle(ctx context.Context, contract v1alpha1.ComputeContract) error {
	envFile, err := agentEnvFile(contract)
	if err != nil || envFile == "" {
		return nil
	}
	if r.q.deps.VaultWrite == nil {
		return errors.New("the requester has no vault write handle for the agent's secrets bundle")
	}
	r.bundleMu.Lock()
	defer r.bundleMu.Unlock()
	ref := *contract.Spec.SecretsRef
	kv, err := r.q.deps.VaultWrite(ctx, ref)
	if err != nil {
		return fmt.Errorf("the agent's secrets bundle %s/%s: %w", ref.Mount, ref.Path, err)
	}
	data, err := kv.Read(ctx, ref.Mount, ref.Path, 0)
	if err != nil && !errors.Is(err, vault.ErrNotFound) {
		return fmt.Errorf("reading the agent's secrets bundle %s/%s: %w", ref.Mount, ref.Path, err)
	}
	if text, _ := data["bundle"].(string); strings.TrimSpace(text) != "" {
		return nil
	}
	raw, err := json.Marshal([]SecretEntry{{Path: envFile, Value: "", Mode: "0600"}})
	if err != nil {
		return err
	}
	next := make(map[string]any, len(data)+1)
	for k, v := range data {
		next[k] = v
	}
	next["bundle"] = string(raw)
	if err := kv.Write(ctx, ref.Mount, ref.Path, next); err != nil {
		return fmt.Errorf("writing the agent's placeholder bundle %s/%s: %w", ref.Mount, ref.Path, err)
	}
	r.log().Info("requester_agent_bundle_placeholder", "mount", ref.Mount, "path", ref.Path, "envFile", envFile, "mode", "0600")
	return nil
}

func agentEnvFile(contract v1alpha1.ComputeContract) (string, error) {
	if contract.Spec.Agent == nil {
		return "", nil
	}
	if contract.Spec.SecretsRef == nil {
		return "", errAgentWithoutSecrets
	}
	if envFile := strings.TrimSpace(contract.Spec.Agent.EnvFile); envFile != "" {
		return envFile, nil
	}
	return cloudinit.DefaultAgentEnvFile, nil
}

var ErrTokenPathConflict = errors.New("requester: a path names an atirc token file")

func CheckTokenPaths(contract v1alpha1.ComputeContract, bundlePaths []string) error {
	tokens := map[string]string{cloudinit.GuestRequesterAtircEnvFile: "the guest requester's atirc token"}
	if agent := contract.Spec.Agent; agent != nil {
		envFile := path.Clean(cmp.Or(strings.TrimSpace(agent.EnvFile), cloudinit.DefaultAgentEnvFile))
		if owner, ok := tokens[envFile]; ok {
			return fmt.Errorf("%w: the agent envFile %s is %s", ErrTokenPathConflict, envFile, owner)
		}
		tokens[envFile] = "the agent's atirc mesh env"
		tokens[cloudinit.AgentIrohKeyFile] = "the agent's atirc mesh key"
	}
	check := func(what, p string) error {
		if owner, ok := tokens[path.Clean(p)]; ok {
			return fmt.Errorf("%w: %s %s is %s", ErrTokenPathConflict, what, p, owner)
		}
		return nil
	}
	if contract.Spec.Association != nil {
		if refFile, err := associationRefFile(contract); err == nil {
			if err := check("spec.association.refFile", refFile); err != nil {
				return err
			}
		}
		if err := check("the association request", cloudinit.DefaultAssociationRequestFile); err != nil {
			return err
		}
	}
	for _, p := range bundlePaths {
		if err := check("the secrets bundle path", p); err != nil {
			return err
		}
	}
	return nil
}

func tokenFile(contract v1alpha1.ComputeContract, p string) bool {
	p = path.Clean(p)
	if p == cloudinit.GuestRequesterAtircEnvFile {
		return true
	}
	agent := contract.Spec.Agent
	return agent != nil && (p == cloudinit.AgentIrohKeyFile || path.Clean(cmp.Or(strings.TrimSpace(agent.EnvFile), cloudinit.DefaultAgentEnvFile)) == p)
}
