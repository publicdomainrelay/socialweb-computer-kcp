package requester

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	SessionAtircParentEnv = "LC_ATIRC_TOKEN"

	sessionAtircLabel = "ssh"

	sessionAtircBinaryWaitSec = 180
)

type sessionAtirc struct {
	token string

	subhandle string
}

type atircTokenClaims struct {
	Sub string `json:"sub"`

	Subhandle string `json:"subhandle"`

	Exp int64 `json:"exp"`

	Leases []atircLease `json:"leases"`
}

func atircClaimsOf(token string) (atircTokenClaims, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return atircTokenClaims{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return atircTokenClaims{}, false
	}
	var claims atircTokenClaims
	if json.Unmarshal(raw, &claims) != nil {
		return atircTokenClaims{}, false
	}
	return claims, true
}

func splitSessionAtircParent(lc map[string]string) (map[string]string, string) {
	parent := strings.TrimSpace(lc[SessionAtircParentEnv])
	out := make(map[string]string, len(lc))
	for key, value := range lc {
		if key != SessionAtircParentEnv {
			out[key] = value
		}
	}
	return out, parent
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

func (q *Requester) mintSessionAtirc(ctx context.Context, account sshtransport.AuthorizedAccount, parent string, state *v1alpha1.NodeClaimContract) (*sessionAtirc, error) {
	claims, ok := atircClaimsOf(parent)
	switch {
	case !ok || strings.ContainsAny(parent, " \t\r\n"):
		return nil, errors.New(SessionAtircParentEnv + " is not an atirc token")
	case claims.Sub != account.DID:
		return nil, fmt.Errorf("%s belongs to %s, not to the ssh account %s", SessionAtircParentEnv, claims.Sub, account.DID)
	}
	base := strings.ToLower(strings.TrimSpace(claims.Subhandle))
	if base == "" {
		return nil, errors.New(SessionAtircParentEnv + " names no subhandle")
	}
	lease, err := contractLease(state, nil)
	if err != nil {
		return nil, err
	}
	subhandle := atircChild(sessionAtircLabel, uriRKey(state.RFPURI), base)
	if subhandle == "" {
		return nil, fmt.Errorf("%q and %q make no subhandle", base, state.RFPURI)
	}
	now := q.now()
	exp := now.Add(q.sessionAtircLifetime())
	if claims.Exp > 0 && time.Unix(claims.Exp, 0).Before(exp) {
		exp = time.Unix(claims.Exp, 0)
	}
	if !exp.After(now) {
		return nil, fmt.Errorf("the token of %s would expire at %s", subhandle, exp.UTC().Format(time.RFC3339))
	}
	mintCtx, cancel := context.WithTimeout(ctx, atircCallTimeout)
	defer cancel()
	var out struct {
		Token string `json:"token"`
	}
	if err := q.postAtirc(mintCtx, AtircMintNSID, parent, atircMintInput{Subhandle: subhandle, Exp: exp.Unix(), Lease: lease}, &out); err != nil {
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
	if err := checkLeaseChain(leases, claims.Leases, lease); err != nil {
		q.revokeSessionAtirc(ctx, minted)
		return nil, fmt.Errorf("mint %s: %w", subhandle, err)
	}
	return minted, nil
}

func (q *Requester) sessionAtircEnv(ctx context.Context, account sshtransport.AuthorizedAccount, parent string, state *v1alpha1.NodeClaimContract, lc map[string]string, io sshtransport.CommandIO) (map[string]string, *sessionAtirc) {
	if parent == "" {
		return lc, nil
	}
	minted, err := q.mintSessionAtirc(ctx, account, parent, state)
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
