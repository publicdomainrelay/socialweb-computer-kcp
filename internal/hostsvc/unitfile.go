package hostsvc

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/systemdunit"
)

const (
	ArgsVar       = "SWC_ARGS"
	WantedBy      = "default.target"
	EnvFileSuffix = ".env"
)

type InstalledFile struct {
	Unit    []byte
	Env     []byte
	EnvPath string
}

type InstallOptions struct {
	Exec    string
	EnvFile string
}

func quoteArg(a string) (string, error) {
	if a == "" || strings.ContainsAny(a, " \t\n\"'\\$%;") {
		return "", fmt.Errorf("hostsvc: argument %q cannot travel through an EnvironmentFile word list", a)
	}
	return a, nil
}

func signalName(s int) string {
	switch s {
	case 2:
		return "SIGINT"
	case 9:
		return "SIGKILL"
	case 15:
		return "SIGTERM"
	case 3:
		return "SIGQUIT"
	}
	return strconv.Itoa(s)
}

func RenderInstalled(spec UnitSpec, o InstallOptions) (InstalledFile, error) {
	if err := ValidateSpec(spec); err != nil {
		return InstalledFile{}, err
	}
	if len(spec.Exec) == 0 {
		return InstalledFile{}, errors.New("hostsvc: an installed unit needs an executable")
	}
	exe := o.Exec
	if exe == "" {
		exe = spec.Exec[0]
	}
	if !filepath.IsAbs(exe) || !filepath.IsAbs(o.EnvFile) {
		return InstalledFile{}, fmt.Errorf("hostsvc: %s: the executable and the environment file must be absolute paths", spec.Name)
	}
	args := make([]string, 0, len(spec.Exec)-1)
	for _, a := range spec.Exec[1:] {
		q, err := quoteArg(a)
		if err != nil {
			return InstalledFile{}, fmt.Errorf("%s: %w", spec.Name, err)
		}
		args = append(args, q)
	}
	desc := spec.Description
	if desc == "" {
		desc = spec.Name
	}
	slice := spec.Slice
	if slice == "" {
		slice = DefaultSlice
	}
	restart := spec.Restart
	if restart == "" {
		restart = RestartNo
	}
	oom := spec.OOMPolicy
	if oom == "" {
		oom = "stop"
	}
	var unitEntries []systemdunit.KeyValue
	add := func(k, v string) {
		unitEntries = append(unitEntries, systemdunit.KV(k, v))
	}
	add("Description", desc)
	if len(spec.After) > 0 {
		add("After", strings.Join(spec.After, " "))
	}
	if len(spec.Wants) > 0 {
		add("Wants", strings.Join(spec.Wants, " "))
	}
	add("StartLimitIntervalSec", "0")
	service := []systemdunit.KeyValue{
		systemdunit.KV("Type", "exec"),
		systemdunit.KV("Slice", slice),
		systemdunit.KV("KillMode", "control-group"),
		systemdunit.KV("OOMPolicy", oom),
		systemdunit.KV("Restart", string(restart)),
	}
	if restart != RestartNo {
		service = append(service, systemdunit.KV("RestartSec", "2"))
	}
	if spec.WorkDir != "" {
		service = append(service, systemdunit.KV("WorkingDirectory", spec.WorkDir))
	}
	for _, e := range spec.Env {
		q, err := quoteArg(e)
		if err != nil {
			return InstalledFile{}, fmt.Errorf("%s: %w", spec.Name, err)
		}
		service = append(service, systemdunit.KV("Environment", q))
	}
	service = append(service,
		systemdunit.KV("EnvironmentFile", o.EnvFile),
		systemdunit.KV("ExecStart", exe+" $"+ArgsVar))
	if spec.KillSignal != 0 {
		service = append(service, systemdunit.KV("KillSignal", signalName(int(spec.KillSignal))))
	}
	if spec.StopTimeout > 0 {
		service = append(service, systemdunit.KV("TimeoutStopSec", strconv.FormatInt(int64(spec.StopTimeout.Seconds()), 10)))
	}
	if spec.Output.StdoutFile != "" {
		service = append(service, systemdunit.KV("StandardOutput", "append:"+spec.Output.StdoutFile))
	}
	if spec.Output.StderrFile != "" {
		service = append(service, systemdunit.KV("StandardError", "append:"+spec.Output.StderrFile))
	}
	if spec.Limits.MemoryMax > 0 {
		service = append(service, systemdunit.KV("MemoryMax", strconv.FormatUint(spec.Limits.MemoryMax, 10)),
			systemdunit.KV("MemorySwapMax", "0"))
	}
	if spec.Delegate {
		service = append(service, systemdunit.KV("Delegate", "yes"))
	}
	if spec.Limits.TasksMax > 0 {
		service = append(service, systemdunit.KV("TasksMax", strconv.FormatUint(spec.Limits.TasksMax, 10)))
	}
	if spec.Limits.CPUWeight > 0 {
		service = append(service, systemdunit.KV("CPUWeight", strconv.FormatUint(spec.Limits.CPUWeight, 10)))
	}
	if spec.FDStore > 0 {
		service = append(service, systemdunit.KV("FileDescriptorStoreMax", strconv.Itoa(spec.FDStore)),
			systemdunit.KV("NotifyAccess", "main"))
	}
	unit := systemdunit.Unit{Sections: []systemdunit.Section{
		{Name: "Unit", Entries: unitEntries},
		{Name: "Service", Entries: service},
		{Name: "Install", Entries: []systemdunit.KeyValue{systemdunit.KV("WantedBy", WantedBy)}},
	}}
	env := ArgsVar + "=" + strings.Join(args, " ") + "\n"
	return InstalledFile{Unit: unit.Render(), Env: []byte(env), EnvPath: o.EnvFile}, nil
}

func RenderDropIn(after, wants []string) []byte {
	var entries []systemdunit.KeyValue
	if len(after) > 0 {
		entries = append(entries, systemdunit.KV("After", strings.Join(after, " ")))
	}
	if len(wants) > 0 {
		entries = append(entries, systemdunit.KV("Wants", strings.Join(wants, " ")))
	}
	return systemdunit.Unit{Sections: []systemdunit.Section{{Name: "Unit", Entries: entries}}}.Render()
}

func ParseArgsEnv(b []byte) ([]string, error) {
	for l := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, ArgsVar+"="); ok {
			return strings.Fields(v), nil
		}
	}
	return nil, fmt.Errorf("hostsvc: no %s line", ArgsVar)
}
