package requester

import (
	"errors"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
)

var errAgentWithoutSecrets = errors.New("an agent needs its token in spec.secretsRef: the secrets bundle writes the agent's envFile on the guest")

func agentEnvFile(contract v1alpha1.ComputeContract) (string, error) {
	if contract.Spec.Agent == nil {
		return "", nil
	}
	if contract.Spec.SecretsRef == nil {
		return "", errAgentWithoutSecrets
	}
	if envFile := strings.TrimSpace(contract.Spec.Agent.EnvFile); envFile != "" {
		return envFile, nil
	}
	return cloudinit.DefaultAgentEnvFile, nil
}
