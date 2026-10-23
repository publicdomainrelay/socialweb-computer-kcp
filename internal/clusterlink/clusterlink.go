package clusterlink

import (
	"context"
	"crypto/ed25519"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/didkey"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const Mount = "socialweb-links"

const EnvSecret = "IROH_SECRET"

const (
	FieldSecret = "secret"

	FieldEndpointID = "endpointId"

	FieldDid = "did"
)

const TicketPrefix = "endpoint"

const connectLine = "dumbpipe connect-tcp "

const ticketVariant = 0x00

var Services = []v1alpha1.ClusterLinkService{v1alpha1.ClusterLinkApiserver, v1alpha1.ClusterLinkOpenBao}

var ticketEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

var (
	ErrStoredKeyUnreadable = errors.New(
		"clusterlink: the link key stored in the cluster vault cannot be read back. It is never " +
			"regenerated over: a new key is a new endpoint id, and every node holding a ticket for " +
			"the old one would dial an endpoint nobody listens as")

	ErrBadTicket = errors.New("clusterlink: not a dumbpipe endpoint ticket")
)

type KeyStore interface {
	Read(ctx context.Context, path string) (map[string]any, bool, error)

	Create(ctx context.Context, path string, data map[string]any) (bool, error)
}

type Key struct {
	Service v1alpha1.ClusterLinkService

	Private ed25519.PrivateKey

	EndpointID string

	Did string
}

func (k Key) Env() string { return EnvSecret + "=" + hex.EncodeToString(k.Private.Seed()) }

func (k Key) record() map[string]any {
	return map[string]any{
		FieldSecret:     hex.EncodeToString(k.Private.Seed()),
		FieldEndpointID: k.EndpointID,
		FieldDid:        k.Did,
	}
}

func KeyFromSeed(service v1alpha1.ClusterLinkService, seed []byte) (Key, error) {
	if len(seed) != ed25519.SeedSize {
		return Key{}, fmt.Errorf("clusterlink: an iroh secret key is a %d byte ed25519 seed, this one is %d",
			ed25519.SeedSize, len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	id, err := didkey.EndpointID(pub)
	if err != nil {
		return Key{}, err
	}
	did, err := didkey.FromEd25519(pub)
	if err != nil {
		return Key{}, err
	}
	return Key{Service: service, Private: priv, EndpointID: id, Did: did}, nil
}

func keyFromRecord(service v1alpha1.ClusterLinkService, data map[string]any) (Key, error) {
	raw, _ := data[FieldSecret].(string)
	seed, err := hex.DecodeString(raw)
	if err != nil || len(seed) != ed25519.SeedSize {
		return Key{}, fmt.Errorf("%w: %s/%s holds no %d byte hex %q", ErrStoredKeyUnreadable, Mount, service,
			ed25519.SeedSize, FieldSecret)
	}
	k, err := KeyFromSeed(service, seed)
	if err != nil {
		return Key{}, err
	}
	if id, _ := data[FieldEndpointID].(string); id != "" && id != k.EndpointID {
		return Key{}, fmt.Errorf("%w: %s/%s names endpoint %s and its secret derives %s",
			ErrStoredKeyUnreadable, Mount, service, id, k.EndpointID)
	}
	return k, nil
}

func EnsureKey(ctx context.Context, store KeyStore, service v1alpha1.ClusterLinkService, random io.Reader) (Key, error) {
	path := string(service)
	data, found, err := store.Read(ctx, path)
	if err != nil {
		return Key{}, fmt.Errorf("clusterlink: reading %s/%s: %w", Mount, path, err)
	}
	if found {
		return keyFromRecord(service, data)
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := io.ReadFull(random, seed); err != nil {
		return Key{}, fmt.Errorf("clusterlink: generating the %s link key: %w", service, err)
	}
	k, err := KeyFromSeed(service, seed)
	if err != nil {
		return Key{}, err
	}
	created, err := store.Create(ctx, path, k.record())
	if err != nil {
		return Key{}, fmt.Errorf("clusterlink: storing the %s link key at %s/%s: %w", service, Mount, path, err)
	}
	if created {
		return k, nil
	}
	data, found, err = store.Read(ctx, path)
	if err != nil {
		return Key{}, fmt.Errorf("clusterlink: reading %s/%s after another writer stored it first: %w", Mount, path, err)
	}
	if !found {
		return Key{}, fmt.Errorf("clusterlink: %s/%s refused a first write as already present and then read as absent",
			Mount, path)
	}
	return keyFromRecord(service, data)
}

func ParseTicket(ticket string) (string, error) {
	body, ok := strings.CutPrefix(ticket, TicketPrefix)
	if !ok {
		return "", fmt.Errorf("%w: %q has no %q prefix", ErrBadTicket, ticket, TicketPrefix)
	}
	raw, err := ticketEncoding.DecodeString(strings.ToUpper(body))
	if err != nil {
		return "", fmt.Errorf("%w: %q is not base32: %v", ErrBadTicket, ticket, err)
	}
	if len(raw) < 1+ed25519.PublicKeySize || raw[0] != ticketVariant {
		return "", fmt.Errorf("%w: %q carries no version-0 endpoint address", ErrBadTicket, ticket)
	}
	return didkey.EndpointID(ed25519.PublicKey(raw[1 : 1+ed25519.PublicKeySize]))
}

func StableTicket(k Key) string {
	pub := k.Private.Public().(ed25519.PublicKey)
	body := append(append([]byte{ticketVariant}, pub...), 0x00)
	return TicketPrefix + strings.ToLower(ticketEncoding.EncodeToString(body))
}

func TicketIn(output string) (string, bool) {
	found := ""
	for line := range strings.SplitSeq(output, "\n") {
		_, rest, ok := strings.Cut(line, connectLine)
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 || !strings.HasPrefix(fields[len(fields)-1], TicketPrefix) {
			continue
		}
		found = fields[len(fields)-1]
	}
	return found, found != ""
}

func Argv(binary string, targetPort int) []string {
	return []string{
		binary, "listen-tcp",
		"--host", "127.0.0.1:" + strconv.Itoa(targetPort),
		"--ipv4-addr", "0.0.0.0:0",
	}
}

func Name(clusterKey string, service v1alpha1.ClusterLinkService) string {
	return clusterKey + "-" + string(service)
}

func Object(clusterKey string, service v1alpha1.ClusterLinkService, targetPort int, status v1alpha1.ClusterLinkStatus) v1alpha1.ClusterLink {
	return v1alpha1.ClusterLink{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ClusterLink"},
		ObjectMeta: metav1.ObjectMeta{
			Name: Name(clusterKey, service),
		},
		Spec: v1alpha1.ClusterLinkSpec{
			ClusterRef: commonv1.LocalObjectRef{Name: clusterKey},
			Service:    service,
			TargetPort: targetPort,
		},
		Status: status,
	}
}

func Listening(k Key, printed string) (v1alpha1.ClusterLinkStatus, error) {
	id, err := ParseTicket(printed)
	if err != nil {
		return v1alpha1.ClusterLinkStatus{}, err
	}
	if id != k.EndpointID {
		return v1alpha1.ClusterLinkStatus{}, fmt.Errorf(
			"clusterlink: the %s link printed a ticket for endpoint %s and its key is %s: the "+
				"listener is not running as the key the vault holds", k.Service, id, k.EndpointID)
	}
	return v1alpha1.ClusterLinkStatus{
		Phase:      v1alpha1.ClusterLinkListening,
		EndpointID: k.EndpointID,
		Did:        k.Did,
		Ticket:     StableTicket(k),
	}, nil
}

func Failed(k Key, message string) v1alpha1.ClusterLinkStatus {
	return v1alpha1.ClusterLinkStatus{
		Phase:      v1alpha1.ClusterLinkFailed,
		EndpointID: k.EndpointID,
		Did:        k.Did,
		Message:    message,
	}
}
