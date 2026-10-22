package appctl

import (
	"fmt"
	"io/fs"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/deploy"
)

const (
	OptionMapPath  = "apps/option-map.yaml"
	MapToOptions   = "options"
	MapToProfile   = "profile"
	MapToState     = "state"
	MapToListener  = "listener"
	MapToRemoved   = "removed"
	MapToSpec      = "spec."
	MapToNetwork   = "network."
	MapToContract  = "contract."
	MapToCLI       = "cli"
	MapToRequester = "requester"
	LegacyRemoval  = "legacy-transport"
	maxOptionValue = 4096
)

type OptionMap struct {
	Apps []AppOptions `json:"apps"`
}

type AppOptions struct {
	App     string          `json:"app"`
	Entry   string          `json:"entry"`
	Options []OptionMapping `json:"options"`
}

type OptionMapping struct {
	Name   string `json:"name"`
	To     string `json:"to"`
	Reason string `json:"reason,omitempty"`
}

func LoadOptionMap() (OptionMap, error) {
	b, err := fs.ReadFile(deploy.FS, OptionMapPath)
	if err != nil {
		return OptionMap{}, fmt.Errorf("appctl: %w", err)
	}
	var m OptionMap
	if err := yaml.UnmarshalStrict(b, &m); err != nil {
		return OptionMap{}, fmt.Errorf("appctl: %s: %w", OptionMapPath, err)
	}
	for _, a := range m.Apps {
		for _, o := range a.Options {
			switch {
			case o.To == MapToOptions, o.To == MapToRequester, o.To == MapToProfile, o.To == MapToState, o.To == MapToListener, o.To == MapToCLI,
				strings.HasPrefix(o.To, MapToSpec), strings.HasPrefix(o.To, MapToNetwork), strings.HasPrefix(o.To, MapToContract):
			case o.To == MapToRemoved:
				if o.Reason != LegacyRemoval {
					return OptionMap{}, fmt.Errorf("appctl: %s: %s --%s is removed for %q; the only accepted removal is %s",
						OptionMapPath, a.App, o.Name, o.Reason, LegacyRemoval)
				}
			default:
				return OptionMap{}, fmt.Errorf("appctl: %s: %s --%s maps to %q", OptionMapPath, a.App, o.Name, o.To)
			}
		}
	}
	return m, nil
}

func (m OptionMap) app(name string) (AppOptions, bool) {
	for _, a := range m.Apps {
		if a.App == name {
			return a, true
		}
	}
	return AppOptions{}, false
}

func OptionArgs(app string, opts []v1alpha1.ApplicationOption) ([]string, error) {
	return optionArgs(app, MapToOptions, opts)
}

func RequesterOptionArgs(app string, opts []v1alpha1.ApplicationOption) ([]string, error) {
	return optionArgs(app, MapToRequester, opts)
}

func optionArgs(app, want string, opts []v1alpha1.ApplicationOption) ([]string, error) {
	if len(opts) == 0 {
		return nil, nil
	}
	var out []string
	for _, o := range opts {
		if len(o.Value) > maxOptionValue {
			return nil, fmt.Errorf("appctl: --%s of %s is longer than %d bytes", o.Name, app, maxOptionValue)
		}
		switch o.Value {
		case "true":
			out = append(out, "--"+o.Name)
		case "false":
		default:
			out = append(out, "--"+o.Name, o.Value)
		}
	}
	return out, nil
}

type AppArg struct {
	Name  string
	Value string
	To    string
}

func ParseAppArgs(app string, args []string) ([]AppArg, error) {
	if len(args) == 0 {
		return nil, nil
	}
	mapped, err := LoadOptionMap()
	if err != nil {
		return nil, err
	}
	known := map[string]OptionMapping{}
	if a, ok := mapped.app(app); ok {
		for _, o := range a.Options {
			known[o.Name] = o
		}
	}
	var out []AppArg
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") || arg == "--" {
			return nil, fmt.Errorf("appctl: %q is no option of %s; pass --name value or --name=value", arg, app)
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if name == "" {
			return nil, fmt.Errorf("appctl: %s has an option with no name", app)
		}
		if !hasValue {
			value = "true"
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++
				value = args[i]
			}
		}
		if len(value) > maxOptionValue {
			return nil, fmt.Errorf("appctl: --%s of %s is longer than %d bytes", name, app, maxOptionValue)
		}
		to := MapToOptions
		if mapping, ok := known[name]; ok {
			to = mapping.To
		}
		if to == MapToRemoved {
			return nil, fmt.Errorf("appctl: --%s of %s is removed: %s", name, app, known[name].Reason)
		}
		out = append(out, AppArg{Name: name, Value: value, To: to})
	}
	return out, nil
}
