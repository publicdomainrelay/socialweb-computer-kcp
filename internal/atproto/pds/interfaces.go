package pds

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/ipfs/go-cid"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/repo"
)

type Mode string

const (
	ModeFull Mode = "full"
	ModeRepo Mode = "repo"
)

var (
	ErrUnsupported        = errors.New("pds: unsupported")
	ErrBlobNotFound       = errors.New("pds: blob not found")
	ErrAccountNotFound    = errors.New("pds: account not found")
	ErrAccountExists      = errors.New("pds: account already exists")
	ErrHandleTaken        = errors.New("pds: handle already taken")
	ErrSessionNotFound    = errors.New("pds: session not found")
	ErrInvalidCredentials = errors.New("pds: invalid credentials")
	ErrAuthentication     = errors.New("pds: authentication required")
	ErrAppPasswordExists  = errors.New("pds: app password already exists")
	ErrAppPasswordMissing = errors.New("pds: app password not found")
)

type DIDDocument struct {
	ID                 string                  `json:"id"`
	AlsoKnownAs        []string                `json:"alsoKnownAs,omitempty"`
	VerificationMethod []DIDVerificationMethod `json:"verificationMethod,omitempty"`
	Service            []DIDService            `json:"service,omitempty"`
}

type DIDVerificationMethod struct {
	ID                 string `json:"id"`
	Type               string `json:"type"`
	Controller         string `json:"controller"`
	PublicKeyMultibase string `json:"publicKeyMultibase,omitempty"`
}

type DIDService struct {
	ID              string `json:"id"`
	Type            string `json:"type"`
	ServiceEndpoint string `json:"serviceEndpoint"`
}

type PLCGenesisRequest struct {
	Handle              string
	Service             string
	SigningKey          string
	RotationKey         string
	VerificationMethods map[string]string
	AlsoKnownAs         []string
}

type PLCGenesisResult struct {
	DID         syntax.DID
	RotationKey string
	Operation   []byte
}

type PLCSubmitter interface {
	SubmitGenesis(ctx context.Context, req PLCGenesisRequest) (PLCGenesisResult, error)
	UpdateHandle(ctx context.Context, did syntax.DID, handle string) error
	UpdateService(ctx context.Context, did syntax.DID, service string) error
}

type DIDResolver interface {
	ResolveDID(ctx context.Context, did syntax.DID) (*DIDDocument, error)
	ResolveHandle(ctx context.Context, handle syntax.Handle) (syntax.DID, error)
}

type DirectoryResolver struct {
	Directory identity.Directory
}

func (d DirectoryResolver) ResolveDID(ctx context.Context, did syntax.DID) (*DIDDocument, error) {
	if d.Directory == nil {
		return nil, ErrUnsupported
	}
	ident, err := d.Directory.LookupDID(ctx, did)
	if err != nil {
		return nil, err
	}
	doc := &DIDDocument{
		ID:          ident.DID.String(),
		AlsoKnownAs: append([]string(nil), ident.AlsoKnownAs...),
	}
	keyIDs := make([]string, 0, len(ident.Keys))
	for id := range ident.Keys {
		keyIDs = append(keyIDs, id)
	}
	sort.Strings(keyIDs)
	for _, id := range keyIDs {
		key := ident.Keys[id]
		doc.VerificationMethod = append(doc.VerificationMethod, DIDVerificationMethod{
			ID:                 ident.DID.String() + "#" + id,
			Type:               key.Type,
			Controller:         ident.DID.String(),
			PublicKeyMultibase: key.PublicKeyMultibase,
		})
	}
	svcIDs := make([]string, 0, len(ident.Services))
	for id := range ident.Services {
		svcIDs = append(svcIDs, id)
	}
	sort.Strings(svcIDs)
	for _, id := range svcIDs {
		svc := ident.Services[id]
		doc.Service = append(doc.Service, DIDService{
			ID:              ident.DID.String() + "#" + id,
			Type:            svc.Type,
			ServiceEndpoint: svc.URL,
		})
	}
	return doc, nil
}

func (d DirectoryResolver) ResolveHandle(ctx context.Context, handle syntax.Handle) (syntax.DID, error) {
	if d.Directory == nil {
		return "", ErrUnsupported
	}
	ident, err := d.Directory.LookupHandle(ctx, handle)
	if err != nil {
		return "", err
	}
	return ident.DID, nil
}

type BlobRef struct {
	CID      cid.Cid
	MimeType string
	Size     int64
}

type BlobStore interface {
	PutBlob(ctx context.Context, did syntax.DID, mimeType string, data []byte) (*BlobRef, error)
	GetBlob(ctx context.Context, did syntax.DID, c cid.Cid) ([]byte, string, error)
}

type RepoKeyStore interface {
	PutRepoKey(ctx context.Context, did syntax.DID, key atcrypto.PrivateKeyExportable) error
	GetRepoKey(ctx context.Context, did syntax.DID) (atcrypto.PrivateKeyExportable, error)
	DeleteRepoKey(ctx context.Context, did syntax.DID) error
}

type OAuthConfig struct {
	Issuer                  string
	Signer                  oauth.Signer
	HTTPClient              *http.Client
	AccessTokenTTL          time.Duration
	RefreshTokenTTL         time.Duration
	AuthorizationCodeTTL    time.Duration
	PARTTL                  time.Duration
	AllowMissingDPoPNonce   bool
	DisableLocalhostClients bool
}

type Config struct {
	Mode          Mode
	Repos         repo.RepoStore
	Store         *Store
	KeyStore      RepoKeyStore
	Resolver      DIDResolver
	Directory     identity.Directory
	PLC           PLCSubmitter
	Blobs         BlobStore
	OAuth         *OAuthConfig
	ServiceDID    string
	PublicBaseURL string
	AlsoKnownAs   []string
	RotationKey   string

	AvailableUserDomains []string
	ContactEmail         string
	Version              string

	SessionSecret        []byte
	AccessTokenTTL       time.Duration
	RefreshTokenTTL      time.Duration
	ServiceAuthAudiences []string
	MaxBlobSize          int64
	MaxRequestBytes      int64
	AllowExplicitDID     bool
}
