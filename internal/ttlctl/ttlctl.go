package ttlctl

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	Annotation      = v1alpha1.AnnotationTTLAfterFinished
	ConditionDone   = "Complete"
	ConditionFailed = "Failed"
)

func ParseTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("ttlctl: %q is not a duration", s)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("ttlctl: %q is not a duration", s)
	}
	return d, nil
}

func FinishedAt(conditions []metav1.Condition) (time.Time, bool) {
	var at time.Time
	found := false
	for _, c := range conditions {
		if c.Type != ConditionDone && c.Type != ConditionFailed {
			continue
		}
		if c.Status != metav1.ConditionTrue || c.LastTransitionTime.IsZero() {
			continue
		}
		if when := c.LastTransitionTime.Time; !found || when.After(at) {
			at, found = when, true
		}
	}
	return at, found
}

func Expiry(annotations map[string]string, conditions []metav1.Condition) (time.Time, bool, error) {
	raw, ok := annotations[Annotation]
	if !ok {
		return time.Time{}, false, nil
	}
	ttl, err := ParseTTL(raw)
	if err != nil {
		return time.Time{}, false, err
	}
	done, finished := FinishedAt(conditions)
	if !finished {
		return time.Time{}, false, nil
	}
	return done.Add(ttl), true, nil
}
