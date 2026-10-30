package cloudinit

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	DefaultAgentEnvFile = "/etc/atirc/atirc.env"

	DefaultAssociationRefFile = "/etc/socialweb-computer/association-ref"

	AgentUnit = "atirc-agent.service"

	AgentRunPath = "/usr/local/bin/atirc-agent-run.sh"

	AtircBinary = "/usr/local/bin/atirc"

	GuestAgentAgentPath = GuestAgentConfigDir + "/agent.json"
)

type AgentOptions struct {
	EnvFile string
}

type guestAgentAgent struct {
	Unit string `json:"unit"`
}

func (opts AgentOptions) envFile() string {
	if opts.EnvFile == "" {
		return DefaultAgentEnvFile
	}
	return opts.EnvFile
}

func (opts AgentOptions) Validate() error {
	if !tunnelPathRE.MatchString(opts.envFile()) {
		return fmt.Errorf("cloudinit agent: envFile %q is not an absolute path of [A-Za-z0-9._/-]", opts.EnvFile)
	}
	return nil
}

func AgentModule(opts AgentOptions) UserDataModule {
	return func(Context) (UserDataPatch, error) {
		return agentPatch(opts)
	}
}

func agentPatch(opts AgentOptions) (UserDataPatch, error) {
	if err := opts.Validate(); err != nil {
		return UserDataPatch{}, err
	}
	raw, err := json.Marshal(guestAgentAgent{Unit: AgentUnit})
	if err != nil {
		return UserDataPatch{}, fmt.Errorf("cloudinit agent: encoding %s: %w", GuestAgentAgentPath, err)
	}
	return UserDataPatch{
		WriteFiles: []WriteFileEntry{
			{Path: "/etc/systemd/system/" + AgentUnit, Owner: "root:root", Permissions: "0644", Content: unitFile(
				[]string{
					"Description=atirc agent",
					"After=network-online.target " + GuestBinariesUnit + " " + strings.Join(SecretsReadyUnits, " "),
					"Wants=network-online.target " + GuestBinariesUnit + " " + GuestSecretsUnit,
					"StartLimitIntervalSec=0",
				},
				[]string{
					"Type=simple",
					"EnvironmentFile=-" + opts.envFile(),
					"ExecStart=" + AgentRunPath,
					"Restart=always",
					"RestartSec=30",
					"StandardOutput=journal",
					"StandardError=journal",
				})},
			{Path: AgentRunPath, Owner: "root:root", Permissions: "0700", Content: agentRunScript},
			{Path: GuestAgentAgentPath, Owner: "root:root", Permissions: "0644", Content: string(raw) + "\n"},
		},
		RuncmdPrepend: []any{"systemctl daemon-reload"},
	}, nil
}

const agentRunScript = `#!/usr/bin/env bash
set -euo pipefail
if [ ! -f "` + GuestSecretsReadyPath + `" ] && [ ! -f "` + SecretsDoneStamp + `" ]; then
  echo "atirc-agent: the secrets bundle is not written yet (` + GuestSecretsReadyPath + `); the agent waits" >&2
  exit 1
fi
if [ ! -x "` + AtircBinary + `" ]; then
  echo "atirc-agent: ` + AtircBinary + ` is not installed yet (` + GuestBinariesUnit + `); the agent waits" >&2
  exit 1
fi
if [ -z "${ATIRC_TOKEN:-}" ]; then
  echo "atirc-agent: ATIRC_TOKEN from the secrets bundle is empty (mint failed or pending); the agent waits" >&2
  exit 1
fi
export ATIRC_TOKEN
nick="$(` + AtircBinary + ` nick)"
if ! [[ "${nick}" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]]; then
  echo "atirc-agent: atirc nick printed ${nick@Q}, not a user name" >&2
  exit 1
fi
getent group wheel >/dev/null || groupadd wheel
mkdir -p /etc/sudoers.d
printf '%%wheel ALL=(ALL) NOPASSWD: ALL\n' > /etc/sudoers.d/wheel
chmod 440 /etc/sudoers.d/wheel
id "${nick}" >/dev/null 2>&1 || useradd -m -G wheel -s /bin/bash "${nick}"
exec runuser -u "${nick}" -- ` + AtircBinary + ` agent run
`
