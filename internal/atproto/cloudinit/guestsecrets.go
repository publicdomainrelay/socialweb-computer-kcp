package cloudinit

import "encoding/json"

const (
	GuestAgentSecretsPath = GuestAgentConfigDir + "/secrets.json"

	GuestAgentStateDir = "/run/socialweb-guest-agent"

	GuestSecretsReadyPath = GuestAgentStateDir + "/secrets.ready"

	GuestSecretsRequestPath = GuestAgentStateDir + "/secrets.request"

	GuestSecretsUnit = "socialweb-guest-secrets.service"

	SecretsFetchUnit = "setup-secrets.service"

	SecretsDoneStamp = "/var/lib/setup-secrets.done"

	GuestSecretsTag = "secrets"

	GuestSecretsFallbackSealed = "sealed"

	SecretsFetchFallbackDeadlineSec = 90
)

var SecretsReadyUnits = []string{SecretsFetchUnit, GuestSecretsUnit}

type GuestSecretsMarker struct {
	Fallback string `json:"fallback"`

	GetSecrets bool `json:"getSecrets"`
}

func GuestSecretsModule(getSecrets bool) UserDataModule {
	return func(Context) (UserDataPatch, error) {
		raw, err := json.Marshal(GuestSecretsMarker{Fallback: GuestSecretsFallbackSealed, GetSecrets: getSecrets})
		if err != nil {
			return UserDataPatch{}, err
		}
		return UserDataPatch{
			WriteFiles: []WriteFileEntry{{
				Path:        GuestAgentSecretsPath,
				Owner:       "root:root",
				Permissions: "0644",
				Content:     string(raw) + "\n",
			}},
		}, nil
	}
}
