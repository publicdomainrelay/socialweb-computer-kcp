package pkiprovisioner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
)

const DefaultAuthorityTTL = 5 * time.Minute

var ErrNoClient = errors.New("pkiprovisioner: a client is required")

type Options struct {
	Client vault.PKIBackend

	RootNamespace string

	Mount string

	RootCommonName string

	RootTTL string

	IntermediateTTL string

	LeafTTL string

	Role string

	Domain string

	AuthorityTTL time.Duration

	Now func() time.Time
}

type Provisioner struct {
	opts Options

	mu sync.Mutex

	root *vault.RootCA

	namespaces *ttlcache.Cache[string, vault.Authority]
}

var _ vault.PKIProvisioner = (*Provisioner)(nil)

func New(opts Options) (*Provisioner, error) {
	if opts.Client == nil {
		return nil, ErrNoClient
	}
	if opts.Mount == "" {
		opts.Mount = vault.DefaultPKIMount
	}
	if opts.Role == "" {
		opts.Role = vault.DefaultPKIRole
	}
	if opts.RootCommonName == "" {
		opts.RootCommonName = vault.DefaultRootCommonName
	}
	if opts.RootTTL == "" {
		opts.RootTTL = vault.DefaultRootTTL
	}
	if opts.IntermediateTTL == "" {
		opts.IntermediateTTL = vault.DefaultIntermediateTTL
	}
	if opts.LeafTTL == "" {
		opts.LeafTTL = vault.DefaultLeafTTL
	}
	if opts.AuthorityTTL <= 0 {
		opts.AuthorityTTL = DefaultAuthorityTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Provisioner{opts: opts, namespaces: ttlcache.New[string, vault.Authority](ttlcache.Options{TTL: opts.AuthorityTTL, Now: opts.Now})}, nil
}

func (p *Provisioner) EnsureRoot(ctx context.Context) (vault.RootCA, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ensureRootLocked(ctx)
}

func (p *Provisioner) ensureRootLocked(ctx context.Context) (vault.RootCA, error) {
	if p.root != nil {
		return *p.root, nil
	}
	root, err := p.ensureRoot(ctx)
	if err != nil {
		return vault.RootCA{}, err
	}
	p.root = &root
	return root, nil
}

func (p *Provisioner) ensureRoot(ctx context.Context) (vault.RootCA, error) {
	if err := p.opts.Client.EnsureMount(ctx, p.opts.RootNamespace, p.opts.Mount, "pki"); err != nil {
		return vault.RootCA{}, err
	}
	serial, err := p.opts.Client.CASerial(ctx, p.opts.RootNamespace, p.opts.Mount)
	if err != nil && !errors.Is(err, vault.ErrNoAuthority) {
		return vault.RootCA{}, err
	}
	if serial != "" {
		chain, err := p.opts.Client.CAChain(ctx, p.opts.RootNamespace, p.opts.Mount)
		if err != nil {
			return vault.RootCA{}, err
		}
		return vault.RootCA{Certificate: chain, Serial: serial}, nil
	}
	root, err := p.opts.Client.GenerateRoot(ctx, p.opts.RootNamespace, p.opts.Mount, p.opts.RootCommonName, p.opts.RootTTL)
	if err != nil {
		return vault.RootCA{}, fmt.Errorf("pkiprovisioner: generating the root CA in namespace %q: %w", p.opts.RootNamespace, err)
	}
	return root, nil
}

func (p *Provisioner) EnsureAuthority(ctx context.Context, path string) (vault.Authority, error) {
	if path == "" {
		return vault.Authority{}, errors.New("pkiprovisioner: a namespace path is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if cached, ok := p.namespaces.Get(path); ok {
		return cached, nil
	}
	authority, err := p.ensureAuthority(ctx, path)
	if err != nil {
		return vault.Authority{}, err
	}
	p.namespaces.Set(path, authority)
	return authority, nil
}

func (p *Provisioner) ensureAuthority(ctx context.Context, path string) (vault.Authority, error) {
	if _, err := p.ensureRootLocked(ctx); err != nil {
		return vault.Authority{}, err
	}
	if err := p.opts.Client.EnsureNamespace(ctx, path); err != nil {
		return vault.Authority{}, fmt.Errorf("pkiprovisioner: creating the namespace %q: %w", path, err)
	}
	if err := p.opts.Client.EnsureMount(ctx, path, p.opts.Mount, "pki"); err != nil {
		return vault.Authority{}, fmt.Errorf("pkiprovisioner: mounting %s in namespace %q: %w", p.opts.Mount, path, err)
	}
	commonName := path + ".intermediate"
	serial, err := p.opts.Client.CASerial(ctx, path, p.opts.Mount)
	if err != nil && !errors.Is(err, vault.ErrNoAuthority) {
		return vault.Authority{}, fmt.Errorf("pkiprovisioner: reading the authority of namespace %q: %w", path, err)
	}
	if serial == "" {
		serial, err = p.signIntermediate(ctx, path, commonName)
		if err != nil {
			return vault.Authority{}, err
		}
	}
	chain, err := p.opts.Client.CAChain(ctx, path, p.opts.Mount)
	if err != nil {
		return vault.Authority{}, fmt.Errorf("pkiprovisioner: reading the chain for namespace %q: %w", path, err)
	}
	if err := p.writeRole(ctx, path); err != nil {
		return vault.Authority{}, err
	}
	return vault.Authority{Namespace: path, CommonName: commonName, Serial: serial, Chain: strings.TrimSpace(chain)}, nil
}

func (p *Provisioner) signIntermediate(ctx context.Context, path, commonName string) (string, error) {
	csr, err := p.opts.Client.GenerateIntermediate(ctx, path, p.opts.Mount, commonName)
	if err != nil {
		return "", fmt.Errorf("pkiprovisioner: generating the intermediate for namespace %q: %w", path, err)
	}
	signed, err := p.opts.Client.SignIntermediate(ctx, p.opts.RootNamespace, p.opts.Mount, csr.CSR, commonName, p.opts.IntermediateTTL)
	if err != nil {
		return "", fmt.Errorf("pkiprovisioner: signing the intermediate for namespace %q against the root: %w", path, err)
	}
	if err := p.opts.Client.SetSignedIntermediate(ctx, path, p.opts.Mount, signed); err != nil {
		return "", fmt.Errorf("pkiprovisioner: installing the signed intermediate in namespace %q: %w", path, err)
	}
	serial, err := p.opts.Client.CASerial(ctx, path, p.opts.Mount)
	if err != nil {
		return "", fmt.Errorf("pkiprovisioner: reading the serial of the intermediate in namespace %q: %w", path, err)
	}
	return serial, nil
}

func (p *Provisioner) writeRole(ctx context.Context, path string) error {
	role := vault.Role{
		AllowSubdomains:  true,
		AllowBareDomains: false,
		EnforceHostnames: true,
		KeyType:          "ec",
		KeyBits:          256,
		MaxTTL:           p.opts.LeafTTL,
		NoStore:          true,
	}
	if p.opts.Domain != "" {
		role.AllowedDomains = []string{p.opts.Domain}
	}
	if err := p.opts.Client.WriteRole(ctx, path, p.opts.Mount, p.opts.Role, role); err != nil {
		return fmt.Errorf("pkiprovisioner: writing role %s in namespace %q: %w", p.opts.Role, path, err)
	}
	return nil
}

func (p *Provisioner) Issue(ctx context.Context, path, commonName string, altNames, ips []string) (vault.Cert, error) {
	if _, err := p.EnsureAuthority(ctx, path); err != nil {
		return vault.Cert{}, err
	}
	if len(ips) == 0 {
		ips = []string{"127.0.0.1", "::1"}
	}
	cert, err := p.opts.Client.Issue(ctx, path, p.opts.Mount, p.opts.Role, vault.CertRequest{
		CommonName: commonName,
		AltNames:   altNames,
		IPSANs:     ips,
		TTL:        p.opts.LeafTTL,
	})
	if err != nil {
		return vault.Cert{}, fmt.Errorf("pkiprovisioner: issuing %s in namespace %q: %w", commonName, path, err)
	}
	return cert, nil
}

func (p *Provisioner) Delete(ctx context.Context, path string) error {
	if path == "" {
		return errors.New("pkiprovisioner: a namespace path is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.namespaces.Delete(path)
	if err := p.opts.Client.DeleteNamespace(ctx, path); err != nil {
		return fmt.Errorf("pkiprovisioner: deleting the namespace %q: %w", path, err)
	}
	return nil
}

func (p *Provisioner) CachedRootPEM() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.root == nil {
		return nil
	}
	return []byte(p.root.Certificate)
}

func LeafChain(cert vault.Cert) string {
	var builder strings.Builder
	builder.WriteString(cert.Certificate)
	for _, entry := range cert.CAChain {
		builder.WriteString("\n")
		builder.WriteString(entry)
	}
	return builder.String()
}
