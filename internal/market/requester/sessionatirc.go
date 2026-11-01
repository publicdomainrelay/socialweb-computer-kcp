package requester

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

const (
	SessionAtircEnv = "LC_ATIRC"

	sessionAtircLabel = "ssh"

	sessionAtircBinaryWaitSec = 180
)

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

func sessionAtircUserData(userData string) (string, error) {
	return cloudinit.BuildUserData(cloudinit.BuildOptions{Base: userData,
		Modules: []any{cloudinit.AgentModule(cloudinit.AgentOptions{})}})
}

func waitForAtircBinary(command string) string {
	if strings.TrimSpace(command) == "" {
		return command
	}
	return fmt.Sprintf("i=0; while [ ! -x %s ] && [ \"$i\" -lt %d ]; do sleep 1; i=$((i+1)); done; %s",
		cloudinit.AtircBinary, sessionAtircBinaryWaitSec, command)
}

func (q *Requester) sessionAtircLifetime() time.Duration {
	if q.cfg.SessionCap > 0 && q.cfg.SessionCap < AtircTokenLifetime {
		return q.cfg.SessionCap
	}
	return AtircTokenLifetime
}

func (q *Requester) mintSessionAtirc(ctx context.Context, state *v1alpha1.NodeClaimContract) (*sessionAtirc, error) {
	lease, err := contractLease(state, nil)
	if err != nil {
		return nil, err
	}
	issuer, err := q.serviceAtircIssuer(ctx)
	if err != nil {
		return nil, err
	}
	subhandle := atircChild(sessionAtircLabel, uriRKey(state.RFPURI), issuer.base)
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

func (q *Requester) sessionAtircEnv(ctx context.Context, account sshtransport.AuthorizedAccount, want bool, state *v1alpha1.NodeClaimContract, lc map[string]string, io sshtransport.CommandIO) (map[string]string, *sessionAtirc) {
	if !want {
		return lc, nil
	}
	minted, err := q.mintSessionAtirc(ctx, state)
	if err != nil {
		q.log.Warn("requester_session_atirc_mint_failed", "did", account.DID, "err", err.Error())
		io.WriteErr([]byte("atirc: this session runs without an atirc token: " + err.Error() + "\n"))
		return lc, nil
	}
	q.log.Info("requester_session_atirc_minted", "did", account.DID, "subhandle", minted.subhandle)
	env := maps.Clone(lc)
	env[AtircTokenEnv] = minted.token
	return env, minted
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

func (q *Requester) heldContractState(name string) *v1alpha1.NodeClaimContract {
	contract, ok := q.contracts.Get(name)
	if !ok {
		return nil
	}
	return contract.Status.Contract
}
