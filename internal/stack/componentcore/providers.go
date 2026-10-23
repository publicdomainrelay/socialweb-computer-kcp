package componentcore

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	sshcontract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/ssh"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/requester"
)

func (c *Core) provideComponents() {
	registry := c.registry
	bus.Provide(c.handlers, schema.GroupKind{Group: v1alpha1.GroupName, Kind: requester.Kind}, sshcontract.CapabilityRun,
		func(_ context.Context, to commonv1.ObjectRef, _ []string) (sshcontract.Runner, error) {
			service, ok := registry.Service(to.Workspace, to.Kind, to.Name)
			if !ok {
				return nil, fmt.Errorf("componentcore: the Requester %s/%s is not registered", to.Workspace, to.Name)
			}
			runner, ok := service.(sshcontract.Runner)
			if !ok {
				return nil, fmt.Errorf("componentcore: the Requester %s/%s does not run commands", to.Workspace, to.Name)
			}
			return runner, nil
		})
}
