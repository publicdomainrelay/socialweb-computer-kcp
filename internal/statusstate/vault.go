package statusstate

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"
)

const maxCertificateScan = 256

type MetadataReader interface {
	Read(ctx context.Context, path string) (map[string]any, error)

	List(ctx context.Context, path string) ([]string, error)
}

type OpenBaoMetadataSource struct {
	Open func(ctx context.Context, server string) (MetadataReader, error)

	RotationWindow time.Duration
}

func (c *Controller) reconcileVaultCredential(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) {
	var obj v1alpha1.VaultCredential
	if err := decode(u, &obj); err != nil {
		c.log.Warn("decoding a VaultCredential", "workspace", c.workspace, "name", u.GetName(), "err", err.Error())
		return
	}
	generation := u.GetGeneration()
	status := obj.Status
	status.ObservedGeneration = generation
	server := strings.TrimSpace(obj.Spec.Server)
	mount := strings.TrimSpace(obj.Spec.Mount)
	path := strings.TrimSpace(obj.Spec.Path)
	if server == "" || mount == "" || path == "" {
		setReady(&status.Conditions, generation, false, ReasonWaiting, "the VaultCredential needs a server, a mount and a path")
		c.write(ctx, dyn, VaultCredentialGVR, u, &status)
		return
	}
	if c.vault == nil {
		setReady(&status.Conditions, generation, false, ReasonSourceUnavailable, "no vault source is configured")
		c.write(ctx, dyn, VaultCredentialGVR, u, &status)
		return
	}
	observation, err := c.vault.CredentialMetadata(ctx, server, mount, path)
	if err != nil {
		setReady(&status.Conditions, generation, false, ReasonSourceError, "reading the credential metadata: "+err.Error())
		c.write(ctx, dyn, VaultCredentialGVR, u, &status)
		return
	}
	status.Exists = observation.Exists
	status.ExpiresAt = observation.ExpiresAt
	status.RotationDue = observation.RotationDue
	setReady(&status.Conditions, generation, true, ReasonReady, "the credential metadata is observed without the secret values")
	c.write(ctx, dyn, VaultCredentialGVR, u, &status)
}

func (c *Controller) reconcileVaultCertificate(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) {
	var obj v1alpha1.VaultCertificate
	if err := decode(u, &obj); err != nil {
		c.log.Warn("decoding a VaultCertificate", "workspace", c.workspace, "name", u.GetName(), "err", err.Error())
		return
	}
	generation := u.GetGeneration()
	status := obj.Status
	status.ObservedGeneration = generation
	server := strings.TrimSpace(obj.Spec.Server)
	mount := strings.TrimSpace(obj.Spec.Mount)
	role := strings.TrimSpace(obj.Spec.Role)
	commonName := strings.TrimSpace(obj.Spec.CommonName)
	if server == "" || mount == "" || role == "" || commonName == "" {
		setReady(&status.Conditions, generation, false, ReasonWaiting, "the VaultCertificate needs a server, a mount, a role and a commonName")
		c.write(ctx, dyn, VaultCertificateGVR, u, &status)
		return
	}
	if c.vault == nil {
		setReady(&status.Conditions, generation, false, ReasonSourceUnavailable, "no vault source is configured")
		c.write(ctx, dyn, VaultCertificateGVR, u, &status)
		return
	}
	observation, err := c.vault.CertificateMetadata(ctx, server, mount, role, commonName)
	if err != nil {
		setReady(&status.Conditions, generation, false, ReasonSourceError, "reading the certificate metadata: "+err.Error())
		c.write(ctx, dyn, VaultCertificateGVR, u, &status)
		return
	}
	status.Exists = observation.Exists
	status.SerialNumber = observation.SerialNumber
	status.Fingerprint = observation.Fingerprint
	status.ExpiresAt = observation.ExpiresAt
	status.RotationDue = observation.RotationDue
	setReady(&status.Conditions, generation, true, ReasonReady, "the certificate metadata is observed without private keys")
	c.write(ctx, dyn, VaultCertificateGVR, u, &status)
}

func (s *OpenBaoMetadataSource) CredentialMetadata(ctx context.Context, server, mount, path string) (VaultCredentialObservation, error) {
	reader, err := s.reader(ctx, server)
	if err != nil {
		return VaultCredentialObservation{}, err
	}
	metadata, err := reader.Read(ctx, metadataPath(mount, path))
	if errors.Is(err, openbao.ErrNotFound) {
		return VaultCredentialObservation{}, nil
	}
	if err != nil {
		return VaultCredentialObservation{}, err
	}
	metadata = unwrapMetadata(metadata)
	if len(metadata) == 0 {
		return VaultCredentialObservation{}, nil
	}
	if versionDeleted(metadata, int64Of(metadata["current_version"])) {
		return VaultCredentialObservation{}, nil
	}
	observation := VaultCredentialObservation{Exists: true}
	custom := mapOf(metadata["custom_metadata"])
	observation.ExpiresAt = firstString(custom, "expires_at", "expiresAt", "expiry")
	observation.RotationDue = boolOf(custom["rotation_due"]) || boolOf(custom["rotationDue"])
	if observation.ExpiresAt != "" {
		if expiry, err := time.Parse(time.RFC3339, observation.ExpiresAt); err == nil && time.Now().Add(s.rotationWindow()).After(expiry) {
			observation.RotationDue = true
		}
	}
	return observation, nil
}

func (s *OpenBaoMetadataSource) CertificateMetadata(ctx context.Context, server, mount, role, commonName string) (VaultCertificateObservation, error) {
	_ = role
	reader, err := s.reader(ctx, server)
	if err != nil {
		return VaultCertificateObservation{}, err
	}
	serials, err := reader.List(ctx, strings.Trim(mount, "/")+"/certs")
	if errors.Is(err, openbao.ErrNotFound) {
		return VaultCertificateObservation{}, nil
	}
	if err != nil {
		return VaultCertificateObservation{}, err
	}
	scanned := 0
	for _, serial := range serials {
		if scanned >= maxCertificateScan {
			break
		}
		scanned++
		entry, err := reader.Read(ctx, strings.Trim(mount, "/")+"/cert/"+url.PathEscape(serial))
		if errors.Is(err, openbao.ErrNotFound) {
			continue
		}
		if err != nil {
			return VaultCertificateObservation{}, err
		}
		entry = unwrapMetadata(entry)
		if int64Of(entry["revocation_time"]) != 0 || firstString(entry, "revocation_time_rfc3339") != "" {
			continue
		}
		certificate, err := parseCertificate(firstString(entry, "certificate"))
		if err != nil {
			continue
		}
		if certificate.Subject.CommonName != commonName {
			continue
		}
		serialNumber := firstString(entry, "serial_number")
		if serialNumber == "" {
			serialNumber = certificate.SerialNumber.Text(16)
		}
		observation := VaultCertificateObservation{
			Exists:       true,
			SerialNumber: serialNumber,
			Fingerprint:  certificateFingerprint(certificate),
			ExpiresAt:    certificate.NotAfter.UTC().Format(time.RFC3339),
			RotationDue:  time.Now().Add(s.rotationWindow()).After(certificate.NotAfter),
		}
		return observation, nil
	}
	return VaultCertificateObservation{}, nil
}

func (s *OpenBaoMetadataSource) reader(ctx context.Context, server string) (MetadataReader, error) {
	if s == nil || s.Open == nil {
		return nil, fmt.Errorf("statusstate: the OpenBao metadata source has no Open function")
	}
	return s.Open(ctx, server)
}

func (s *OpenBaoMetadataSource) rotationWindow() time.Duration {
	if s == nil || s.RotationWindow <= 0 {
		return DefaultRotationWindow
	}
	return s.RotationWindow
}

func metadataPath(mount, path string) string {
	mount = strings.Trim(mount, "/")
	path = strings.Trim(path, "/")
	if strings.HasPrefix(path, mount+"/metadata/") {
		return path
	}
	return mount + "/metadata/" + path
}

func unwrapMetadata(metadata map[string]any) map[string]any {
	if inner, ok := metadata["data"].(map[string]any); ok {
		return inner
	}
	return metadata
}

func versionDeleted(metadata map[string]any, current int64) bool {
	versions := mapOf(metadata["versions"])
	if len(versions) == 0 || current <= 0 {
		return false
	}
	entry := mapOf(versions[strconv.FormatInt(current, 10)])
	return firstString(entry, "deletion_time") != ""
}

func parseCertificate(value string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(value))
	if block == nil {
		return nil, fmt.Errorf("statusstate: the certificate is not PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}

func certificateFingerprint(certificate *x509.Certificate) string {
	sum := sha256.Sum256(certificate.Raw)
	return hex.EncodeToString(sum[:])
}

func mapOf(value any) map[string]any {
	if out, ok := value.(map[string]any); ok {
		return out
	}
	return nil
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if out := stringOf(values[key]); out != "" {
			return out
		}
	}
	return ""
}

func stringOf(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	default:
		return ""
	}
}

func boolOf(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		out, err := strconv.ParseBool(strings.TrimSpace(typed))
		return err == nil && out
	default:
		return false
	}
}

func int64Of(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	case string:
		out, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err == nil {
			return out
		}
	}
	return 0
}
