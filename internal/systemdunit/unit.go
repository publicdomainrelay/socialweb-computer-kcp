package systemdunit

import (
	"cmp"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type KeyValue struct {
	Key string

	Value string
}

func KV(key, value string) KeyValue { return KeyValue{Key: key, Value: value} }

type Section struct {
	Name string

	Entries []KeyValue
}

func (s Section) With(key, value string) Section {
	s.Entries = append(append([]KeyValue(nil), s.Entries...), KV(key, value))
	return s
}

type DropIn struct {
	Name string

	Sections []Section
}

type Unit struct {
	Name string

	Sections []Section

	DropIns []DropIn
}

func renderSections(sections []Section) []byte {
	var b strings.Builder
	for i, section := range sections {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("[")
		b.WriteString(section.Name)
		b.WriteString("]\n")
		for _, entry := range section.Entries {
			b.WriteString(entry.Key)
			b.WriteString("=")
			b.WriteString(entry.Value)
			b.WriteString("\n")
		}
	}
	return []byte(b.String())
}

func (u Unit) Render() []byte { return renderSections(u.Sections) }

func (d DropIn) Render() []byte { return renderSections(d.Sections) }

type File struct {
	Path string

	Content []byte

	Mode fs.FileMode
}

func Files(dir string, units ...Unit) []File {
	out := []File{}
	for _, unit := range units {
		out = append(out, File{Path: filepath.Join(dir, unit.Name), Content: unit.Render(), Mode: 0o644})
		for _, dropIn := range unit.DropIns {
			out = append(out, File{Path: filepath.Join(dir, unit.Name+".d", dropIn.Name), Content: dropIn.Render(), Mode: 0o644})
		}
	}
	return out
}

func target(root, path string) (string, error) {
	clean := filepath.Clean(path)
	if root == "" {
		if !filepath.IsAbs(clean) {
			return "", fmt.Errorf("systemdunit: %s is not an absolute path", path)
		}
		return clean, nil
	}
	if filepath.IsAbs(clean) {
		clean = strings.TrimPrefix(clean, string(filepath.Separator))
	}
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("systemdunit: %s escapes root %s", path, root)
	}
	return filepath.Join(root, clean), nil
}

func Install(root string, files []File) error {
	for _, file := range files {
		dest, err := target(root, file.Path)
		if err != nil {
			return err
		}
		mode := file.Mode
		mode = cmp.Or(mode, 0o644)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("systemdunit: creating the directory of %s: %w", dest, err)
		}
		tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".tmp-*")
		if err != nil {
			return fmt.Errorf("systemdunit: creating a temporary file beside %s: %w", dest, err)
		}
		name := tmp.Name()
		remove := true
		defer func() {
			if remove {
				os.Remove(name)
			}
		}()
		if _, err := tmp.Write(file.Content); err != nil {
			tmp.Close()
			return fmt.Errorf("systemdunit: writing %s: %w", dest, err)
		}
		if err := tmp.Chmod(mode); err != nil {
			tmp.Close()
			return fmt.Errorf("systemdunit: setting the mode of %s: %w", dest, err)
		}
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return fmt.Errorf("systemdunit: syncing %s: %w", dest, err)
		}
		if err := tmp.Close(); err != nil {
			return fmt.Errorf("systemdunit: closing %s: %w", dest, err)
		}
		if err := os.Rename(name, dest); err != nil {
			return fmt.Errorf("systemdunit: renaming %s into place: %w", dest, err)
		}
		remove = false
	}
	return nil
}
