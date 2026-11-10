package compose

import (
	"context"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

const vaultBindQueue = 64

func VaultBindWatch(c *Composer, rootKind string) ctlruntime.Option {
	ch := make(chan ctlruntime.Request, vaultBindQueue)
	c.Bus().Subscribe(func(key bus.Key) {
		if key.Kind != KindVault {
			return
		}
		go func() {
			ri, err := c.Client(key.Workspace, v1alpha1.VaultGVR)
			if err != nil {
				return
			}
			vault, err := kube.Get[v1alpha1.Vault](context.Background(), ri, key.Name)
			if err != nil {
				return
			}
			for _, req := range mapByLabel(rootKind, ctlruntime.Request{}, vault.GetLabels()) {
				select {
				case ch <- req:
				default:
				}
			}
		}()
	})
	return ctlruntime.Events(ch)
}
