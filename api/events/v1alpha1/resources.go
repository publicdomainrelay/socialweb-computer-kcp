package v1alpha1

import (
	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	corev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type PrinterColumn = corev1alpha1.PrinterColumn

type APIResource = corev1alpha1.APIResource

var APIResources = []APIResource{
	{Kind: "EventStream", Plural: "eventstreams", Revision: 7, Export: "events", Object: EventStream{}, PrinterColumns: []PrinterColumn{
		{Name: "Source", Type: "string", JSONPath: ".spec.sourceRef.name"},
		{Name: "Sink", Type: "string", JSONPath: ".spec.sink.kind"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Cursor", Type: "integer", JSONPath: ".status.cursor"},
		{Name: "Lag", Type: "integer", JSONPath: ".status.lag"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "EventSource", Plural: "eventsources", Revision: 1, Export: "events", Object: EventSource{}, PrinterColumns: []PrinterColumn{
		{Name: "Backend", Type: "string", JSONPath: ".spec.backendRef.kind"},
		{Name: "Cursor", Type: "integer", JSONPath: ".status.cursor"},
		{Name: "Lag", Type: "integer", JSONPath: ".status.lag"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "PdsEventSource", Plural: "pdseventsources", Revision: 1, Export: "events", Object: PdsEventSource{}, Contracts: []string{commonv1.ContractEventSources}, PrinterColumns: []PrinterColumn{
		{Name: "Repo", Type: "string", JSONPath: ".spec.repoRef.name"},
		{Name: "Cursor", Type: "integer", JSONPath: ".status.cursor"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "RelayEventSource", Plural: "relayeventsources", Revision: 1, Export: "events", Object: RelayEventSource{}, Contracts: []string{commonv1.ContractEventSources}, PrinterColumns: []PrinterColumn{
		{Name: "Relay", Type: "string", JSONPath: ".spec.relayRef.name"},
		{Name: "URL", Type: "string", JSONPath: ".spec.url"},
		{Name: "Cursor", Type: "integer", JSONPath: ".status.cursor"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "JetstreamEventSource", Plural: "jetstreameventsources", Revision: 1, Export: "events", Object: JetstreamEventSource{}, Contracts: []string{commonv1.ContractEventSources}, PrinterColumns: []PrinterColumn{
		{Name: "URL", Type: "string", JSONPath: ".spec.url"},
		{Name: "Cursor", Type: "integer", JSONPath: ".status.cursor"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
}
