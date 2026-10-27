//go:build darwin

package devsup

import (
	"fmt"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
)

func (s *server) openFDStore(u *unit) error {
	if u.spec.FDStore > 0 {
		return fmt.Errorf("%w: unit %s asks for an fd store, which the %s backend does not offer on darwin",
			hostsvc.ErrUnsupported, u.spec.Name, hostsvc.BackendProc)
	}
	return nil
}
