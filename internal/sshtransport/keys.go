package sshtransport

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayapi"
)

const (
	KeysDefaultMaxBodyBytes = int64(8 * 1024)

	KeysDefaultMaxKeys = 100

	MaxKeyNameLength = 128
)

var (
	keyAlgorithmPattern = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp256|ecdsa-sha2-nistp384|ecdsa-sha2-nistp521|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com)$`)

	keyBase64Pattern = regexp.MustCompile(`^[A-Za-z0-9+/]+=*$`)

	rkeyPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,512}$`)
)

const tidAlphabet = "234567abcdefghijklmnopqrstuvwxyz"

type Authenticator interface {
	Authenticate(r *http.Request) (*gatewayapi.WebCaller, error)
}

type AgentProvider interface {
	AgentFor(caller *gatewayapi.WebCaller) gatewayapi.WebAccountAgent
}

type KeysOptions struct {
	Agents AgentProvider

	Auth Authenticator

	MaxBodyBytes int64

	MaxKeys int

	Log *slog.Logger
}

type Keys struct {
	opts    KeysOptions
	maxBody int64
	maxKeys int
	log     *slog.Logger
}

func NewKeys(opts KeysOptions) (*Keys, error) {
	if opts.Agents == nil {
		return nil, fmt.Errorf("sshtransport: keys API has no account agent provider")
	}
	if opts.Auth == nil {
		return nil, fmt.Errorf("sshtransport: keys API has no authenticator")
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = KeysDefaultMaxBodyBytes
	}
	if opts.MaxKeys <= 0 {
		opts.MaxKeys = KeysDefaultMaxKeys
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Keys{opts: opts, maxBody: opts.MaxBodyBytes, maxKeys: opts.MaxKeys, log: opts.Log}, nil
}

func (k *Keys) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /keys", k.list)
	mux.HandleFunc("POST /keys", k.create)
	mux.HandleFunc("DELETE /keys/{rkey}", k.remove)
}

func (k *Keys) Handler() http.Handler {
	mux := http.NewServeMux()
	k.Register(mux)
	return mux
}

func (k *Keys) caller(w http.ResponseWriter, r *http.Request) (*gatewayapi.WebCaller, bool) {
	who, err := k.opts.Auth.Authenticate(r)
	if err != nil {
		k.log.Error("keys_identify_failed", "err", err)
		writeSSHJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the vault is not answering"})
		return nil, false
	}
	if who == nil {
		writeSSHJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized", "message": "sign in again"})
		return nil, false
	}
	return who, true
}

type keyView struct {
	Rkey  string `json:"rkey"`
	Name  string `json:"name"`
	KeyID string `json:"keyId"`
}

func (k *Keys) list(w http.ResponseWriter, r *http.Request) {
	who, ok := k.caller(w, r)
	if !ok {
		return
	}
	found, err := k.opts.Agents.AgentFor(who).ListRecords(r.Context(), who.DID, BadgeBlueKeysNSID, k.maxKeys)
	if err != nil {
		k.failed(w, who.DID, "list keys", err)
		return
	}
	keys := make([]keyView, 0, len(found))
	for _, record := range found {
		challenge, _ := record.Value["challenge"].(string)
		service, _ := record.Value["service"].(string)
		if challenge != who.DID || service != RequesterAssociateService {
			continue
		}
		rkey := record.URI
		if slash := strings.LastIndex(rkey, "/"); slash >= 0 {
			rkey = rkey[slash+1:]
		}
		name, _ := record.Value["name"].(string)
		if name == "" {
			name = "unnamed"
		}
		keys = append(keys, keyView{Rkey: rkey, Name: name, KeyID: stringOf(record.Value["keyId"])})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeSSHJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

type parsedPublicKey struct {
	algo    string
	key     string
	comment string
}

func parsePublicKey(text string) (parsedPublicKey, bool) {
	parts := strings.Fields(strings.TrimSpace(text))
	if len(parts) < 2 {
		return parsedPublicKey{}, false
	}
	if !keyAlgorithmPattern.MatchString(parts[0]) {
		return parsedPublicKey{}, false
	}
	if !keyBase64Pattern.MatchString(parts[1]) {
		return parsedPublicKey{}, false
	}
	return parsedPublicKey{algo: parts[0], key: parts[1], comment: strings.Join(parts[2:], " ")}, true
}

func (k *Keys) create(w http.ResponseWriter, r *http.Request) {
	who, ok := k.caller(w, r)
	if !ok {
		return
	}
	if declared, err := strconv.ParseInt(r.Header.Get("Content-Length"), 10, 64); err == nil && declared > k.maxBody {
		writeSSHJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "body too large"})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, k.maxBody+1))
	if err != nil || int64(len(raw)) > k.maxBody {
		writeSSHJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "body too large"})
		return
	}
	var body struct {
		PublicKey any `json:"publicKey"`
		Name      any `json:"name"`
	}
	_ = json.Unmarshal(raw, &body)
	line, _ := body.PublicKey.(string)
	parsed, valid := parsePublicKey(line)
	if !valid {
		writeSSHJSON(w, http.StatusBadRequest, map[string]any{"error": "that does not look like an OpenSSH public key"})
		return
	}
	name := parsed.comment
	if provided, ok := body.Name.(string); ok && strings.TrimSpace(provided) != "" {
		name = strings.TrimSpace(provided)
	}
	if name == "" {
		name = "key"
	}
	if runes := []rune(name); len(runes) > MaxKeyNameLength {
		name = string(runes[:MaxKeyNameLength])
	}
	rkey := newTID()
	record := map[string]any{
		"$type":     BadgeBlueKeysNSID,
		"keyId":     parsed.algo + " " + parsed.key,
		"name":      name,
		"challenge": who.DID,
		"service":   RequesterAssociateService,
		"createdAt": time.Now().UTC().Format(time.RFC3339Nano),
	}
	if _, err := k.opts.Agents.AgentFor(who).CreateRecord(r.Context(), who.DID, BadgeBlueKeysNSID, rkey, record); err != nil {
		k.failed(w, who.DID, "register the key", err)
		return
	}
	k.log.Info("keys_registered", "did", who.DID, "rkey", rkey)
	writeSSHJSON(w, http.StatusOK, map[string]any{"rkey": rkey})
}

func (k *Keys) remove(w http.ResponseWriter, r *http.Request) {
	who, ok := k.caller(w, r)
	if !ok {
		return
	}
	rkey := r.PathValue("rkey")
	if !rkeyPattern.MatchString(rkey) {
		writeSSHJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid key id"})
		return
	}
	writes := []gatewayapi.Write{{Action: "delete", Collection: BadgeBlueKeysNSID, Rkey: rkey}}
	if err := k.opts.Agents.AgentFor(who).ApplyWrites(r.Context(), who.DID, writes); err != nil {
		k.failed(w, who.DID, "remove the key", err)
		return
	}
	k.log.Info("keys_removed", "did", who.DID, "rkey", rkey)
	writeSSHJSON(w, http.StatusOK, map[string]any{"rkey": rkey})
}

func (k *Keys) failed(w http.ResponseWriter, did, action string, err error) {
	k.log.Error("keys_failed", "did", did, "action", action, "err", err)
	writeSSHJSON(w, http.StatusBadGateway, map[string]any{"error": "could not " + action + ": " + err.Error()})
}

func newTID() string {
	n := uint64(time.Now().UnixMilli())*1000 + cryptoUint64(1000)
	out := make([]byte, 13)
	for i := 12; i >= 0; i-- {
		out[i] = tidAlphabet[n&31]
		n >>= 5
	}
	return string(out)
}

func cryptoUint64(max uint64) uint64 {
	if max == 0 {
		return 0
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return uint64(time.Now().UnixNano()) % max
	}
	n := uint64(0)
	for _, b := range raw {
		n = n<<8 | uint64(b)
	}
	return n % max
}

func stringOf(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case nil:
		return ""
	case json.Number:
		return v.String()
	default:
		return fmt.Sprint(v)
	}
}

func writeSSHJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
