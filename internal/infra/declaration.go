package infra

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	record "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/infra"
)

type Kind string

const (
	KindInfra Kind = "Infra"

	KindEphemeralInfra Kind = "EphemeralInfra"
)

func (k Kind) NSID() string {
	if k == KindEphemeralInfra {
		return record.EphemeralInfraNSID
	}
	return record.InfraNSID
}

func (k Kind) Prefix() string {
	if k == KindEphemeralInfra {
		return "einfra"
	}
	return "infra"
}

var ErrInvalidDeclaration = errors.New("infra: invalid declaration")

var labelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

type Declaration struct {
	Description string `json:"description,omitempty"`

	Contracts []v1alpha1.InfraContract `json:"contracts,omitempty"`

	TTLSeconds int32 `json:"ttlSeconds,omitempty"`

	EndWhen v1alpha1.EphemeralInfraEnd `json:"endWhen,omitempty"`
}

func FromSpec(spec v1alpha1.InfraSpec) Declaration {
	return normalize(Declaration{Description: spec.Description, Contracts: spec.Contracts})
}

func FromEphemeralSpec(spec v1alpha1.EphemeralInfraSpec) Declaration {
	d := FromSpec(spec.InfraSpec)
	d.TTLSeconds = DefaultTTL
	if spec.TTLSeconds != nil {
		d.TTLSeconds = *spec.TTLSeconds
	}
	d.EndWhen = spec.EndWhen
	return normalize(d)
}

const DefaultTTL int32 = 3600

func normalize(d Declaration) Declaration {
	out := Declaration{Description: d.Description, TTLSeconds: d.TTLSeconds, EndWhen: d.EndWhen}
	for _, c := range d.Contracts {
		c.State = StateOrStaged(c.State)
		c.BidderDids = nonEmpty(c.BidderDids)
		c.DenyBidderDids = nonEmpty(c.DenyBidderDids)
		c.SshAuthorizedKeys = nonEmpty(c.SshAuthorizedKeys)
		c.Tunnels = normalTunnels(c.Tunnels)
		c.Agent = normalAgent(c.Agent)
		c.Association = normalAssociation(c.Association)
		c.GuestTools = normalGuestTools(c.GuestTools)
		out.Contracts = append(out.Contracts, c)
	}
	if out.TTLSeconds != 0 && out.EndWhen == "" {
		out.EndWhen = v1alpha1.EphemeralInfraEndComplete
	}
	return out
}

func StateOrStaged(s v1alpha1.InfraContractState) v1alpha1.InfraContractState {
	if s == "" {
		return v1alpha1.InfraContractStaged
	}
	return s
}

func normalTunnels(in []v1alpha1.ComputeContractTunnel) []v1alpha1.ComputeContractTunnel {
	if len(in) == 0 {
		return nil
	}
	out := make([]v1alpha1.ComputeContractTunnel, 0, len(in))
	for _, t := range in {
		if t.Transport == "" {
			t.Transport = v1alpha1.TunnelTransportOpenTunnel
		}
		if t.Shell != nil {
			shell := *t.Shell
			if shell.EnvFile == "" {
				shell.EnvFile = DefaultShellEnvFile
			}
			t.Shell = &shell
		}
		out = append(out, t)
	}
	return out
}

const DefaultShellEnvFile = "/etc/wootty/wootty.env"

func normalAgent(in *v1alpha1.ComputeContractAgent) *v1alpha1.ComputeContractAgent {
	if in == nil {
		return nil
	}
	out := *in
	if out.EnvFile == "" {
		out.EnvFile = DefaultAgentEnvFile
	}
	return &out
}

const DefaultAgentEnvFile = "/etc/atirc/atirc.env"

const DefaultAssociationRefFile = "/etc/socialweb-computer/association-ref"

func normalAssociation(in *v1alpha1.ComputeContractAssociation) *v1alpha1.ComputeContractAssociation {
	if in == nil || (in.Subject == "" && !in.FromGuest) {
		return nil
	}
	out := *in
	if out.RefFile == "" {
		out.RefFile = DefaultAssociationRefFile
	}
	return &out
}

func normalGuestTools(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return slices.Compact(slices.Sorted(slices.Values(in)))
}

func nonEmpty(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return in
}

func (d Declaration) Published() Declaration {
	out := Declaration{Description: d.Description, TTLSeconds: d.TTLSeconds, EndWhen: d.EndWhen}
	for _, c := range d.Contracts {
		out.Contracts = append(out.Contracts, v1alpha1.InfraContract{Name: c.Name, State: c.State,
			ComputeContractTemplate: v1alpha1.ComputeContractTemplate{VMName: c.VMName, BidderDids: c.BidderDids,
				DenyBidderDids: c.DenyBidderDids, Tunnels: c.Tunnels, Agent: c.Agent, Association: c.Association, GuestTools: c.GuestTools}})
	}
	return normalize(out)
}

func (d Declaration) Hash() string {
	b, _ := json.Marshal(normalize(d))
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (d Declaration) Validate() error {
	seen := map[string]bool{}
	for _, c := range d.Contracts {
		if len(c.Name) > 63 || !labelPattern.MatchString(c.Name) {
			return fmt.Errorf("%w: the contract name %q is not a DNS-1123 label", ErrInvalidDeclaration, c.Name)
		}
		if seen[c.Name] {
			return fmt.Errorf("%w: the contract name %q is declared twice", ErrInvalidDeclaration, c.Name)
		}
		seen[c.Name] = true
		switch StateOrStaged(c.State) {
		case v1alpha1.InfraContractStaged, v1alpha1.InfraContractRequested, v1alpha1.InfraContractReleased:
		default:
			return fmt.Errorf("%w: the contract %s has the state %q", ErrInvalidDeclaration, c.Name, c.State)
		}
		if c.ReleaseRequestedAt != "" {
			if _, err := time.Parse(time.RFC3339, c.ReleaseRequestedAt); err != nil {
				return fmt.Errorf("%w: the contract %s has releaseRequestedAt %q, not an RFC 3339 time", ErrInvalidDeclaration, c.Name, c.ReleaseRequestedAt)
			}
		}
		if err := validTunnels(c); err != nil {
			return err
		}
	}
	if d.TTLSeconds != 0 && d.TTLSeconds < 60 {
		return fmt.Errorf("%w: ttlSeconds %d is under 60", ErrInvalidDeclaration, d.TTLSeconds)
	}
	switch d.EndWhen {
	case "", v1alpha1.EphemeralInfraEndComplete, v1alpha1.EphemeralInfraEndDeadline:
	default:
		return fmt.Errorf("%w: endWhen %q", ErrInvalidDeclaration, d.EndWhen)
	}
	return nil
}

type Observed struct {
	Phase v1alpha1.InfraPhase

	Contracts []v1alpha1.InfraContractStatus

	ExpiresAt string

	EndedAt string

	EndReason v1alpha1.EphemeralInfraEnd
}

func (o Observed) Published() Observed {
	out := o
	out.Contracts = nil
	for _, c := range o.Contracts {
		out.Contracts = append(out.Contracts, v1alpha1.InfraContractStatus{Name: c.Name, Phase: c.Phase, Winner: c.Winner, ReceiptURI: c.ReceiptURI})
	}
	return out
}

var recordStates = map[v1alpha1.InfraContractState]string{
	v1alpha1.InfraContractStaged:    "staged",
	v1alpha1.InfraContractRequested: "requested",
	v1alpha1.InfraContractReleased:  "released",
}

func toRecordContract(c v1alpha1.InfraContract) (record.InfraContract, error) {
	c.State = v1alpha1.InfraContractState(recordStates[StateOrStaged(c.State)])
	var out record.InfraContract
	b, err := json.Marshal(c)
	if err == nil {
		err = json.Unmarshal(b, &out)
	}
	if err != nil {
		return record.InfraContract{}, fmt.Errorf("infra: rendering the contract %s: %w", c.Name, err)
	}
	return out, nil
}

func fromRecordContract(c record.InfraContract) (v1alpha1.InfraContract, error) {
	var state v1alpha1.InfraContractState
	for kcp, rec := range recordStates {
		if rec == strings.TrimSpace(c.State) {
			state = kcp
		}
	}
	if state == "" {
		return v1alpha1.InfraContract{}, fmt.Errorf("%w: the contract %s has the record state %q", ErrInvalidDeclaration, c.Name, c.State)
	}
	var out v1alpha1.InfraContract
	b, err := json.Marshal(c)
	if err == nil {
		err = json.Unmarshal(b, &out)
	}
	if err != nil {
		return v1alpha1.InfraContract{}, fmt.Errorf("%w: the contract %s: %v", ErrInvalidDeclaration, c.Name, err)
	}
	out.State = state
	return out, nil
}

func validTunnels(c v1alpha1.InfraContract) error {
	seen := map[string]bool{}
	for _, t := range c.Tunnels {
		if len(t.Tag) > 32 || !labelPattern.MatchString(t.Tag) {
			return fmt.Errorf("%w: the contract %s has the tunnel tag %q, not a DNS-1123 label of at most 32", ErrInvalidDeclaration, c.Name, t.Tag)
		}
		if seen[t.Tag] {
			return fmt.Errorf("%w: the contract %s declares the tunnel %s twice", ErrInvalidDeclaration, c.Name, t.Tag)
		}
		seen[t.Tag] = true
		if t.Port < 1 || t.Port > 65535 {
			return fmt.Errorf("%w: the contract %s has the tunnel %s on port %d", ErrInvalidDeclaration, c.Name, t.Tag, t.Port)
		}
		switch t.Transport {
		case "", v1alpha1.TunnelTransportOpenTunnel, v1alpha1.TunnelTransportWebRTC:
		default:
			return fmt.Errorf("%w: the contract %s has the tunnel %s with transport %q", ErrInvalidDeclaration, c.Name, t.Tag, t.Transport)
		}
	}
	return nil
}

func toRecordStatus(o Observed) record.InfraStatus {
	out := record.InfraStatus{Phase: strings.ToLower(string(o.Phase))}
	for _, c := range o.Contracts {
		out.Contracts = append(out.Contracts, record.InfraContractStatus{Name: c.Name, Phase: strings.ToLower(string(c.Phase)),
			Winner: c.Winner, Receipt: c.ReceiptURI, Message: c.Message})
	}
	return out
}

func Render(kind Kind, d Declaration, o Observed, createdAt time.Time) (json.RawMessage, error) {
	d = normalize(d)
	var contracts []record.InfraContract
	for _, c := range d.Contracts {
		rc, err := toRecordContract(c)
		if err != nil {
			return nil, err
		}
		contracts = append(contracts, rc)
	}
	created := createdAt.UTC().Format(time.RFC3339)
	status := toRecordStatus(o)
	var value any
	if kind == KindEphemeralInfra {
		ttl := d.TTLSeconds
		if ttl == 0 {
			ttl = DefaultTTL
		}
		value = record.EphemeralInfra{Type: kind.NSID(), Description: d.Description, Contracts: contracts, TtlSeconds: int64(ttl),
			EndWhen: strings.ToLower(string(d.EndWhen)), CreatedAt: created,
			Status: &record.EphemeralInfraStatus{Phase: status.Phase, Contracts: status.Contracts, ExpiresAt: o.ExpiresAt, EndedAt: o.EndedAt,
				EndReason: strings.ToLower(string(o.EndReason))}}
	} else {
		value = record.Infra{Type: kind.NSID(), Description: d.Description, Contracts: contracts, CreatedAt: created, Status: &status}
	}
	return json.Marshal(value)
}

const ReservedRkey = "self"

var ErrReserved = errors.New("infra: the record at rkey self is the account's index of live deployments, not a deployment")

type Decoded struct {
	Declaration Declaration

	CreatedAt time.Time

	TTLSeconds int32

	EndedAt string

	EndReason v1alpha1.EphemeralInfraEnd
}

func (d Decoded) Ended() bool {
	return d.EndedAt != ""
}

func (d Decoded) Expired(kind Kind, now time.Time) bool {
	if kind != KindEphemeralInfra || d.CreatedAt.IsZero() || d.TTLSeconds <= 0 {
		return false
	}
	return !now.Before(d.CreatedAt.Add(time.Duration(d.TTLSeconds) * time.Second))
}

func (d Decoded) Over(kind Kind, now time.Time) bool {
	return d.Ended() || d.Expired(kind, now)
}

var endReasons = map[string]v1alpha1.EphemeralInfraEnd{
	"complete": v1alpha1.EphemeralInfraEndComplete,
	"deadline": v1alpha1.EphemeralInfraEndDeadline,
	"deleted":  v1alpha1.EphemeralInfraEndDeleted,
}

func endReason(raw string) v1alpha1.EphemeralInfraEnd {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if r, ok := endReasons[raw]; ok {
		return r
	}
	if raw == "" {
		return v1alpha1.EphemeralInfraEndDeleted
	}
	return v1alpha1.EphemeralInfraEnd(raw)
}

func Decode(kind Kind, raw json.RawMessage) (Decoded, error) {
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return Decoded{}, fmt.Errorf("%w: the record is not a JSON object", ErrInvalidDeclaration)
	}
	var out Decoded
	var d Declaration
	var created, ended, reason string
	var contracts []record.InfraContract
	var invalid error
	if kind == KindEphemeralInfra {
		r, err := record.ParseEphemeralInfra(value)
		if err != nil {
			return Decoded{}, err
		}
		d.Description, contracts, created = r.Description, r.Contracts, r.CreatedAt
		if r.Status != nil {
			ended, reason = strings.TrimSpace(r.Status.EndedAt), r.Status.EndReason
		}
		switch {
		case r.TtlSeconds == 0:
			d.TTLSeconds = DefaultTTL
		case r.TtlSeconds < 60 || r.TtlSeconds > math.MaxInt32:
			invalid = fmt.Errorf("%w: ttlSeconds %d is outside 60 to %d", ErrInvalidDeclaration, r.TtlSeconds, math.MaxInt32)
		default:
			d.TTLSeconds = int32(r.TtlSeconds)
		}
		switch strings.TrimSpace(r.EndWhen) {
		case "", "complete":
			d.EndWhen = v1alpha1.EphemeralInfraEndComplete
		case "deadline":
			d.EndWhen = v1alpha1.EphemeralInfraEndDeadline
		default:
			invalid = cmp.Or(invalid, fmt.Errorf("%w: endWhen %q", ErrInvalidDeclaration, r.EndWhen))
		}
	} else {
		r, err := record.ParseInfra(value)
		if err != nil {
			return Decoded{}, err
		}
		if r.Index != nil {
			return Decoded{}, ErrReserved
		}
		d.Description, contracts, created = r.Description, r.Contracts, r.CreatedAt
		if r.Status != nil {
			ended, reason = strings.TrimSpace(r.Status.EndedAt), r.Status.EndReason
		}
	}
	if t, err := time.Parse(time.RFC3339, created); err == nil {
		out.CreatedAt = t
	}
	out.TTLSeconds = d.TTLSeconds
	if ended != "" {
		out.EndedAt, out.EndReason = ended, endReason(reason)
	}
	if invalid != nil {
		return out, invalid
	}
	for _, c := range contracts {
		kc, err := fromRecordContract(c)
		if err != nil {
			return out, err
		}
		d.Contracts = append(d.Contracts, kc)
	}
	d = normalize(d)
	if err := d.Validate(); err != nil {
		return out, err
	}
	out.Declaration = d
	return out, nil
}

func Final(kind Kind, raw json.RawMessage, o Observed) (json.RawMessage, error) {
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return nil, fmt.Errorf("%w: the record is not a JSON object", ErrInvalidDeclaration)
	}
	status, _ := value["status"].(map[string]any)
	if status == nil {
		status = map[string]any{}
	}
	status["phase"] = strings.ToLower(string(v1alpha1.InfraPhaseEnded))
	status["endedAt"] = o.EndedAt
	status["endReason"] = strings.ToLower(string(cmp.Or(o.EndReason, v1alpha1.EphemeralInfraEndDeleted)))
	if len(o.Contracts) > 0 {
		var contracts []any
		b, err := json.Marshal(toRecordStatus(o).Contracts)
		if err == nil {
			err = json.Unmarshal(b, &contracts)
		}
		if err != nil {
			return nil, fmt.Errorf("infra: rendering the final contract status: %w", err)
		}
		status["contracts"] = contracts
	}
	if _, ok := status["expiresAt"]; !ok && kind == KindEphemeralInfra && o.ExpiresAt != "" {
		status["expiresAt"] = o.ExpiresAt
	}
	value["status"] = status
	if _, ok := value["$type"]; !ok {
		value["$type"] = kind.NSID()
	}
	return json.Marshal(value)
}

const MaxSuccessors = 64

func Successor(name, uid string, n int) string {
	switch {
	case n <= 0:
		return name
	case n == 1:
		return SuccessorRkey(name, uid)
	}
	return SuccessorRkey(name, uid) + "-" + strconv.Itoa(n)
}

func InSuccession(name, uid, rkey string) bool {
	base := SuccessorRkey(name, uid)
	if rkey == name || rkey == base {
		return true
	}
	tail, ok := strings.CutPrefix(rkey, base+"-")
	if !ok {
		return false
	}
	n, err := strconv.Atoi(tail)
	return err == nil && n >= 2 && n <= MaxSuccessors && strconv.Itoa(n) == tail
}

func SuccessorRkey(name, uid string) string {
	hex := strings.ReplaceAll(uid, "-", "")
	if len(hex) > 8 {
		hex = hex[:8]
	}
	return name + "-" + hex
}
