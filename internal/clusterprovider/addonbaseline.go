package clusterprovider

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/scale"
)

const AddOnFieldManager = "socialweb-addons"

const AddOnBaselineName = "addon-baseline.json"

const addOnBaselineMode = 0o600

func AddOnBaselinePath(dataDir string) string {
	return filepath.Join(dataDir, AddOnBaselineName)
}

func ReadAddOnBaseline(dataDir string) (scale.Baseline, bool, error) {
	path := AddOnBaselinePath(dataDir)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return scale.Baseline{}, false, nil
	}
	if err != nil {
		return scale.Baseline{}, false, fmt.Errorf("clusterprovider: reading the add-on baseline at %s: %w", path, err)
	}
	var b scale.Baseline
	if err := json.Unmarshal(raw, &b); err != nil {
		return scale.Baseline{}, false, fmt.Errorf("clusterprovider: the add-on baseline at %s is not JSON: %v", path, err)
	}
	return b, true, nil
}

func WriteAddOnBaseline(dataDir string, b scale.Baseline) error {
	path := AddOnBaselinePath(dataDir)
	body, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("clusterprovider: encoding the add-on baseline for %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(dataDir, AddOnBaselineName+".")
	if err != nil {
		return fmt.Errorf("clusterprovider: a temporary file beside %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(addOnBaselineMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("clusterprovider: the mode of %s: %w", tmp.Name(), err)
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("clusterprovider: writing %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("clusterprovider: closing %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("clusterprovider: renaming %s over %s: %w", tmp.Name(), path, err)
	}
	return nil
}
