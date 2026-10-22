package bundledactions

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed all:workflows all:tangy
var assets embed.FS

type Input struct {
	Default string `yaml:"default"`
}

type Action struct {
	Uses   string
	Path   string
	Using  string
	Main   string
	Inputs map[string]Input
	Source []byte
	YAML   []byte
}

type actionDef struct {
	Runs struct {
		Using string `yaml:"using"`
		Main  string `yaml:"main"`
	} `yaml:"runs"`
	Inputs map[string]Input `yaml:"inputs"`
}

func Lookup(uses string) (*Action, error) {
	value := strings.TrimSpace(uses)
	if value == "" {
		return nil, fmt.Errorf("action uses is empty")
	}
	if strings.HasPrefix(value, "./") || strings.HasPrefix(value, "/") {
		return nil, fmt.Errorf("net-only sandbox refuses local action %q", value)
	}
	parts := strings.SplitN(value, "@", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
		return nil, fmt.Errorf("unsupported uses format (expected org/repo@version): %q", value)
	}
	orgRepo := strings.TrimSpace(parts[0])
	if !strings.HasPrefix(orgRepo, "tangy/") {
		return nil, fmt.Errorf("action %q is not bundled; remote action downloads are disabled", orgRepo)
	}
	clean := path.Clean(orgRepo)
	if clean != orgRepo || strings.Contains(clean, "..") || !fs.ValidPath(clean) {
		return nil, fmt.Errorf("invalid bundled action path %q", orgRepo)
	}
	dir := clean
	yamlPath := path.Join(dir, "action.yml")
	yamlBytes, err := assets.ReadFile(yamlPath)
	if err != nil {
		yamlPath = path.Join(dir, "action.yaml")
		yamlBytes, err = assets.ReadFile(yamlPath)
		if err != nil {
			return nil, fmt.Errorf("bundled action %q not found", orgRepo)
		}
	}
	var def actionDef
	if err := yaml.Unmarshal(yamlBytes, &def); err != nil {
		return nil, fmt.Errorf("parse %s: %w", yamlPath, err)
	}
	main := strings.TrimSpace(def.Runs.Main)
	if main == "" {
		return nil, fmt.Errorf("bundled action %q has no main", orgRepo)
	}
	if !fs.ValidPath(main) || strings.Contains(main, "..") {
		return nil, fmt.Errorf("bundled action %q has invalid main %q", orgRepo, main)
	}
	source, err := assets.ReadFile(path.Join(dir, main))
	if err != nil {
		return nil, fmt.Errorf("bundled action %q main %q not found", orgRepo, main)
	}
	return &Action{
		Uses:   value,
		Path:   "bundled-actions/" + orgRepo,
		Using:  strings.TrimSpace(def.Runs.Using),
		Main:   main,
		Inputs: def.Inputs,
		Source: source,
		YAML:   yamlBytes,
	}, nil
}

func Workflows() map[string]string {
	entries, err := fs.ReadDir(assets, "workflows")
	if err != nil {
		return map[string]string{}
	}
	out := make(map[string]string)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".yml") {
			continue
		}
		data, err := assets.ReadFile(path.Join("workflows", name))
		if err != nil {
			continue
		}
		out[strings.TrimSuffix(name, ".yml")] = string(data)
	}
	return out
}

func Workflow(name string) ([]byte, bool) {
	clean := strings.TrimSpace(name)
	if clean == "" || strings.Contains(clean, "/") || strings.Contains(clean, "..") {
		return nil, false
	}
	data, err := assets.ReadFile(path.Join("workflows", clean+".yml"))
	if err != nil {
		return nil, false
	}
	return data, true
}

func Names() []string {
	all := Workflows()
	out := make([]string, 0, len(all))
	for name := range all {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
