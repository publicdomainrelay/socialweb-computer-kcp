package clicfg

import (
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"sort"
	"strconv"
	"strings"
)

//go:embed options.json
var optionsJSON []byte

const ConfigPathEnv = "CONFIG_PATH_SOCIALWEB_COMPUTER"

const (
	TypeString  = "string"
	TypeNumber  = "number"
	TypeBoolean = "boolean"
)

const (
	SourceFlag    = "flag"
	SourceEnv     = "env"
	SourceConfig  = "config"
	SourceDefault = "default"
)

type Option struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Env         string `json:"env,omitempty"`
	Default     any    `json:"default,omitempty"`
	Build       string `json:"build,omitempty"`
	Bare        string `json:"bare,omitempty"`
}

const BuildDev = "dev"

type Spec struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Options     map[string]Option `json:"options"`
}

func Load() (Spec, error) {
	s, err := Parse(optionsJSON)
	if err != nil {
		return Spec{}, err
	}
	if releaseBuild {
		for name, o := range s.Options {
			if o.Build == BuildDev {
				delete(s.Options, name)
			}
		}
	}
	return s, nil
}

func ReleaseBuild() bool { return releaseBuild }

func Parse(b []byte) (Spec, error) {
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return Spec{}, fmt.Errorf("clicfg: options.json: %w", err)
	}
	for name, o := range s.Options {
		switch o.Type {
		case TypeString, TypeNumber, TypeBoolean:
		default:
			return Spec{}, fmt.Errorf("clicfg: option %s has type %q: want string, number or boolean", name, o.Type)
		}
		if o.Build != "" && o.Build != BuildDev {
			return Spec{}, fmt.Errorf("clicfg: option %s has build %q: want empty or %q", name, o.Build, BuildDev)
		}
		if o.Type == TypeBoolean && o.Default != nil {
			return Spec{}, fmt.Errorf("clicfg: boolean option %s carries a default; a boolean is a flag", name)
		}
		if o.Bare != "" && o.Type != TypeString {
			return Spec{}, fmt.Errorf("clicfg: option %s has a bare value and type %s; only a string takes --%s alone", name, o.Type, name)
		}
	}
	return s, nil
}

type Env interface {
	LookupEnv(key string) (string, bool)
}

type OSEnv struct{}

func (OSEnv) LookupEnv(key string) (string, bool) { return os.LookupEnv(key) }

type MapEnv map[string]string

func (m MapEnv) LookupEnv(key string) (string, bool) {
	v, ok := m[key]
	return v, ok
}

type Values struct {
	vals   map[string]any
	source map[string]string
}

func (v Values) Has(name string) bool {
	_, ok := v.vals[name]
	return ok
}

func (v Values) Source(name string) string { return v.source[name] }

func (v Values) With(name string, val any) Values {
	out := Values{vals: map[string]any{}, source: map[string]string{}}
	maps.Copy(out.vals, v.vals)
	maps.Copy(out.source, v.source)
	out.vals[name], out.source[name] = val, SourceFlag
	return out
}

func (v Values) String(name string) string {
	switch x := v.vals[name].(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

func (v Values) Int(name string) int {
	switch x := v.vals[name].(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func (v Values) Bool(name string) bool {
	switch x := v.vals[name].(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1"
	}
	return false
}

type stringFlag struct {
	set  bool
	val  string
	bare string
}

func (f *stringFlag) String() string { return f.val }

func (f *stringFlag) IsBoolFlag() bool { return f.bare != "" }

func (f *stringFlag) Set(s string) error {
	if f.bare != "" && s == "true" {
		s = f.bare
	}
	f.set, f.val = true, s
	return nil
}

func coerce(name string, typ, raw string) (any, error) {
	switch typ {
	case TypeNumber:
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("clicfg: option %s wants a number, got %q", name, raw)
		}
		return n, nil
	case TypeBoolean:
		return raw == "true" || raw == "1", nil
	}
	return raw, nil
}

func ConfigFile(env Env) (map[string]any, error) {
	path, ok := env.LookupEnv(ConfigPathEnv)
	if !ok || path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("clicfg: %s=%s: %w", ConfigPathEnv, path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("clicfg: %s=%s: %w", ConfigPathEnv, path, err)
	}
	return m, nil
}

func (s Spec) Resolve(command string, names []string, args []string, env Env, config map[string]any, out io.Writer) (Values, []string, error) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(out)
	strs := map[string]*stringFlag{}
	bools := map[string]*bool{}
	for _, name := range names {
		o, ok := s.Options[name]
		if !ok {
			return Values{}, nil, fmt.Errorf("clicfg: %s names option %s, which options.json does not define", command, name)
		}
		usage := o.Description
		if o.Env != "" {
			usage += " (env " + o.Env + ")"
		}
		if o.Default != nil {
			usage += fmt.Sprintf(" (default %v)", o.Default)
		}
		if o.Type == TypeBoolean {
			bools[name] = fs.Bool(name, false, usage)
			continue
		}
		strs[name] = &stringFlag{bare: o.Bare}
		fs.Var(strs[name], name, usage)
	}
	fs.Usage = func() {
		fmt.Fprintf(out, "usage: socialweb-computer %s [options]\n", command)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return Values{}, nil, err
	}
	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	v := Values{vals: map[string]any{}, source: map[string]string{}}
	for _, name := range names {
		o := s.Options[name]
		set := func(val any, src string) {
			v.vals[name], v.source[name] = val, src
		}
		if visited[name] {
			if o.Type == TypeBoolean {
				set(*bools[name], SourceFlag)
				continue
			}
			val, err := coerce(name, o.Type, strs[name].val)
			if err != nil {
				return Values{}, nil, err
			}
			set(val, SourceFlag)
			continue
		}
		if o.Env != "" {
			if raw, ok := env.LookupEnv(o.Env); ok {
				val, err := coerce(name, o.Type, raw)
				if err != nil {
					return Values{}, nil, fmt.Errorf("%w (from env %s)", err, o.Env)
				}
				set(val, SourceEnv)
				continue
			}
		}
		if c, ok := config[name]; ok {
			set(c, SourceConfig)
			continue
		}
		if o.Default != nil {
			set(o.Default, SourceDefault)
		}
	}
	return v, fs.Args(), nil
}

var ErrMissing = errors.New("clicfg: a required option is unset")

func (s Spec) Require(v Values, name string) error {
	if v.Has(name) && v.String(name) != "" {
		return nil
	}
	o := s.Options[name]
	hint := "--" + name
	if o.Env != "" {
		hint += " or env " + o.Env
	}
	return fmt.Errorf("%w: %s (%s); it has no default", ErrMissing, name, hint)
}

func (s Spec) Names() []string {
	var out []string
	for n := range s.Options {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (s Spec) EnvNames() map[string]string {
	out := map[string]string{}
	for n, o := range s.Options {
		if o.Env != "" {
			out[o.Env] = n
		}
	}
	return out
}

func IsPortLike(name string) bool {
	return strings.HasSuffix(name, "-port") || name == "port" || strings.HasSuffix(name, "-addr") ||
		strings.HasSuffix(name, "-address")
}
