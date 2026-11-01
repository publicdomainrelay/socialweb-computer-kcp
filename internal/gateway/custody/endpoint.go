package custody

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
)

var ErrNotReady = errors.New("not Ready")

func EndpointFor(status v1alpha1.OpenBaoServerStatus, client, mount string) (Endpoint, v1alpha1.OpenBaoServerClientRefs, error) {
	if status.Phase != v1alpha1.OpenBaoServerPhaseReady {
		return Endpoint{}, v1alpha1.OpenBaoServerClientRefs{}, fmt.Errorf("the OpenBaoServer is %s, %w", status.Phase, ErrNotReady)
	}
	refs, err := openbaoctl.ClientRefs(status, client)
	if err != nil {
		return Endpoint{}, refs, err
	}
	ca, err := readFileRef("the OpenBao CA", status.Refs.CA)
	if err != nil {
		return Endpoint{}, refs, err
	}
	cert, err := readFileRef("the OpenBao client certificate", refs.ClientCert)
	if err != nil {
		return Endpoint{}, refs, err
	}
	key, err := readFileRef("the OpenBao client key", refs.ClientKey)
	if err != nil {
		return Endpoint{}, refs, err
	}
	return Endpoint{Address: status.Address, CACert: ca, ClientCert: cert, ClientKey: key, CertMount: refs.CertAuthMount,
		CertRole: refs.CertAuthRole, Mount: strings.Trim(mount, "/"), Timeout: DefaultTimeout}, refs, nil
}

func readFileRef(what, ref string) ([]byte, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, fmt.Errorf("%s: the ref is empty", what)
	}
	raw, err := os.ReadFile(openbaoctl.FileRef(ref))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return raw, nil
}
