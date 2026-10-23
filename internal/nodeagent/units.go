package nodeagent

import (
	"context"
	"log/slog"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/systemdunit"
)

const (
	UnitBootstrap = "socialweb-node-bootstrap.service"
	UnitPrep      = "socialweb-node-prep.service"

	UnitK3sAgent = "socialweb-k3s-agent.service"

	UnitDNSLink = "socialweb-node-dns-link.service"

	UnitMasquerade = "socialweb-pod-masquerade.service"

	UnitCNI = "socialweb-pod-cni.service"

	UnitDiagnose = "socialweb-node-diagnose.service"

	UnitMeshdInstall = "socialweb-meshd-install.service"

	UnitMeshd = "socialweb-meshd.service"
)

func Units(cfg Config) []systemdunit.Unit {
	prep := systemdunit.Unit{Name: UnitPrep, Sections: []systemdunit.Section{
		{Name: "Unit", Entries: []systemdunit.KeyValue{
			systemdunit.KV("Description", "socialweb node: fetch the join secrets and supervise the cluster links"),
			systemdunit.KV("After", "network-online.target setup-secrets.service"),
			systemdunit.KV("Wants", "network-online.target"),
			systemdunit.KV("StartLimitIntervalSec", "0"),
		}},
		{Name: "Service", Entries: []systemdunit.KeyValue{
			systemdunit.KV("Type", "notify"),
			systemdunit.KV("ExecStart", cfg.NodeBinaryPath+" join --config "+cfg.ConfigPath),
			systemdunit.KV("Restart", "always"),
			systemdunit.KV("RestartSec", "5"),
			systemdunit.KV("NotifyAccess", "main"),
			systemdunit.KV("TimeoutStartSec", "infinity"),
			systemdunit.KV("StandardOutput", "journal+console"),
			systemdunit.KV("StandardError", "journal+console"),
		}},
		{Name: "Install", Entries: []systemdunit.KeyValue{systemdunit.KV("WantedBy", "multi-user.target")}},
	}}
	k3sAgent := systemdunit.Unit{Name: UnitK3sAgent, Sections: []systemdunit.Section{
		{Name: "Unit", Entries: []systemdunit.KeyValue{
			systemdunit.KV("Description", "socialweb node: k3s agent joined through the apiserver link"),
			systemdunit.KV("After", "network-online.target "+UnitPrep+" "+UnitDNSLink+" "+UnitMasquerade),
			systemdunit.KV("Wants", "network-online.target "+UnitPrep+" "+UnitDNSLink+" "+UnitMasquerade),
			systemdunit.KV("StartLimitIntervalSec", "0"),
		}},
		{Name: "Service", Entries: []systemdunit.KeyValue{
			systemdunit.KV("Type", "notify"),
			systemdunit.KV("EnvironmentFile", cfg.APIServerEnvPath),
			systemdunit.KV("KillMode", "process"),
			systemdunit.KV("Delegate", "yes"),
			systemdunit.KV("LimitNOFILE", "1048576"),
			systemdunit.KV("LimitNPROC", "infinity"),
			systemdunit.KV("LimitCORE", "infinity"),
			systemdunit.KV("TasksMax", "infinity"),
			systemdunit.KV("TimeoutStartSec", "0"),
			systemdunit.KV("ExecStartPre", "-/sbin/modprobe br_netfilter"),
			systemdunit.KV("ExecStartPre", "-/sbin/modprobe overlay"),
			systemdunit.KV("ExecStartPre", "/usr/bin/test -x "+cfg.K3sPath),
			systemdunit.KV("ExecStartPre", "/usr/bin/test -s "+cfg.TokenPath),
			systemdunit.KV("ExecStartPre", cfg.NodeBinaryPath+" hostname --config "+cfg.ConfigPath),
			systemdunit.KV("ExecStart", cfg.K3sPath+" agent --server https://${ADVERTISE_ADDRESS}:${APISERVER_PORT} --token-file "+
				cfg.TokenPath+" --node-name ${NODE_NAME} --data-dir "+cfg.K3sDataDir+
				" --kubelet-arg=cluster-dns="+NodeLocalDNSAddress+" --node-label=socialweb.computer/node-local-dns=true"),
			systemdunit.KV("Restart", "always"),
			systemdunit.KV("RestartSec", "5"),
			systemdunit.KV("StandardOutput", "journal+console"),
			systemdunit.KV("StandardError", "journal+console"),
		}},
		{Name: "Install", Entries: []systemdunit.KeyValue{systemdunit.KV("WantedBy", "multi-user.target")}},
	}}
	dnsLink := oneshotUnit(UnitDNSLink, "socialweb node: the per-node DNS address on a dummy interface",
		nil, nil, cfg.NodeBinaryPath+" dns-link --config "+cfg.ConfigPath, true, true)
	masquerade := oneshotUnit(UnitMasquerade, "socialweb node: forward pod traffic and SNAT only what leaves the cluster CIDR",
		nil, nil, cfg.NodeBinaryPath+" masquerade --config "+cfg.ConfigPath, true, true)
	cni := systemdunit.Unit{Name: UnitCNI, Sections: []systemdunit.Section{
		{Name: "Unit", Entries: []systemdunit.KeyValue{
			systemdunit.KV("Description", "socialweb node: bridge CNI on the Node's podCIDR, once the Node has registered"),
			systemdunit.KV("After", UnitPrep),
			systemdunit.KV("Wants", UnitPrep),
			systemdunit.KV("StartLimitIntervalSec", "0"),
		}},
		{Name: "Service", Entries: []systemdunit.KeyValue{
			systemdunit.KV("Type", "oneshot"),
			systemdunit.KV("RemainAfterExit", "yes"),
			systemdunit.KV("EnvironmentFile", cfg.APIServerEnvPath),
			systemdunit.KV("ExecStart", cfg.NodeBinaryPath+" cni --config "+cfg.ConfigPath),
			systemdunit.KV("Restart", "on-failure"),
			systemdunit.KV("RestartSec", "5"),
			systemdunit.KV("TimeoutStartSec", "infinity"),
			systemdunit.KV("StandardOutput", "journal+console"),
			systemdunit.KV("StandardError", "journal+console"),
		}},
		{Name: "Install", Entries: []systemdunit.KeyValue{systemdunit.KV("WantedBy", "multi-user.target")}},
	}}
	diagnose := oneshotUnit(UnitDiagnose, "socialweb node: print containerd's CNI status to the console until the node network is ready",
		nil, nil, cfg.NodeBinaryPath+" diagnose --config "+cfg.ConfigPath, false, true)
	out := []systemdunit.Unit{prep, k3sAgent, dnsLink, masquerade, cni, diagnose}
	if cfg.Meshd != nil {
		install := oneshotUnit(UnitMeshdInstall, "socialweb node: install the sha256-pinned socialweb-meshd",
			[]string{"network-online.target"}, []string{"network-online.target"},
			cfg.NodeBinaryPath+" install-meshd --config "+cfg.ConfigPath, true, true)
		mesh := systemdunit.Unit{Name: UnitMeshd, Sections: []systemdunit.Section{
			{Name: "Unit", Entries: []systemdunit.KeyValue{
				systemdunit.KV("Description", "socialweb.computer pod mesh over iroh (socialweb-meshd)"),
				systemdunit.KV("Wants", "network-online.target"),
				systemdunit.KV("After", "network-online.target"),
			}},
			{Name: "Service", Entries: []systemdunit.KeyValue{
				systemdunit.KV("Type", "notify"),
				systemdunit.KV("NotifyAccess", "main"),
				systemdunit.KV("TimeoutStartSec", "infinity"),
				systemdunit.KV("EnvironmentFile", cfg.MeshdEnvPath),
				systemdunit.KV("ExecStart", cfg.MeshdPath+" run --kubeconfig "+cfg.KubeletKubeconfig+" --secret-file /var/lib/socialweb/meshd/secret"),
				systemdunit.KV("Restart", "always"),
				systemdunit.KV("RestartSec", "2"),
				systemdunit.KV("StateDirectory", "socialweb/meshd"),
				systemdunit.KV("StateDirectoryMode", "0700"),
			}},
			{Name: "Install", Entries: []systemdunit.KeyValue{systemdunit.KV("WantedBy", "multi-user.target")}},
		}, DropIns: []systemdunit.DropIn{{Name: "10-socialweb-node.conf", Sections: []systemdunit.Section{
			{Name: "Unit", Entries: []systemdunit.KeyValue{
				systemdunit.KV("After", UnitMeshdInstall+" "+UnitPrep),
				systemdunit.KV("Wants", UnitMeshdInstall+" "+UnitPrep),
				systemdunit.KV("StartLimitIntervalSec", "0"),
			}},
		}}}}
		out = append(out, install, mesh)
	}
	return out
}

func oneshotUnit(name, description string, after, wants []string, exec string, restart, install bool) systemdunit.Unit {
	unitEntries := []systemdunit.KeyValue{systemdunit.KV("Description", description)}
	if len(after) > 0 {
		unitEntries = append(unitEntries, systemdunit.KV("After", joinStrings(after)))
	}
	if len(wants) > 0 {
		unitEntries = append(unitEntries, systemdunit.KV("Wants", joinStrings(wants)))
	}
	unitEntries = append(unitEntries, systemdunit.KV("StartLimitIntervalSec", "0"))
	service := []systemdunit.KeyValue{
		systemdunit.KV("Type", "oneshot"),
		systemdunit.KV("RemainAfterExit", "yes"),
		systemdunit.KV("ExecStart", exec),
	}
	if restart {
		service = append(service, systemdunit.KV("Restart", "on-failure"), systemdunit.KV("RestartSec", "5"))
	}
	service = append(service,
		systemdunit.KV("StandardOutput", "journal+console"),
		systemdunit.KV("StandardError", "journal+console"))
	unit := systemdunit.Unit{Name: name, Sections: []systemdunit.Section{
		{Name: "Unit", Entries: unitEntries},
		{Name: "Service", Entries: service},
	}}
	if install {
		unit.Sections = append(unit.Sections, systemdunit.Section{Name: "Install",
			Entries: []systemdunit.KeyValue{systemdunit.KV("WantedBy", "multi-user.target")}})
	}
	return unit
}

func joinStrings(values []string) string {
	var out strings.Builder
	for i, value := range values {
		if i > 0 {
			out.WriteString(" ")
		}
		out.WriteString(value)
	}
	return out.String()
}

func UnitFiles(cfg Config) []systemdunit.File {
	return systemdunit.Files(DefaultUnitDir, Units(cfg)...)
}

func UnitNames(cfg Config) []string {
	names := []string{UnitPrep, UnitK3sAgent, UnitDNSLink, UnitMasquerade, UnitCNI, UnitDiagnose}
	if cfg.Meshd != nil {
		names = append(names, UnitMeshdInstall, UnitMeshd)
	}
	return names
}

func InstallUnits(root string, cfg Config) error { return systemdunit.Install(root, UnitFiles(cfg)) }

func StartUnits(ctx context.Context, cfg Config, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	controller := systemdunit.Controller{}
	names := UnitNames(cfg)
	if err := controller.DaemonReload(ctx); err != nil {
		return err
	}
	if err := controller.Enable(ctx, names...); err != nil {
		return err
	}
	if err := controller.StartNoBlock(ctx, names...); err != nil {
		return err
	}
	log.Info("enabled and started the node units", "units", joinStrings(names))
	return nil
}
