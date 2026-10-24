package guestidentity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

const MaxVMHostKeys = 8

var (
	guestNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

	hostKeyTypeRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.@-]*$`)

	hostKeyBlobRE = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)

	networkTagRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

	dtlsFingerprintRE = regexp.MustCompile(`^sha-256 [0-9A-F]{2}(:[0-9A-F]{2}){31}$`)
)

var (
	ErrUnknownGuest = errors.New("computevm: no live ComputeVM holds this guest")

	ErrUnknownAccept = errors.New("computevm: the accept is not this guest's")

	ErrReportRefused = errors.New("computevm: the guest report was refused")
)

type VMStore interface {
	Get(ctx context.Context, name string) (*v1alpha1.ComputeVM, error)

	RecordGuest(ctx context.Context, name string, report v1alpha1.ComputeVMGuestReport) error
}

func GuestReportOf(o *v1alpha1.ComputeVM) *v1alpha1.ComputeVMGuestReport {
	if o == nil || o.Status.Market == nil {
		return nil
	}
	return o.Status.Market.Guest
}

func Ended(o *v1alpha1.ComputeVM) bool {
	if o == nil {
		return false
	}
	return o.Status.Phase == v1alpha1.ComputeVMPhaseLost || o.Status.Phase == v1alpha1.ComputeVMPhaseFailed
}

func Releasing(o *v1alpha1.ComputeVM) bool {
	if o == nil {
		return false
	}
	return o.DeletionTimestamp != nil || o.Status.Phase == v1alpha1.ComputeVMPhaseReleasing
}

type guestRecord struct {
	store VMStore

	log *slog.Logger

	now func() time.Time
}

func (g *guestRecord) Live(ctx context.Context, name string) (*v1alpha1.ComputeVM, error) {
	if g == nil || g.store == nil {
		return nil, errors.New("computevm: the guest recorder has no ComputeVM store")
	}
	if len(name) > 201 || !guestNameRE.MatchString(name) {
		return nil, ErrUnknownGuest
	}
	o, err := g.store.Get(ctx, name)
	if apierrors.IsNotFound(err) {
		return nil, ErrUnknownGuest
	}
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, ErrUnknownGuest
	}
	if Ended(o) || Releasing(o) {
		return nil, ErrUnknownGuest
	}
	return o, nil
}

func (g *guestRecord) OnNetwork(ctx context.Context, name string, report OnNetworkReport) error {
	acceptURI, acceptCID := strings.TrimSpace(report.AcceptURI), strings.TrimSpace(report.AcceptCID)
	address, tag := strings.TrimSpace(report.Address), strings.TrimSpace(report.Tag)
	createdAt := report.CreatedAt
	switch {
	case acceptURI == "" || acceptCID == "":
		return fmt.Errorf("%w: the report names no accept", ErrReportRefused)
	case len(acceptURI) > 2048 || len(acceptCID) > 128:
		return fmt.Errorf("%w: the accept ref is too long", ErrReportRefused)
	case address == "" || len(address) > 4096:
		return fmt.Errorf("%w: the report names no address", ErrReportRefused)
	case len(createdAt) > 64:
		return fmt.Errorf("%w: the report's createdAt is too long", ErrReportRefused)
	case tag != "" && !networkTagRE.MatchString(tag):
		return fmt.Errorf("%w: the tag %q is not a DNS label of at most 32 characters", ErrReportRefused, tag)
	}
	if err := checkNetworkKind(strings.TrimSpace(report.Kind), tag, address); err != nil {
		return err
	}
	o, err := g.Live(ctx, name)
	if err != nil {
		return err
	}
	if c := o.Spec.Contract; c != nil && c.Accept.URI != "" && (c.Accept.URI != acceptURI || c.Accept.CID != acceptCID) {
		return ErrUnknownAccept
	}
	held := GuestReportOf(o)
	reportedAt := strings.TrimSpace(createdAt)
	if reportedAt == "" {
		reportedAt = g.now().UTC().Format(time.RFC3339)
	}
	next := v1alpha1.ComputeVMGuestReport{}
	if held != nil {
		next = *held.DeepCopy()
	}
	if tag == "" {
		if held != nil && held.Address == address && held.ReportedAt != "" {
			return nil
		}
		next.Address, next.ReportedAt = address, reportedAt
	} else {
		i := slices.IndexFunc(next.Networks, func(n v1alpha1.ComputeVMGuestNetwork) bool { return n.Tag == tag })
		switch {
		case i >= 0 && next.Networks[i].Address == address:
			return nil
		case i >= 0:
			next.Networks[i] = v1alpha1.ComputeVMGuestNetwork{Tag: tag, Address: address, ReportedAt: reportedAt}
		default:
			next.Networks = append(next.Networks, v1alpha1.ComputeVMGuestNetwork{Tag: tag, Address: address, ReportedAt: reportedAt})
		}
	}
	if err := g.store.RecordGuest(ctx, name, next); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrUnknownGuest
		}
		return err
	}
	g.log.Info("the guest's on-network address is recorded", "computevm", name, "tag", tag, "address", address, "reportedAt", reportedAt)
	return nil
}

func checkNetworkKind(kind, tag, address string) error {
	if kind == "" {
		kind = market.OnNetworkKindIroh
		if tag != "" {
			kind = market.OnNetworkKindHTTPS
		}
	}
	switch kind {
	case market.OnNetworkKindIroh:
		if tag != "" {
			return fmt.Errorf("%w: an iroh address carries no tag", ErrReportRefused)
		}
	case market.OnNetworkKindHTTPS:
		if tag == "" || len(address) > 2048 || !strings.HasPrefix(address, "https://") {
			return fmt.Errorf("%w: an https network is tagged and its address is an https URL of at most 2048 bytes", ErrReportRefused)
		}
	case market.OnNetworkKindWebRTC:
		if tag == "" || !dtlsFingerprintRE.MatchString(address) {
			return fmt.Errorf("%w: a webrtc network is tagged and its address is a DTLS fingerprint \"sha-256 AB:CD:...\"", ErrReportRefused)
		}
	default:
		return fmt.Errorf("%w: the network kind %q is not iroh, https or webrtc", ErrReportRefused, kind)
	}
	return nil
}

func NetworkKind(tag, address string) string {
	switch {
	case tag == "":
		return market.OnNetworkKindIroh
	case dtlsFingerprintRE.MatchString(address):
		return market.OnNetworkKindWebRTC
	default:
		return market.OnNetworkKindHTTPS
	}
}

func (g *guestRecord) Accept(ctx context.Context, name, acceptURI, acceptCID string) error {
	o, err := g.Live(ctx, name)
	if err != nil {
		return err
	}
	c := o.Spec.Contract
	if c == nil || c.Accept.URI == "" || c.Accept.URI != acceptURI || c.Accept.CID != acceptCID {
		return ErrUnknownAccept
	}
	if c.Receipt.URI == "" || c.Receipt.CID == "" {
		return fmt.Errorf("computevm: the ComputeVM %s holds no receipt yet", name)
	}
	return nil
}

func (g *guestRecord) HostKeys(ctx context.Context, name string, keys []v1alpha1.ComputeVMHostKey) error {
	o, err := g.Live(ctx, name)
	if err != nil {
		return err
	}
	checked, err := CheckHostKeys(keys)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrReportRefused, err)
	}
	if err := RequireEd25519(checked); err != nil {
		return fmt.Errorf("%w: %v", ErrReportRefused, err)
	}
	held := GuestReportOf(o)
	if held != nil && len(held.HostKeys) > 0 {
		if SameHostKeys(held.HostKeys, checked) {
			return nil
		}
		return fmt.Errorf("%w: host keys are already reported for this guest", ErrReportRefused)
	}
	next := v1alpha1.ComputeVMGuestReport{HostKeys: checked}
	if held != nil {
		next.Address, next.ReportedAt = held.Address, held.ReportedAt
		next.Networks = append([]v1alpha1.ComputeVMGuestNetwork(nil), held.Networks...)
	}
	if err := g.store.RecordGuest(ctx, name, next); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrUnknownGuest
		}
		return err
	}
	g.log.Info("the guest's bound host keys are recorded", "computevm", name, "types", keyTypes(checked))
	return nil
}

func ParseHostKey(line string) (v1alpha1.ComputeVMHostKey, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return v1alpha1.ComputeVMHostKey{}, errors.New("computevm: the host key is not an authorized_keys line")
	}
	keys, err := CheckHostKeys([]v1alpha1.ComputeVMHostKey{{Type: fields[0], PublicKey: fields[1]}})
	if err != nil {
		return v1alpha1.ComputeVMHostKey{}, err
	}
	return keys[0], nil
}

func CheckHostKeys(keys []v1alpha1.ComputeVMHostKey) ([]v1alpha1.ComputeVMHostKey, error) {
	if len(keys) == 0 {
		return nil, errors.New("computevm: no host keys")
	}
	if len(keys) > MaxVMHostKeys {
		return nil, fmt.Errorf("computevm: more than %d host keys", MaxVMHostKeys)
	}
	out := make([]v1alpha1.ComputeVMHostKey, 0, len(keys))
	types := map[string]bool{}
	for _, key := range keys {
		if len(key.Type) > 64 || !hostKeyTypeRE.MatchString(key.Type) {
			return nil, fmt.Errorf("computevm: host key type %q is malformed", key.Type)
		}
		if len(key.PublicKey) < 16 || len(key.PublicKey) > 4096 || !hostKeyBlobRE.MatchString(key.PublicKey) {
			return nil, fmt.Errorf("computevm: the %s public key is not a base64 key blob", key.Type)
		}
		blobType, err := keyBlobType(key.PublicKey)
		if err != nil {
			return nil, err
		}
		if blobType != key.Type {
			return nil, fmt.Errorf("computevm: the %s key blob names type %s", key.Type, blobType)
		}
		if types[key.Type] {
			return nil, fmt.Errorf("computevm: %s is reported twice", key.Type)
		}
		types[key.Type] = true
		fingerprint, err := keyFingerprint(key.PublicKey)
		if err != nil {
			return nil, err
		}
		if key.Fingerprint != "" && key.Fingerprint != fingerprint {
			return nil, fmt.Errorf("computevm: the %s fingerprint does not match its key", key.Type)
		}
		out = append(out, v1alpha1.ComputeVMHostKey{Type: key.Type, PublicKey: key.PublicKey, Fingerprint: fingerprint})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out, nil
}

func RequireEd25519(keys []v1alpha1.ComputeVMHostKey) error {
	for _, key := range keys {
		if key.Type == "ssh-ed25519" {
			return nil
		}
	}
	return errors.New("computevm: the report carries no ssh-ed25519 key")
}

func SameHostKeys(a, b []v1alpha1.ComputeVMHostKey) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type || a[i].PublicKey != b[i].PublicKey {
			return false
		}
	}
	return true
}

func keyFingerprint(publicKey string) (string, error) {
	blob, err := decodeKeyBlob(publicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), nil
}

func keyBlobType(publicKey string) (string, error) {
	blob, err := decodeKeyBlob(publicKey)
	if err != nil {
		return "", err
	}
	if len(blob) < 4 {
		return "", errors.New("computevm: the key blob is shorter than its type")
	}
	length := int(binary.BigEndian.Uint32(blob[:4]))
	if length == 0 || length > 64 || len(blob) < 4+length {
		return "", errors.New("computevm: the key blob has no length-prefixed type")
	}
	return string(blob[4 : 4+length]), nil
}

func decodeKeyBlob(publicKey string) ([]byte, error) {
	blob, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil {
		blob, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(publicKey, "="))
	}
	if err != nil {
		return nil, errors.New("computevm: the public key is not base64")
	}
	return blob, nil
}

func keyTypes(keys []v1alpha1.ComputeVMHostKey) []string {
	out := make([]string, len(keys))
	for i := range keys {
		out[i] = keys[i].Type
	}
	return out
}

const (
	MaxGuestEventTag = 32

	MaxGuestKeyKind = 32

	MaxGuestKey = 256

	MaxGuestSignalKind = 16

	MaxGuestCiphertext = 16384

	MaxGuestEventBytes = 24 << 10

	MaxGuestSecretsEventBytes = market.MaxGuestSecretsCiphertext + 4<<10

	MaxGuestSecretsVia = 16

	ageArmorHeader = "-----BEGIN AGE ENCRYPTED FILE-----"
)

func CheckGuestEvent(record map[string]any) (string, error) {
	nsid, _ := record["$type"].(string)
	raw, err := json.Marshal(record)
	limit := MaxGuestEventBytes
	if nsid == market.ComputeEventsGuestSecretsNSID {
		limit = MaxGuestSecretsEventBytes
	}
	switch {
	case err != nil:
		return "", fmt.Errorf("%w: the guest event is not JSON: %v", ErrReportRefused, err)
	case len(raw) > limit:
		return "", fmt.Errorf("%w: the guest event is larger than %d bytes", ErrReportRefused, limit)
	}
	switch nsid {
	case market.ComputeEventsGuestPublicKeyNSID:
		r, err := market.ParseComputeEventsGuestPublicKey(record)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrReportRefused, err)
		}
		if err := checkGuestEventFields(r.Tag, r.Kind, MaxGuestKeyKind, r.CreatedAt); err != nil {
			return "", err
		}
		if r.Key == "" || len(r.Key) > MaxGuestKey || !printableASCII(r.Key) {
			return "", fmt.Errorf("%w: key is one printable token of at most %d bytes", ErrReportRefused, MaxGuestKey)
		}
	case market.ComputeEventsGuestSignalWebRTCNSID:
		r, err := market.ParseComputeEventsGuestSignalWebRTC(record)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrReportRefused, err)
		}
		if err := checkGuestEventFields(r.Tag, r.Kind, MaxGuestSignalKind, r.CreatedAt); err != nil {
			return "", err
		}
		if len(r.Ciphertext) > MaxGuestCiphertext || !strings.HasPrefix(strings.TrimSpace(r.Ciphertext), ageArmorHeader) {
			return "", fmt.Errorf("%w: ciphertext is age armored ciphertext of at most %d bytes", ErrReportRefused, MaxGuestCiphertext)
		}
	case market.ComputeEventsGuestSecretsNSID:
		r, err := market.ParseComputeEventsGuestSecrets(record)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrReportRefused, err)
		}
		if _, err := syntax.ParseDatetime(r.CreatedAt); err != nil {
			return "", fmt.Errorf("%w: createdAt is not an atproto datetime", ErrReportRefused)
		}
		if len(r.Ciphertext) > market.MaxGuestSecretsCiphertext || !strings.HasPrefix(strings.TrimSpace(r.Ciphertext), ageArmorHeader) {
			return "", fmt.Errorf("%w: ciphertext is age armored ciphertext of at most %d bytes", ErrReportRefused, market.MaxGuestSecretsCiphertext)
		}
	case market.ComputeEventsGuestSecretsRequestedNSID:
		r, err := market.ParseComputeEventsGuestSecretsRequested(record)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrReportRefused, err)
		}
		if _, err := syntax.ParseDatetime(r.CreatedAt); err != nil {
			return "", fmt.Errorf("%w: createdAt is not an atproto datetime", ErrReportRefused)
		}
	case market.ComputeEventsGuestSecretsAppliedNSID:
		r, err := market.ParseComputeEventsGuestSecretsApplied(record)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrReportRefused, err)
		}
		if _, err := syntax.ParseDatetime(r.CreatedAt); err != nil {
			return "", fmt.Errorf("%w: createdAt is not an atproto datetime", ErrReportRefused)
		}
		if r.Via == "" || len(r.Via) > MaxGuestSecretsVia || !printableASCII(r.Via) || r.Count < 0 {
			return "", fmt.Errorf("%w: via is printable ASCII of 1 to %d bytes and count is not negative", ErrReportRefused, MaxGuestSecretsVia)
		}
	default:
		return "", fmt.Errorf("%w: %q is not a guest event", ErrReportRefused, nsid)
	}
	return nsid, nil
}

func checkGuestEventFields(tag, kind string, maxKind int, createdAt string) error {
	switch {
	case tag == "" || len(tag) > MaxGuestEventTag || !printableASCII(tag):
		return fmt.Errorf("%w: tag is printable ASCII of 1 to %d bytes", ErrReportRefused, MaxGuestEventTag)
	case kind == "" || len(kind) > maxKind || !printableASCII(kind):
		return fmt.Errorf("%w: kind is printable ASCII of 1 to %d bytes", ErrReportRefused, maxKind)
	}
	if _, err := syntax.ParseDatetime(createdAt); err != nil {
		return fmt.Errorf("%w: createdAt is not an atproto datetime", ErrReportRefused)
	}
	return nil
}

func printableASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}
