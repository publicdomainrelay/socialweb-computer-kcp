package infra

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
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

var labelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

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

func nonEmpty(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return in
}

func (d Declaration) Hash() string {
	b, _ := json.Marshal(normalize(d))
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (d Declaration) Validate() error {
	seen := map[string]bool{}
	for _, c := range d.Contracts {
		if !labelPattern.MatchString(c.Name) {
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

var recordStates = map[v1alpha1.InfraContractState]string{
	v1alpha1.InfraContractStaged:    "staged",
	v1alpha1.InfraContractRequested: "requested",
	v1alpha1.InfraContractReleased:  "released",
}

func toRecordContract(c v1alpha1.InfraContract) record.InfraContract {
	return record.InfraContract{Name: c.Name, State: recordStates[StateOrStaged(c.State)], VMName: c.VMName, BidderDids: c.BidderDids,
		DenyBidderDids: c.DenyBidderDids, SSHAuthorizedKeys: c.SshAuthorizedKeys, UserData: c.UserData, Policy: c.Policy,
		PolicyArgs: c.PolicyArgs, VMReadyTimeoutSec: int64(c.VMReadyTimeoutSec)}
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
	return v1alpha1.InfraContract{Name: c.Name, State: state, VMName: c.VMName, BidderDids: c.BidderDids, DenyBidderDids: c.DenyBidderDids,
		SshAuthorizedKeys: c.SSHAuthorizedKeys, UserData: c.UserData, Policy: c.Policy, PolicyArgs: c.PolicyArgs,
		VMReadyTimeoutSec: int(c.VMReadyTimeoutSec)}, nil
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
		contracts = append(contracts, toRecordContract(c))
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

type Decoded struct {
	Declaration Declaration

	CreatedAt time.Time
}

func Decode(kind Kind, raw json.RawMessage) (Decoded, error) {
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return Decoded{}, fmt.Errorf("%w: the record is not a JSON object", ErrInvalidDeclaration)
	}
	var d Declaration
	var created string
	var contracts []record.InfraContract
	if kind == KindEphemeralInfra {
		r, err := record.ParseEphemeralInfra(value)
		if err != nil {
			return Decoded{}, err
		}
		d.Description, contracts, created = r.Description, r.Contracts, r.CreatedAt
		switch {
		case r.TtlSeconds == 0:
			d.TTLSeconds = DefaultTTL
		case r.TtlSeconds < 60 || r.TtlSeconds > math.MaxInt32:
			return Decoded{}, fmt.Errorf("%w: ttlSeconds %d is outside 60 to %d", ErrInvalidDeclaration, r.TtlSeconds, math.MaxInt32)
		default:
			d.TTLSeconds = int32(r.TtlSeconds)
		}
		switch strings.TrimSpace(r.EndWhen) {
		case "", "complete":
			d.EndWhen = v1alpha1.EphemeralInfraEndComplete
		case "deadline":
			d.EndWhen = v1alpha1.EphemeralInfraEndDeadline
		default:
			return Decoded{}, fmt.Errorf("%w: endWhen %q", ErrInvalidDeclaration, r.EndWhen)
		}
	} else {
		r, err := record.ParseInfra(value)
		if err != nil {
			return Decoded{}, err
		}
		d.Description, contracts, created = r.Description, r.Contracts, r.CreatedAt
	}
	for _, c := range contracts {
		kc, err := fromRecordContract(c)
		if err != nil {
			return Decoded{}, err
		}
		d.Contracts = append(d.Contracts, kc)
	}
	d = normalize(d)
	if err := d.Validate(); err != nil {
		return Decoded{}, err
	}
	out := Decoded{Declaration: d}
	if t, err := time.Parse(time.RFC3339, created); err == nil {
		out.CreatedAt = t
	}
	return out, nil
}
