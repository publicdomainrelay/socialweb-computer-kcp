package vault

import (
	"context"
	"errors"
)

var ErrNoAuthority = errors.New("vault: the namespace holds no authority yet")

type Cert struct {
	Certificate string `json:"certificate"`

	PrivateKey string `json:"private_key"`

	IssuingCA string `json:"issuing_ca"`

	CAChain []string `json:"ca_chain"`

	Serial string `json:"serial_number"`
}

type RootCA struct {
	Certificate string `json:"certificate"`

	Serial string `json:"serial_number"`
}

type IntermediateCSR struct {
	CSR string `json:"csr"`

	PrivateKey string `json:"private_key"`
}

type Role struct {
	AllowedDomains []string

	AllowSubdomains bool

	AllowBareDomains bool

	EnforceHostnames bool

	KeyType string

	KeyBits int

	MaxTTL string

	NoStore bool
}

type CertRequest struct {
	CommonName string

	AltNames []string

	IPSANs []string

	TTL string
}

type Health struct {
	Initialized bool `json:"initialized"`

	Sealed bool `json:"sealed"`

	Standby bool `json:"standby"`

	Version string `json:"version"`
}

type PKIBackend interface {
	EnsureNamespace(ctx context.Context, path string) error

	EnsureMount(ctx context.Context, namespace, path, kind string) error

	GenerateRoot(ctx context.Context, namespace, mount, commonName, ttl string) (RootCA, error)

	CASerial(ctx context.Context, namespace, mount string) (string, error)

	CAChain(ctx context.Context, namespace, mount string) (string, error)

	GenerateIntermediate(ctx context.Context, namespace, mount, commonName string) (IntermediateCSR, error)

	SignIntermediate(ctx context.Context, rootNamespace, mount, csr, commonName, ttl string) (string, error)

	SetSignedIntermediate(ctx context.Context, namespace, mount, chain string) error

	WriteRole(ctx context.Context, namespace, mount, name string, role Role) error

	Issue(ctx context.Context, namespace, mount, role string, req CertRequest) (Cert, error)

	DeleteNamespace(ctx context.Context, path string) error
}
