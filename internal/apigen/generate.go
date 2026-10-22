package apigen

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

const RevisionLockFile = "deploy/apiresourceschema-revisions.txt"

type Output struct {
	Path string
	Data []byte
}

func Generate(root string) ([]Output, error) {
	models, err := Build(root)
	if err != nil {
		return nil, err
	}
	lockData, err := os.ReadFile(filepath.Join(root, RevisionLockFile))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	lock, err := ParseRevisionLock(lockData)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RevisionLockFile, err)
	}

	var out []Output
	for _, m := range models {
		for _, r := range m.Resources {
			data, err := m.APIResourceSchema(r)
			if err != nil {
				return nil, err
			}
			name, hash, err := SpecHash(data)
			if err != nil {
				return nil, err
			}
			if err := lock.Admit(name, hash); err != nil {
				return nil, err
			}
			out = append(out, Output{Path: SchemaFile(r), Data: data})
		}
		for _, e := range m.Exports() {
			data, err := m.APIExport(e)
			if err != nil {
				return nil, err
			}
			out = append(out, Output{Path: ExportFile(e), Data: data})
		}
	}
	lockOut, err := lock.Marshal(RevisionOrder(lockData)...)
	if err != nil {
		return nil, err
	}
	out = append(out, Output{Path: RevisionLockFile, Data: lockOut})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func Write(dir string, outputs []Output) error {
	for _, o := range outputs {
		path := filepath.Join(dir, filepath.FromSlash(o.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, o.Data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
