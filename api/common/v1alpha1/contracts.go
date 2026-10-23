package v1alpha1

const (
	ContractLabelPrefix = "contracts.socialweb.computer/"

	ContractVersion = "v1alpha1"
)

const (
	ContractAtprotoRepos = "atprotorepos"

	ContractVaults = "vaults"

	ContractComputeVMs = "computevms"

	ContractEventSources = "eventsources"
)

const (
	ContractLabelAtprotoRepos = ContractLabelPrefix + ContractAtprotoRepos

	ContractLabelVaults = ContractLabelPrefix + ContractVaults

	ContractLabelComputeVMs = ContractLabelPrefix + ContractComputeVMs

	ContractLabelEventSources = ContractLabelPrefix + ContractEventSources
)

const (
	ConditionReady = "Ready"

	ConditionBackendBound = "BackendBound"
)

func ContractLabel(abstractResource string) string {
	return ContractLabelPrefix + abstractResource
}

func ContractLabels(abstractResources ...string) map[string]string {
	if len(abstractResources) == 0 {
		return nil
	}
	out := make(map[string]string, len(abstractResources))
	for _, r := range abstractResources {
		out[ContractLabel(r)] = ContractVersion
	}
	return out
}
