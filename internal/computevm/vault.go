package computevm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	VaultServer     = "bidder"
	VaultClient     = "computevm"
	VaultMount      = "bidder"
	VaultGrace      = 2 * time.Minute
	VaultLoginFor   = 10 * time.Minute
	VaultConflictOf = 10
)

type vaultLogin struct {
	client *openbao.Client
	until  time.Time
}

func (c Config) vaultServer() string {
	if c.VaultServer == "" {
		return VaultServer
	}
	return c.VaultServer
}

func (c Config) vaultClient() string {
	if c.VaultClient == "" {
		return VaultClient
	}
	return c.VaultClient
}

func (c Config) vaultMount() string {
	if c.VaultMount == "" {
		return VaultMount
	}
	return c.VaultMount
}

func (ct *Controller) server(ctx context.Context, name string) (*v1alpha1.OpenBaoServer, error) {
	u, err := ct.servers.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o, err := decode[v1alpha1.OpenBaoServer](u)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func (ct *Controller) vaultFor(ctx context.Context, server string) (*openbao.Client, error) {
	ct.mu.Lock()
	l, ok := ct.logins[server]
	ct.mu.Unlock()
	if ok && time.Now().Before(l.until) {
		return l.client, nil
	}
	o, err := ct.server(ctx, server)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, fmt.Errorf("no OpenBaoServer %s in this workspace", server)
	}
	client, _, err := openbaoctl.ClientLogin(ctx, o.Status, ct.c.vaultClient())
	if err != nil {
		return nil, fmt.Errorf("the OpenBaoServer %s as client %s: %w", server, ct.c.vaultClient(), err)
	}
	until := time.Now().Add(VaultLoginFor)
	if at := client.RefreshAt(); !at.IsZero() && at.Before(until) {
		until = at
	}
	ct.mu.Lock()
	ct.logins[server] = vaultLogin{client: client, until: until}
	ct.mu.Unlock()
	return client, nil
}

func (ct *Controller) forgetLogin(server string) {
	ct.mu.Lock()
	delete(ct.logins, server)
	ct.mu.Unlock()
}

func userDataPath(ref commonv1.SecretRef) (string, error) {
	mount, path := strings.Trim(ref.Mount, "/"), strings.Trim(ref.Path, "/")
	if v1alpha1.VaultServer(ref.Vault, "") == "" || mount == "" || path == "" || strings.Contains(path, "..") {
		return "", fmt.Errorf("spec.userData.vault names server %q, mount %q and path %q; all three are required", v1alpha1.VaultServer(ref.Vault, ""), ref.Mount, ref.Path)
	}
	return mount + "/data/" + path, nil
}

func (ct *Controller) userData(ctx context.Context, o v1alpha1.ComputeVM) (string, bool, error) {
	ref := o.Spec.UserData.Vault
	path, err := userDataPath(ref)
	if err != nil {
		return "", false, err
	}
	client, err := ct.vaultFor(ctx, v1alpha1.VaultServer(ref.Vault, ""))
	if err != nil {
		ct.c.log().Info("the user data waits for the bidder's vault", "computevm", o.Name, "err", err.Error())
		return "", false, nil
	}
	data, err := client.GetKV(ctx, path)
	var re *openbao.ResponseError
	if openbao.IsAuthError(err) {
		ct.forgetLogin(v1alpha1.VaultServer(ref.Vault, ""))
		if client, err = ct.vaultFor(ctx, v1alpha1.VaultServer(ref.Vault, "")); err != nil {
			ct.c.log().Info("the user data waits for the bidder's vault", "computevm", o.Name, "err", err.Error())
			return "", false, nil
		}
		data, err = client.GetKV(ctx, path)
	}
	switch {
	case errors.As(err, &re) && re.Status == 404:
		return "", false, nil
	case errors.As(err, &re) && re.Status == http.StatusForbidden:
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	raw, _ := data[v1alpha1.GuestUserDataKey].(string)
	if raw == "" {
		return "", false, fmt.Errorf("%s holds no %s", path, v1alpha1.GuestUserDataKey)
	}
	return raw, true, nil
}

func guestScope(d *v1alpha1.OpenBaoServerDeclared, mount string) (int, bool) {
	for i, s := range d.KV {
		if strings.Trim(s.Mount, "/") == mount && strings.Trim(s.Prefix, "/") == v1alpha1.GuestVaultPrefix {
			return i, true
		}
	}
	return 0, false
}

func (ct *Controller) declareGuests(ctx context.Context, have map[string]bool) error {
	names := make([]string, 0, len(have))
	for n := range have {
		names = append(names, n)
	}
	sort.Strings(names)
	server, mount := ct.c.vaultServer(), ct.c.vaultMount()
	want := v1alpha1.OpenBaoKVScope{Mount: mount, Prefix: v1alpha1.GuestVaultPrefix, Keys: names, Grace: VaultGrace.String()}
	for attempt := 0; ; attempt++ {
		u, err := ct.servers.Get(ctx, server, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if u.GetDeletionTimestamp() != nil {
			return nil
		}
		o, err := decode[v1alpha1.OpenBaoServer](u)
		if err != nil {
			return err
		}
		d := &v1alpha1.OpenBaoServerDeclared{}
		if o.Spec.Declared != nil {
			raw, _ := json.Marshal(o.Spec.Declared)
			if err := json.Unmarshal(raw, d); err != nil {
				return err
			}
		}
		if i, ok := guestScope(d, mount); ok {
			cur := d.KV[i]
			if slices.Equal(cur.Keys, want.Keys) && cur.Grace == want.Grace {
				return nil
			}
			d.KV[i] = want
		} else {
			d.KV = append(d.KV, want)
		}
		o.Spec.Declared = d
		next, err := encode(&o, "OpenBaoServer")
		if err != nil {
			return err
		}
		if _, err := ct.servers.Update(ctx, next, metav1.UpdateOptions{FieldManager: v1alpha1.FieldManagerComputeVMController}); err != nil {
			if apierrors.IsConflict(err) && attempt < VaultConflictOf {
				continue
			}
			return err
		}
		ct.c.log().Info("the bidder's vault declares exactly the live ComputeVMs' guest material", "openbaoserver", server,
			"scope", mount+"/"+v1alpha1.GuestVaultPrefix, "guests", names)
		return nil
	}
}
