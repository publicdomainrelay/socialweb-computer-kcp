package childapplication

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	NetworkLabel = "childapplication.socialweb.computer/network"

	AppLabel = "childapplication.socialweb.computer/app"

	childNameMax = 63

	childHashLen = 8
)

func Children(network *v1alpha1.MarketNetwork) []*v1alpha1.ChildApplication {
	if network == nil {
		return nil
	}
	profiles := map[string]string{}
	for _, app := range network.Spec.Apps {
		profiles[app.Name] = app.Profile
	}
	owner := metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: "MarketNetwork", Name: network.Name, UID: network.UID}
	seen := map[string]bool{}
	var out []*v1alpha1.ChildApplication
	for _, app := range network.Status.Apps {
		if app.Name == "" || seen[app.Name] {
			continue
		}
		seen[app.Name] = true
		child := &v1alpha1.ChildApplication{
			ObjectMeta: metav1.ObjectMeta{Name: ChildName(network.Name, app.Name), Labels: map[string]string{NetworkLabel: network.Name, AppLabel: app.Name}},
			Spec:       v1alpha1.ChildApplicationSpec{NetworkRef: commonv1.LocalObjectRef{Name: network.Name}, App: app.Name, Profile: profiles[app.Name]},
		}
		if network.UID != "" {
			child.OwnerReferences = []metav1.OwnerReference{owner}
		}
		out = append(out, child)
	}
	return out
}

func ChildName(network, app string) string {
	sum := sha256.Sum256([]byte(network + "\x00" + app))
	suffix := hex.EncodeToString(sum[:])[:childHashLen]
	base := sanitizeName(network + "-" + app)
	limit := childNameMax - childHashLen - 1
	if len(base) > limit {
		base = strings.Trim(base[:limit], "-")
	}
	base = cmp.Or(base, "child")
	return base + "-" + suffix
}

func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
