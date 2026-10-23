package denospec

import (
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
)

type ServiceAccountRef struct {
	Name string `json:"name"`

	Namespace string `json:"namespace,omitempty"`
}

type Permission struct {
	Allow bool `json:"allow,omitempty"`

	AllowList []string `json:"allowList,omitempty"`

	Deny bool `json:"deny,omitempty"`

	DenyList []string `json:"denyList,omitempty"`
}

type Permissions struct {
	All bool `json:"all,omitempty"`

	NoPrompt bool `json:"noPrompt,omitempty"`

	HRTime bool `json:"hrtime,omitempty"`

	Read *Permission `json:"read,omitempty"`

	Write *Permission `json:"write,omitempty"`

	Net *Permission `json:"net,omitempty"`

	Env *Permission `json:"env,omitempty"`

	Run *Permission `json:"run,omitempty"`

	FFI *Permission `json:"ffi,omitempty"`

	Sys *Permission `json:"sys,omitempty"`

	Import *Permission `json:"import,omitempty"`

	IgnoreEnv *Permission `json:"ignoreEnv,omitempty"`

	AllowScripts []string `json:"allowScripts,omitempty"`
}

type PodTemplate struct {
	DenoJSON runtime.RawExtension `json:"denoJson"`

	DenoLock string `json:"denoLock,omitempty"`

	Script string `json:"script"`

	Permissions *Permissions `json:"permissions,omitempty"`

	ServiceAccount *ServiceAccountRef `json:"serviceAccount,omitempty"`

	APIServer string `json:"apiServer,omitempty"`

	Env map[string]string `json:"env,omitempty"`
}

type ExecProbe struct {
	Command []string `json:"command,omitempty"`

	PeriodSeconds *int32 `json:"periodSeconds,omitempty"`

	FailureThreshold *int32 `json:"failureThreshold,omitempty"`

	TimeoutSeconds *int32 `json:"timeoutSeconds,omitempty"`
}

type RestartPolicy string

const (
	RestartAlways RestartPolicy = "Always"

	RestartOnFailure RestartPolicy = "OnFailure"

	RestartNever RestartPolicy = "Never"
)

func EffectiveRestartPolicy(policy RestartPolicy) RestartPolicy {
	if policy == "" {
		return RestartAlways
	}
	return policy
}

var capabilityFlags = []struct {
	name string

	permission func(*Permissions) *Permission
}{
	{"read", func(p *Permissions) *Permission { return p.Read }},
	{"write", func(p *Permissions) *Permission { return p.Write }},
	{"net", func(p *Permissions) *Permission { return p.Net }},
	{"env", func(p *Permissions) *Permission { return p.Env }},
	{"run", func(p *Permissions) *Permission { return p.Run }},
	{"ffi", func(p *Permissions) *Permission { return p.FFI }},
	{"sys", func(p *Permissions) *Permission { return p.Sys }},
	{"import", func(p *Permissions) *Permission { return p.Import }},
}

func Validate(p *Permissions) error {
	if p == nil {
		return nil
	}
	for _, capability := range capabilityFlags {
		if err := validatePermission(capability.name, capability.permission(p)); err != nil {
			return err
		}
	}
	if p.IgnoreEnv != nil {
		if p.IgnoreEnv.Allow || p.IgnoreEnv.Deny {
			return errors.New("denospec: ignoreEnv takes only an allowList, not allow or deny")
		}
		if err := validateValues("ignoreEnv", p.IgnoreEnv.AllowList); err != nil {
			return err
		}
	}
	return validateValues("allowScripts", p.AllowScripts)
}

func validatePermission(capability string, p *Permission) error {
	if p == nil {
		return nil
	}
	if err := validateValues(capability+" allowList", p.AllowList); err != nil {
		return err
	}
	return validateValues(capability+" denyList", p.DenyList)
}

func validateValues(what string, values []string) error {
	for _, value := range values {
		if value == "" {
			return fmt.Errorf("denospec: %s holds an empty value", what)
		}
		if strings.Contains(value, ",") {
			return fmt.Errorf("denospec: %s value %q holds a comma; deno separates values with commas", what, value)
		}
	}
	return nil
}

func ProbeCommand(runtime, shim, probe, name, path string, allowEnv []string) []string {
	return ProbeCommandReading(runtime, shim, probe, name, path, allowEnv, nil)
}

func ProbeCommandReading(runtime, shim, probe, name, path string, allowEnv, allowRead []string) []string {
	if runtime == "" {
		runtime = "deno"
	}
	args := []string{runtime, "run", "--allow-env=" + strings.Join(allowEnv, ","), "--allow-net"}
	if len(allowRead) > 0 {
		args = append(args, "--allow-read="+strings.Join(allowRead, ","))
	}
	args = append(args, "--preload", shim, probe, name)
	if path == "" {
		path = "/"
	}
	return append(args, path)
}

func Args(p *Permissions) ([]string, error) {
	if err := Validate(p); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, nil
	}
	if p.All {
		return []string{"-A"}, nil
	}
	var args []string
	if p.HRTime {
		args = append(args, "--allow-hrtime")
	}
	for _, capability := range capabilityFlags {
		args = append(args, permissionArgs("--allow-"+capability.name, capability.permission(p), false)...)
		args = append(args, permissionArgs("--deny-"+capability.name, capability.permission(p), true)...)
	}
	if p.IgnoreEnv != nil && len(p.IgnoreEnv.AllowList) > 0 {
		args = append(args, "--ignore-env="+strings.Join(p.IgnoreEnv.AllowList, ","))
	}
	if len(p.AllowScripts) > 0 {
		args = append(args, "--allow-scripts="+strings.Join(p.AllowScripts, ","))
	}
	if p.NoPrompt {
		args = append(args, "--no-prompt")
	}
	return args, nil
}

func permissionArgs(flag string, permission *Permission, deny bool) []string {
	if permission == nil {
		return nil
	}
	if deny {
		if permission.Deny {
			return []string{flag}
		}
		if len(permission.DenyList) > 0 {
			return []string{flag + "=" + strings.Join(permission.DenyList, ",")}
		}
		return nil
	}
	if permission.Allow {
		return []string{flag}
	}
	if len(permission.AllowList) > 0 {
		return []string{flag + "=" + strings.Join(permission.AllowList, ",")}
	}
	return nil
}
