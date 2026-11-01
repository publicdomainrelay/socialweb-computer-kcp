package embedded

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketrequester"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/market/requester"
)

const ContractSecretsMount = marketrequester.RequesterVaultClient

var contractName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func ContractSecretsRef(contract string) commonv1.SecretRef {
	return commonv1.SecretRef{Mount: ContractSecretsMount, Path: "contracts/" + contract + "/secrets"}
}

func (r *Requester) PutContractSecrets(ctx context.Context, contract string, bundleJSON []byte) error {
	if r == nil || r.vault == nil {
		return errors.New("embedded: the vault is not running")
	}
	if len(contract) > 253 || !contractName.MatchString(contract) {
		return fmt.Errorf("embedded: %q is not a ComputeContract name", contract)
	}
	entries, err := requester.ParseSecretsBundle(string(bundleJSON))
	if err != nil {
		return fmt.Errorf("embedded: the secrets bundle: %w", err)
	}
	if len(entries) == 0 {
		return errors.New("embedded: the secrets bundle is empty")
	}
	ref := ContractSecretsRef(contract)
	return r.vault.Provider().Store(RootWorkspace).Write(ctx, ref.Mount, ref.Path, map[string]any{"bundle": string(bundleJSON)})
}
