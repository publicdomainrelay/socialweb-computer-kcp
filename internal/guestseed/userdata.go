package guestseed

import (
	"cmp"
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestnet"
)

const (
	AcceptPathVM = "/root/secrets/publicdomainrelay.com/market/accept.json"

	SecretsPrefix = "/root/secrets/digitalocean.com/serviceaccount"

	OnNetworkBaseURLPath = SecretsPrefix + "/base_url"

	GuestCurlrcPath = "/root/.curlrc"

	GuestLoginScript = cloudinit.GuestLoginScriptPath

	GuestLoginUnit = cloudinit.GuestLoginUnit

	GuestLoginTokenPath = cloudinit.GuestLoginTokenPath

	GuestLoginNamespace = cloudinit.GuestLoginNamespace

	GuestLoginDefaultMount = cloudinit.GuestLoginDefaultMount

	GuestLoginRoute = "/v1"

	OnNetworkScript = "/usr/local/bin/send-onnetwork.sh"

	OnNetworkUnit = "guest-onnetwork.service"

	IrohServiceUnit = "iroh.service"

	IrohListenerScript = "/usr/local/bin/iroh-listener.sh"

	IrohTicketPath = "/run/guest-iroh-ticket"

	GuestFQDNPath = "/run/guest-fqdn"

	IrohTargetPort = 22

	PoweroffModule = cloudinit.PoweroffEndsVMModule

	PoweroffPath = cloudinit.PoweroffEndsVMPath
)

var (
	guestLoginRoleRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,200}$`)

	guestLoginMountRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

	guestBindSecretRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

	guestLocalhostURL = regexp.MustCompile(`(?i)https://([a-z0-9.-]+\.localhost)`)

	guestAnyLocalhostURL = regexp.MustCompile(`(?i)https?://([a-z0-9.-]+\.localhost)`)
)

type GuestLoginOptions = cloudinit.GuestLoginOptions

type ProvisionOptions struct {
	UserData string

	Login *GuestLoginOptions

	AcceptPath string

	FallbackURL string

	BearerAudience string

	Reporter bool

	BaseURL bool

	HostAddress string

	TLSPort int
}

func GuestLoginModule(opts GuestLoginOptions) (cloudinit.UserDataModule, error) {
	if err := validateGuestLogin(opts); err != nil {
		return nil, err
	}
	return cloudinit.GuestLoginModule(opts), nil
}

func IrohTransportModule(userData string) (cloudinit.UserDataModule, error) {
	if strings.Contains(userData, IrohServiceUnit) || strings.Contains(userData, IrohListenerScript) {
		return nil, nil
	}
	modules, err := cloudinit.GetUserDataModules([]any{cloudinit.GuestTransportModule})
	if err != nil {
		return nil, err
	}
	if len(modules) != 1 {
		return nil, fmt.Errorf("guestseed: the %s user-data module is not registered", cloudinit.GuestTransportModule)
	}
	inner := modules[0]
	return func(ctx cloudinit.Context) (cloudinit.UserDataPatch, error) {
		patch, err := inner(ctx)
		if err != nil {
			return cloudinit.UserDataPatch{}, err
		}
		if ctx.SSHAuthorizedKey != "" {
			return patch, nil
		}
		files := patch.WriteFiles[:0]
		for _, file := range patch.WriteFiles {
			if file.Path == "/root/.ssh/authorized_keys" {
				continue
			}
			files = append(files, file)
		}
		patch.WriteFiles = files
		return patch, nil
	}, nil
}

func WithIrohTransport(userData string, authorizedKeys, hostAliases []string) (string, error) {
	if !strings.HasPrefix(strings.TrimLeft(userData, " \t\r\n"), "#cloud-config") {
		return userData, nil
	}
	module, err := IrohTransportModule(userData)
	if err != nil || module == nil {
		return userData, err
	}
	keys := make([]string, 0, len(authorizedKeys))
	for _, key := range authorizedKeys {
		key = strings.TrimSpace(key)
		if key != "" {
			keys = append(keys, key)
		}
	}
	return cloudinit.BuildUserData(cloudinit.BuildOptions{
		Base:    userData,
		Modules: []any{module},
		Ctx: cloudinit.Context{
			SSHAuthorizedKey: strings.Join(keys, "\n"),
			TargetPort:       IrohTargetPort,
			HostAliases:      hostAliases,
			DumbpipeVersion:  cloudinit.DumbpipeVersionDefault,
		},
	})
}

func AcceptBundle(acceptPath string, bundle map[string]any) (cloudinit.UserDataModule, error) {
	acceptPath = cmp.Or(acceptPath, AcceptPathVM)
	if err := checkAbsolute(acceptPath); err != nil {
		return nil, err
	}
	return cloudinit.AcceptBundleModule(acceptPath, bundle), nil
}

func OnNetworkReporterModule(userData, fallbackURL, acceptPath, bearerAudience string) (cloudinit.UserDataModule, bool, error) {
	if strings.Contains(userData, OnNetworkUnit) {
		return nil, false, nil
	}
	acceptPath = cmp.Or(acceptPath, AcceptPathVM)
	if err := checkAbsolute(acceptPath); err != nil {
		return nil, false, err
	}
	targets := guestnet.RelayTargets(userData, fallbackURL)
	if len(targets) == 0 || targets[0].URL == "" {
		return nil, false, fmt.Errorf("guestseed: the guest reports on-network to no URL; the fallback %q is not one", fallbackURL)
	}
	for _, target := range targets {
		if err := checkURLLike(target.URL, "on-network URL"); err != nil {
			return nil, false, err
		}
	}
	if bearerAudience != "" && !safeInDoubleQuotes(bearerAudience) {
		return nil, false, fmt.Errorf("guestseed: bearer audience %q carries a character a shell would read", bearerAudience)
	}
	var attempts strings.Builder
	auth := ""
	if bearerAudience != "" {
		auth = ` -H "Authorization: Bearer ${TOKEN}"`
	}
	for i, target := range targets {
		code := fmt.Sprintf("CODE%d", i+1)
		resolve := ""
		if target.Resolve != "" {
			resolve = `--resolve ` + strconv.Quote(target.Resolve) + ` `
		}
		fmt.Fprintf(&attempts, "  %s=\"$(curl -sS -m 20 -o /tmp/guest-onnetwork.out -w '%%{http_code}' %s\\\n", code, resolve)
		fmt.Fprintf(&attempts, "    -X POST %s -H 'Content-Type: application/json'%s -d \"${PAYLOAD}\" \\\n", strconv.Quote(target.URL), auth)
		attempts.WriteString("    2>>/tmp/guest-onnetwork.err || echo 000)\"\n")
		fmt.Fprintf(&attempts, "  echo \"guest-onnetwork: http ${%s} %s\"\n", code, target.URL)
		fmt.Fprintf(&attempts, "  if [ \"${%s}\" = \"200\" ]; then echo \"guest-onnetwork: reported ${FQDN}\"; exit 0; fi\n", code)
	}
	script := `#!/usr/bin/env bash
set -uo pipefail
ACCEPT_JSON="` + acceptPath + `"
if [ ! -f "${ACCEPT_JSON}" ]; then echo "guest-onnetwork: no ${ACCEPT_JSON}"; exit 0; fi
FQDN=""
for _ in $(seq 1 45); do
  FQDN="$(cat /run/guest-fqdn 2>/dev/null || true)"
  [ -n "${FQDN}" ] && break
  systemctl is-failed --quiet iroh.service && { echo "guest-onnetwork: iroh.service failed"; break; }
  sleep 2
done
if [ -z "${FQDN}" ]; then
  echo "guest-onnetwork: no /run/guest-fqdn"
  echo "guest-onnetwork: iroh is $(systemctl is-active iroh.service 2>&1), restarts=$(systemctl show -p NRestarts --value iroh.service 2>&1)"
  journalctl -u iroh.service --no-pager -n 40 2>&1 | tail -40
  exit 0
fi
ACCEPT_URI="$(jq -r '.accept.uri // empty' "${ACCEPT_JSON}")"
ACCEPT_CID="$(jq -r '.accept.cid // empty' "${ACCEPT_JSON}")"
if [ -z "${ACCEPT_URI}" ] || [ -z "${ACCEPT_CID}" ]; then echo "guest-onnetwork: accept.json has no accept ref"; exit 0; fi
PAYLOAD="$(jq -nc --arg au "${ACCEPT_URI}" --arg ac "${ACCEPT_CID}" --arg addr "${FQDN}" \
  '{acceptUri: $au, acceptCid: $ac, address: $addr}')"
echo "guest-onnetwork: reporting ${FQDN}"
for _ in 1 2 3 4 5; do
` + bearerLines(bearerAudience) + attempts.String() + `  sleep 6
done
echo "guest-onnetwork: gave up; last response: $(cat /tmp/guest-onnetwork.out 2>/dev/null)"
`
	after := "iroh.service"
	wants := "iroh.service"
	if bearerAudience != "" {
		after += " " + GuestLoginUnit
		wants += " " + GuestLoginUnit
	}
	unit := strings.Join([]string{
		"[Unit]",
		"Description=Report the guest iroh ticket to the market",
		"After=network-online.target " + after,
		"Wants=network-online.target " + wants,
		"",
		"[Service]",
		"Type=oneshot",
		"RemainAfterExit=yes",
		"ExecStart=" + OnNetworkScript,
		"StandardOutput=journal+console",
		"StandardError=journal+console",
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	}, "\n")
	module := func(cloudinit.Context) (cloudinit.UserDataPatch, error) {
		return cloudinit.UserDataPatch{
			WriteFiles: []cloudinit.WriteFileEntry{
				{Path: OnNetworkScript, Owner: "root:root", Permissions: "0700", Content: script},
				{Path: "/etc/systemd/system/" + OnNetworkUnit, Owner: "root:root", Permissions: "0644", Content: unit},
			},
			RuncmdPrepend: []any{
				"systemctl daemon-reload",
				"systemctl enable --no-block " + OnNetworkUnit,
				"systemctl start --no-block " + OnNetworkUnit,
			},
		}, nil
	}
	return module, true, nil
}

func bearerLines(audience string) string {
	if audience == "" {
		return ""
	}
	return `  TOKEN="$(` + GuestLoginScript + ` token "` + audience + `")" || TOKEN=""
  if [ -z "${TOKEN}" ]; then echo "guest-onnetwork: no token from the bidder's OpenBao yet"; sleep 6; continue; fi
`
}

func OnNetworkBaseURLModule(userData, fallbackURL string) (cloudinit.UserDataModule, bool, error) {
	if strings.Contains(userData, OnNetworkBaseURLPath) {
		return nil, false, nil
	}
	target := guestnet.OnNetworkURLFromBundle(userData, fallbackURL)
	if target == "" {
		return nil, false, fmt.Errorf("guestseed: the guest names no on-network URL; the fallback %q is not one", fallbackURL)
	}
	if err := checkURLLike(target, "on-network URL"); err != nil {
		return nil, false, err
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return nil, false, fmt.Errorf("guestseed: on-network URL %q is not a URL", target)
	}
	origin := u.Scheme + "://" + u.Host
	parent := path.Dir(OnNetworkBaseURLPath)
	module := func(cloudinit.Context) (cloudinit.UserDataPatch, error) {
		return cloudinit.UserDataPatch{
			WriteFiles:    []cloudinit.WriteFileEntry{{Path: OnNetworkBaseURLPath, Owner: "root:root", Permissions: "0600", Content: origin + "\n"}},
			RuncmdPrepend: []any{[]any{"sh", "-c", "install -d -m 0700 -o root -g root " + shellQuote(parent)}},
		}, nil
	}
	return module, true, nil
}

func PointAtHostModules(userData, gateway string, tlsPort int) (string, []cloudinit.UserDataModule, error) {
	if tlsPort < 0 || tlsPort > 65535 {
		return "", nil, fmt.Errorf("guestseed: guest TLS port %d is not a port", tlsPort)
	}
	rewritten := userData
	if tlsPort > 0 {
		rewritten = addGuestTLSPort(userData, tlsPort)
	}
	hosts := localhostHosts(rewritten)
	if len(hosts) == 0 {
		return rewritten, nil, nil
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(gateway))
	if err != nil || !addr.Is4() {
		return "", nil, fmt.Errorf("guestseed: host address %q is not an IPv4 address", gateway)
	}
	var lines []any
	for _, host := range hosts {
		lines = append(lines, []any{"sh", "-c", "echo " + addr.String() + " " + host + " >> /etc/hosts"})
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	var files []cloudinit.WriteFileEntry
	if tlsPort > 0 {
		files = []cloudinit.WriteFileEntry{{Path: GuestCurlrcPath, Owner: "root:root", Permissions: "0644",
			Content: "resolve = *:" + strconv.Itoa(tlsPort) + ":" + addr.String() + "\n"}}
	}
	module := func(cloudinit.Context) (cloudinit.UserDataPatch, error) {
		return cloudinit.UserDataPatch{WriteFiles: files, RuncmdPrepend: lines}, nil
	}
	return rewritten, []cloudinit.UserDataModule{module}, nil
}

func InjectAcceptBundle(userData string, bundle map[string]any, acceptPath string) (string, error) {
	module, err := AcceptBundle(acceptPath, bundle)
	if err != nil {
		return "", err
	}
	return cloudinit.BuildUserData(cloudinit.BuildOptions{
		Base:    userData,
		Modules: []any{module, PoweroffModule},
	})
}

func WithGuestLogin(userData string, opts GuestLoginOptions) (string, error) {
	module, err := GuestLoginModule(opts)
	if err != nil {
		return "", err
	}
	return cloudinit.BuildUserData(cloudinit.BuildOptions{Base: userData, Modules: []any{module}})
}

func WithOnNetworkReporter(userData, fallbackURL, acceptPath, bearerAudience string) (string, error) {
	module, add, err := OnNetworkReporterModule(userData, fallbackURL, acceptPath, bearerAudience)
	if err != nil || !add {
		return userData, err
	}
	return cloudinit.BuildUserData(cloudinit.BuildOptions{Base: userData, Modules: []any{module}})
}

func WithOnNetworkBaseURL(userData, fallbackURL string) (string, error) {
	module, add, err := OnNetworkBaseURLModule(userData, fallbackURL)
	if err != nil || !add {
		return userData, err
	}
	return cloudinit.BuildUserData(cloudinit.BuildOptions{Base: userData, Modules: []any{module}})
}

func Provision(opts ProvisionOptions) (string, error) {
	acceptPath := opts.AcceptPath
	acceptPath = cmp.Or(acceptPath, AcceptPathVM)
	modules := []any{}
	ctx := cloudinit.Context{SecretsAcceptPath: acceptPath}
	if opts.TLSPort > 0 && opts.TLSPort <= 65535 {
		opts.UserData = addGuestTLSPort(opts.UserData, opts.TLSPort)
		opts.FallbackURL = addGuestTLSPort(opts.FallbackURL, opts.TLSPort)
		if opts.Login != nil {
			login := *opts.Login
			login.URL = addGuestTLSPort(login.URL, opts.TLSPort)
			opts.Login = &login
		}
	}
	if opts.Login != nil {
		module, err := GuestLoginModule(*opts.Login)
		if err != nil {
			return "", err
		}
		ctx.VMName = opts.Login.Role
		modules = append(modules, module)
	}
	if opts.Reporter {
		module, add, err := OnNetworkReporterModule(opts.UserData, opts.FallbackURL, acceptPath, opts.BearerAudience)
		if err != nil {
			return "", err
		}
		if add {
			modules = append(modules, module)
		}
	}
	if opts.BaseURL {
		module, add, err := OnNetworkBaseURLModule(opts.UserData, opts.FallbackURL)
		if err != nil {
			return "", err
		}
		if add {
			modules = append(modules, module)
		}
	}
	rewritten, hostModules, err := PointAtHostModules(opts.UserData, opts.HostAddress, opts.TLSPort)
	if err != nil {
		return "", err
	}
	for _, module := range hostModules {
		modules = append(modules, module)
	}
	if len(modules) == 0 {
		return rewritten, nil
	}
	return cloudinit.BuildUserData(cloudinit.BuildOptions{Base: rewritten, Modules: modules, Ctx: ctx})
}

func validateGuestLogin(opts GuestLoginOptions) error {
	url := strings.TrimRight(strings.TrimSpace(opts.URL), "/")
	if err := checkURLLike(url, "guest login URL"); err != nil {
		return err
	}
	if !guestLoginRoleRE.MatchString(opts.Role) {
		return fmt.Errorf("guestseed: guest login role %q is not a ComputeVM name", opts.Role)
	}
	if !guestBindSecretRE.MatchString(opts.BindSecret) {
		return fmt.Errorf("guestseed: guest login bind secret is not 64 lowercase hex characters")
	}
	mount := opts.Mount
	mount = cmp.Or(mount, GuestLoginDefaultMount)
	if !guestLoginMountRE.MatchString(mount) {
		return fmt.Errorf("guestseed: guest login mount %q is not an auth mount name", mount)
	}
	if opts.SSHDir != "" {
		if err := checkAbsolute(opts.SSHDir); err != nil {
			return err
		}
	}
	if opts.TokenPath != "" {
		if err := checkAbsolute(opts.TokenPath); err != nil {
			return err
		}
	}
	if opts.Attempts < 0 || opts.Attempts > 1000 {
		return fmt.Errorf("guestseed: guest login attempts %d is not between 0 and 1000", opts.Attempts)
	}
	if opts.RetryDelaySec < 0 || opts.RetryDelaySec > 300 {
		return fmt.Errorf("guestseed: guest login retry delay %d s is not between 0 and 300", opts.RetryDelaySec)
	}
	return nil
}

func addGuestTLSPort(userData string, port int) string {
	matches := guestLocalhostURL.FindAllStringSubmatchIndex(userData, -1)
	if len(matches) == 0 {
		return userData
	}
	var b strings.Builder
	last := 0
	suffix := ":" + strconv.Itoa(port)
	for _, m := range matches {
		end := m[1]
		if end < len(userData) && hostContinuation(rune(userData[end])) {
			continue
		}
		b.WriteString(userData[last:m[0]])
		b.WriteString("https://")
		b.WriteString(userData[m[2]:m[3]])
		b.WriteString(suffix)
		last = end
	}
	b.WriteString(userData[last:])
	return b.String()
}

func hostContinuation(r rune) bool {
	return r == ':' || r == '.' || r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func localhostHosts(userData string) []string {
	seen := map[string]bool{}
	var hosts []string
	for _, m := range guestAnyLocalhostURL.FindAllStringSubmatch(userData, -1) {
		host := strings.ToLower(m[1])
		if seen[host] {
			continue
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

func checkAbsolute(p string) error {
	if p == "" || !strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
		return fmt.Errorf("guestseed: %q is not an absolute path inside the guest", p)
	}
	if !safeInDoubleQuotes(p) {
		return fmt.Errorf("guestseed: %q carries a character a shell would read", p)
	}
	return nil
}

func checkURLLike(raw, what string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("guestseed: %s %q is not an absolute http or https URL", what, raw)
	}
	if u.User != nil || u.Fragment != "" {
		return fmt.Errorf("guestseed: %s %q carries a user or fragment", what, raw)
	}
	if !safeInDoubleQuotes(raw) {
		return fmt.Errorf("guestseed: %s %q carries a character a shell would read", what, raw)
	}
	return nil
}

func safeInDoubleQuotes(s string) bool {
	return !strings.ContainsAny(s, "\"'`$\\\n\r")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
