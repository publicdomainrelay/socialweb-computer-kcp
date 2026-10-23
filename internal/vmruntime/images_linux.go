//go:build linux

package vmruntime

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/fcapi"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestimage"
)

func (r *firecracker) imagesOfWorkDirs(keep map[string]string) {
	configs, _ := filepath.Glob(filepath.Join(r.cfg.StateDir, "handles", "*", "vms", "*", ConfigFile))
	for _, p := range configs {
		var c fcapi.Config
		b, err := os.ReadFile(p)
		if err != nil || json.Unmarshal(b, &c) != nil || c.BootSource.KernelImagePath == "" {
			continue
		}
		sha := filepath.Base(filepath.Dir(c.BootSource.KernelImagePath))
		if _, ok := keep[sha]; !ok {
			keep[sha] = "the VM work dir " + filepath.Dir(p) + " boots it"
		}
	}
}

func (r *firecracker) CollectImages(ctx context.Context, keep map[string]string) (guestimage.GCReport, error) {
	all := map[string]string{}
	maps.Copy(all, keep)
	names, err := guestimage.Names()
	if err != nil {
		return guestimage.GCReport{}, err
	}
	for _, n := range names {
		all[filepath.Base(n)] = "versions.json of this binary pins it"
	}
	for _, bin := range r.cfg.Binaries {
		named, err := guestimage.NamedByBinary(ctx, bin)
		if err != nil {
			continue
		}
		for _, n := range named {
			if _, ok := all[filepath.Base(n)]; !ok {
				all[filepath.Base(n)] = "the binary " + bin + " pins it"
			}
		}
	}
	r.imagesOfWorkDirs(all)
	return r.imageStore().Collect(all, time.Now())
}
