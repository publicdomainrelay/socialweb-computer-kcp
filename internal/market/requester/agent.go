package requester

import (
	"cmp"
	"errors"
	"fmt"
	"path"
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

var ErrTokenPathConflict = errors.New("requester: a path names an atirc token file")

func CheckTokenPaths(contract v1alpha1.ComputeContract, bundlePaths []string) error {
	tokens := map[string]string{cloudinit.GuestRequesterAtircEnvFile: "the guest requester's atirc token"}
	if agent := contract.Spec.Agent; agent != nil {
		envFile := path.Clean(cmp.Or(strings.TrimSpace(agent.EnvFile), cloudinit.DefaultAgentEnvFile))
		if owner, ok := tokens[envFile]; ok {
			return fmt.Errorf("%w: the agent envFile %s is %s", ErrTokenPathConflict, envFile, owner)
		}
		tokens[envFile] = "the agent's atirc token"
	}
	check := func(what, p string) error {
		if owner, ok := tokens[path.Clean(p)]; ok {
			return fmt.Errorf("%w: %s %s is %s", ErrTokenPathConflict, what, p, owner)
		}
		return nil
	}
	if contract.Spec.Association != nil {
		if refFile, err := associationRefFile(contract); err == nil {
			if err := check("spec.association.refFile", refFile); err != nil {
				return err
			}
		}
		if err := check("the association request", cloudinit.DefaultAssociationRequestFile); err != nil {
			return err
		}
	}
	for _, p := range bundlePaths {
		if err := check("the secrets bundle path", p); err != nil {
			return err
		}
	}
	return nil
}

func tokenFile(contract v1alpha1.ComputeContract, p string) bool {
	p = path.Clean(p)
	if p == cloudinit.GuestRequesterAtircEnvFile {
		return true
	}
	agent := contract.Spec.Agent
	return agent != nil && path.Clean(cmp.Or(strings.TrimSpace(agent.EnvFile), cloudinit.DefaultAgentEnvFile)) == p
}
