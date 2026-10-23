package ttlctl

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ConditionDone   = "Complete"
	ConditionFailed = "Failed"
)

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

func Expiry(ttlSeconds *int32, conditions []metav1.Condition) (time.Time, bool) {
	if ttlSeconds == nil || *ttlSeconds < 0 {
		return time.Time{}, false
	}
	done, finished := FinishedAt(conditions)
	if !finished {
		return time.Time{}, false
	}
	return done.Add(time.Duration(*ttlSeconds) * time.Second), true
}
