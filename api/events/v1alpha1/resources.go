package v1alpha1

import (
	corev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type PrinterColumn = corev1alpha1.PrinterColumn

type APIResource = corev1alpha1.APIResource

var APIResources = []APIResource{
	{Kind: "EventStream", Plural: "eventstreams", Revision: 3, Export: "events", Object: EventStream{}, PrinterColumns: []PrinterColumn{
		{Name: "Source", Type: "string", JSONPath: ".spec.source.kind"},
		{Name: "Sink", Type: "string", JSONPath: ".spec.sink.kind"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Cursor", Type: "integer", JSONPath: ".status.cursor"},
		{Name: "Lag", Type: "integer", JSONPath: ".status.lag"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
}
