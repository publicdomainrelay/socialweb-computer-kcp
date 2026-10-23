package conds

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Object interface {
	GetGeneration() int64
	GetConditions() []metav1.Condition
	SetConditions([]metav1.Condition)
}

func Status(ok bool) metav1.ConditionStatus {
	if ok {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func New(kind string, ok bool, reason, message string, generation int64) metav1.Condition {
	return metav1.Condition{Type: kind, Status: Status(ok), Reason: reason, Message: message, ObservedGeneration: generation}
}

func Set(conditions *[]metav1.Condition, generation int64, kind string, ok bool, reason, message string) {
	SetStatus(conditions, generation, kind, Status(ok), reason, message)
}

func SetStatus(conditions *[]metav1.Condition, generation int64, kind string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(conditions, metav1.Condition{Type: kind, Status: status, Reason: reason, Message: message,
		ObservedGeneration: generation})
}

func SetStatusAt(conditions *[]metav1.Condition, generation int64, kind string, status metav1.ConditionStatus, reason, message string, now time.Time) {
	meta.SetStatusCondition(conditions, metav1.Condition{Type: kind, Status: status, Reason: reason, Message: message,
		ObservedGeneration: generation, LastTransitionTime: metav1.NewTime(now)})
}

func SetOn(obj Object, kind string, ok bool, reason, message string) {
	conditions := obj.GetConditions()
	Set(&conditions, obj.GetGeneration(), kind, ok, reason, message)
	obj.SetConditions(conditions)
}
