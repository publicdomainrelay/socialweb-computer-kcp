package requester

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

const (
	SessionAtircEnv = "LC_ATIRC"

	sessionAtircTokenEnv = "LC_ATIRC_TOKEN"

	sessionAtircLabel = "ssh"

	sessionAtircBinaryWaitSec = 180

	sessionAgentCommand = "atirc agent run"

	maxAtircSubhandle = 253
)

var atircSubhandleLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type sessionAtirc struct {
	token string

	subhandle string
}

func sessionWantsAtirc(command string, lc map[string]string) bool {
	if value, set := lc[SessionAtircEnv]; set {
		return truthy(value)
	}
	fields := strings.Fields(command)
	return len(fields) > 0 && fields[0] == "atirc"
}

func sessionAgentRun(command string) (string, bool) {
	fields := strings.Fields(command)
	if len(fields) != 4 || fields[0] != "atirc" || fields[1] != "agent" || fields[2] != "run" {
		return "", false
	}
	return fields[3], true
}

func agentSubhandle(sub, handle string) (string, error) {
	handle = strings.ToLower(strings.TrimSpace(handle))
	if handle == "" {
		return "", errors.New("the requester identity has no handle")
	}
	value := strings.ToLower(sub)
	if value != handle && !strings.HasSuffix(value, "."+handle) {
		value += "." + handle
	}
	bad := len(value) > maxAtircSubhandle
	for _, label := range strings.Split(value, ".") {
		bad = bad || !atircSubhandleLabel.MatchString(label)
	}
	if bad {
		return "", fmt.Errorf("%q is not a subhandle atirc takes; use DNS labels of letters, digits and '-' below %s", sub, handle)
	}
	return value, nil
}

func sessionAtircUserData(userData string) (string, error) {
	return cloudinit.BuildUserData(cloudinit.BuildOptions{Base: userData,
		Modules: []any{cloudinit.AgentModule(cloudinit.AgentOptions{})}})
}

func atircCommand(command string) string {
	if strings.TrimSpace(command) == "" {
		return command
	}
	return fmt.Sprintf("i=0; while { [ ! -x %s ] || [ ! -x %s ]; } && [ \"$i\" -lt %d ]; do sleep 1; i=$((i+1)); done; export %s=\"$%s\"; unset %s; exec %s %s",
		cloudinit.AtircBinary, cloudinit.AgentExecPath, sessionAtircBinaryWaitSec, AtircTokenEnv, sessionAtircTokenEnv, sessionAtircTokenEnv,
		cloudinit.AgentExecPath, "'"+strings.ReplaceAll(command, "'", "'\"'\"'")+"'")
}

func (q *Requester) sessionAtircLifetime() time.Duration {
	if q.cfg.SessionCap > 0 && q.cfg.SessionCap < AtircTokenLifetime {
		return q.cfg.SessionCap
	}
	return AtircTokenLifetime
}

func (q *Requester) mintSessionAtirc(ctx context.Context, state *v1alpha1.NodeClaimContract, agent string) (*sessionAtirc, error) {
	lease, err := contractLease(state, nil)
	if err != nil {
		return nil, err
	}
	issuer, err := q.serviceAtircIssuer(ctx)
	if err != nil {
		return nil, err
	}
	subhandle := agent
	if subhandle == "" {
		subhandle = atircChild(sessionAtircLabel, uriRKey(state.RFPURI), issuer.base)
	}
	if subhandle == "" {
		return nil, fmt.Errorf("%q and %q make no subhandle", issuer.base, state.RFPURI)
	}
	exp := q.now().Add(q.sessionAtircLifetime())
	mintCtx, cancel := context.WithTimeout(ctx, atircCallTimeout)
	defer cancel()
	var out struct {
		Token string `json:"token"`
	}
	if err := q.postAtirc(mintCtx, AtircMintNSID, issuer.bearer, atircMintInput{Subhandle: subhandle, Exp: exp.Unix(), Lease: lease}, &out); err != nil {
		return nil, fmt.Errorf("mint %s: %w", subhandle, err)
	}
	minted := &sessionAtirc{token: strings.TrimSpace(out.Token), subhandle: subhandle}
	if minted.token == "" || strings.ContainsAny(minted.token, " \t\r\n") {
		return nil, errors.New(q.atircBase() + " answered no token")
	}
	leases, ok := atircTokenLeases(minted.token)
	if !ok {
		q.revokeSessionAtirc(ctx, minted)
		return nil, fmt.Errorf("mint %s: %w (%s)", subhandle, errAtircLeaseMissing, q.atircBase())
	}
	if err := checkLeaseChain(leases, issuer.leases, lease); err != nil {
		q.revokeSessionAtirc(ctx, minted)
		return nil, fmt.Errorf("mint %s: %w", subhandle, err)
	}
	return minted, nil
}

func (q *Requester) sessionAtirc(ctx context.Context, account sshtransport.AuthorizedAccount, want bool, agent string, state *v1alpha1.NodeClaimContract, io sshtransport.CommandIO) (*sessionAtirc, error) {
	if !want {
		return nil, nil
	}
	minted, err := q.mintSessionAtirc(ctx, state, agent)
	switch {
	case err != nil && agent != "":
		q.log.Warn("requester_session_atirc_mint_failed", "did", account.DID, "subhandle", agent, "err", err.Error())
		_ = io.WriteErr([]byte("atirc agent run: no token for " + agent + ": " + err.Error() + "\n"))
		return nil, err
	case err != nil:
		q.log.Warn("requester_session_atirc_mint_failed", "did", account.DID, "err", err.Error())
		_ = io.WriteErr([]byte("atirc: this session runs without an atirc token: " + err.Error() + "\n"))
		return nil, nil
	}
	q.log.Info("requester_session_atirc_minted", "did", account.DID, "subhandle", minted.subhandle, "agent", agent != "")
	return minted, nil
}

func (m *sessionAtirc) env() map[string]string {
	if m == nil {
		return nil
	}
	return map[string]string{sessionAtircTokenEnv: m.token}
}

func (q *Requester) revokeSessionAtirc(ctx context.Context, minted *sessionAtirc) {
	if minted == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), atircCallTimeout)
	defer cancel()
	var out struct {
		RevokedAt int64 `json:"revokedAt"`
	}
	err := q.postAtirc(ctx, AtircRevokeNSID, minted.token, atircRevokeInput{Subhandle: minted.subhandle}, &out)
	var xerr *atircError
	switch {
	case err == nil:
		q.log.Info("requester_session_atirc_revoked", "subhandle", minted.subhandle)
	case errors.As(err, &xerr) && xerr.status == http.StatusUnauthorized:
		q.log.Info("requester_session_atirc_revoked", "subhandle", minted.subhandle, "already", true)
	default:
		q.log.Warn("requester_session_atirc_revoke_failed", "subhandle", minted.subhandle, "err", err.Error())
	}
}
