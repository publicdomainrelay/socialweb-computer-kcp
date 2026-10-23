package main

import (
	"fmt"
	"strings"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

func applyCommsOption(comms *v1alpha1.MarketComms, name, value string) (*v1alpha1.MarketComms, bool, error) {
	switch name {
	case "comms-mode", "comms-event-source":
	default:
		return comms, false, nil
	}
	if comms == nil {
		comms = &v1alpha1.MarketComms{}
	}
	switch name {
	case "comms-mode":
		mode := v1alpha1.MarketCommsMode(strings.TrimSpace(value))
		switch mode {
		case v1alpha1.MarketCommsModeXRPC, v1alpha1.MarketCommsModeFirehose, v1alpha1.MarketCommsModeBoth:
		default:
			return comms, true, fmt.Errorf("--comms-mode %s: want XRPC, Firehose or Both", value)
		}
		comms.Mode = mode
	case "comms-event-source":
		for _, name := range splitList(value) {
			comms.EventSourceRefs = append(comms.EventSourceRefs, commonv1.ObjectRef{Group: eventsv1alpha1.GroupName, Kind: eventsv1alpha1.EventSourceKind, Name: name})
		}
	}
	return comms, true, nil
}
