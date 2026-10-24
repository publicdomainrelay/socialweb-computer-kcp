package manifests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"sigs.k8s.io/yaml"
)

type None struct{}

type PermissionInventory struct {
	ProviderCluster string `json:",omitempty"`
}

type MarketIdentity struct {
	Workspace string `json:",omitempty"`
}

type MarketNetwork struct {
	NetworkID           string `json:",omitempty"`
	StateDir            string `json:",omitempty"`
	GuestTLSDir         string `json:",omitempty"`
	PDSPrivateKeyHex    string `json:",omitempty"`
	BobPDSPrivateKeyHex string `json:",omitempty"`
	DispatcherPort      int    `json:",omitempty"`
	GuestTLSPort        int    `json:",omitempty"`
	Ports               NetworkPorts
}

type NetworkPorts struct {
	Plc           int
	Relay         int
	Dispatcher    int
	DispatcherTLS int
	AlicePds      int
	BobPds        int
}

type MarketBidder struct {
	StateDir         string `json:",omitempty"`
	Handle           string `json:",omitempty"`
	PdsUrl           string `json:",omitempty"`
	PlcDirectoryUrl  string `json:",omitempty"`
	IngressProxyHost string `json:",omitempty"`
	GuestTLSPort     int    `json:",omitempty"`
	CABundle         string `json:",omitempty"`
	Policy           string `json:",omitempty"`
	Vault            BidderVault
	Kcp              KcpRef
	Guest            BidderGuest
	Trust            string
	Read             string
	ExtraArgs        string
	ProviderArgs     string
	Pds              RepoPds
}

type RepoPds struct {
	StateDir          string `json:",omitempty"`
	Identity          string `json:",omitempty"`
	KcpServer         string `json:",omitempty"`
	KcpWorkspace      string `json:",omitempty"`
	VaultServer       string `json:",omitempty"`
	VaultClient       string `json:",omitempty"`
	VaultKcpServer    string `json:",omitempty"`
	VaultMount        string `json:",omitempty"`
	VaultMountClient  string `json:",omitempty"`
	VaultWorkspace    string `json:",omitempty"`
	PlcDirectoryUrl   string `json:",omitempty"`
	IngressProxyHost  string `json:",omitempty"`
	AssociateService  string `json:",omitempty"`
	WatchEventSources string
	Trust             string
	Read              string
	ExtraArgs         string
}

type BidderGuest struct {
	StateDir    string `json:",omitempty"`
	Handle      string `json:",omitempty"`
	VaultClient string `json:",omitempty"`
	Read        string
	ExtraArgs   string
}

type BidderVault struct {
	Server    string `json:",omitempty"`
	Client    string `json:",omitempty"`
	KcpServer string `json:",omitempty"`
}

type MarketRequester struct {
	StateDir  string `json:",omitempty"`
	CABundle  string `json:",omitempty"`
	Kcp       KcpRef
	Vault     BidderVault
	ExtraArgs string
}

type SshGateway struct {
	StateDir   string `json:",omitempty"`
	CABundle   string `json:",omitempty"`
	EnvAllow   string `json:",omitempty"`
	Kcp        KcpRef
	Vault      BidderVault
	IrohDial   IrohDial
	ExtraArgs  string
	ExtraRead  string
	VaultExtra string
}

type IrohDial struct {
	Unit     string `json:",omitempty"`
	Socket   string `json:",omitempty"`
	Dumbpipe string `json:",omitempty"`
}

type KcpRef struct {
	Server    string `json:",omitempty"`
	Workspace string `json:",omitempty"`
}

var valueTypes = map[string]func() reflect.Type{
	"None":                func() reflect.Type { return reflect.TypeFor[*None]() },
	"PermissionInventory": func() reflect.Type { return reflect.TypeFor[*PermissionInventory]() },
	"MarketIdentity":      func() reflect.Type { return reflect.TypeFor[*MarketIdentity]() },
	"MarketNetwork":       func() reflect.Type { return reflect.TypeFor[*MarketNetwork]() },
	"MarketBidder":        func() reflect.Type { return reflect.TypeFor[*MarketBidder]() },
	"Gateway":             func() reflect.Type { return reflect.TypeFor[*Gateway]() },
	"MarketRequester":     func() reflect.Type { return reflect.TypeFor[*MarketRequester]() },
	"SshGateway":          func() reflect.Type { return reflect.TypeFor[*SshGateway]() },
}

type Gateway struct {
	StateDir          string `json:",omitempty"`
	KcpServer         string `json:",omitempty"`
	ExtraProjected    string
	GatewayWorkspace  string `json:",omitempty"`
	GatewayName       string `json:",omitempty"`
	TLS               string `json:",omitempty"`
	Trust             string `json:",omitempty"`
	ExtraArgs         string
	ExtraRead         string
	TestArgs          string
	TestRead          string
	IdentityWorkspace string `json:",omitempty"`
	Vault             GatewayVault
	Requester         GatewayRequester
	Pds               RepoPds
}

type GatewayRequester struct {
	StateDir       string `json:",omitempty"`
	Vault          GatewayVault
	ExtraArgs      string
	ExtraRead      string
	ExtraProjected string
}

type GatewayVault struct {
	Server string `json:",omitempty"`
	Client string `json:",omitempty"`
}

const CaptureLogicalCluster = "LogicalCluster"

var captureKinds = map[string]reflect.Kind{
	CaptureLogicalCluster: reflect.String,
}

func ValueTypeNames() []string {
	out := make([]string, 0, len(valueTypes))
	for n := range valueTypes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func NewValues(name string) (any, error) {
	t, ok := valueTypes[name]
	if !ok {
		return nil, fmt.Errorf("manifests: no values type %q (%v)", name, ValueTypeNames())
	}
	return reflect.New(t().Elem()).Interface(), nil
}

func DecodeValues(name string, raw []byte) (any, error) {
	v, err := NewValues(name)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return v, nil
	}
	if err := yaml.UnmarshalStrict(raw, v); err != nil {
		return nil, fmt.Errorf("manifests: values %s: %w", name, err)
	}
	return v, nil
}

func SetCaptured(values any, c Capture, raw string) error {
	rv := reflect.ValueOf(values)
	if rv.Kind() != reflect.Pointer || rv.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("manifests: capture into %s: values is not a pointer to a struct", c.Into)
	}
	f := rv.Elem().FieldByName(c.Into)
	if !f.IsValid() || !f.CanSet() || f.Kind() != captureKinds[c.From] {
		return fmt.Errorf("manifests: capture %s into %s: no such %s field", c.From, c.Into, captureKinds[c.From])
	}
	switch c.From {
	case CaptureLogicalCluster:
		if raw == "" {
			return fmt.Errorf("manifests: capture %s: an empty logical cluster", c.Into)
		}
		f.SetString(raw)
	default:
		return fmt.Errorf("manifests: capture %s: unknown source %q", c.Into, c.From)
	}
	return nil
}

func dataOf(values any) (map[string]any, error) {
	b, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("manifests: values: %w", err)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	out := map[string]any{}
	if err := d.Decode(&out); err != nil {
		return nil, fmt.Errorf("manifests: values: %w", err)
	}
	return out, nil
}
