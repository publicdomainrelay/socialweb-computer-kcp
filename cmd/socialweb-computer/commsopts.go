package main

import (
	"fmt"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

func applyCommsOption(comms *v1alpha1.MarketComms, name, value string) (*v1alpha1.MarketComms, bool, error) {
	switch name {
	case "comms-mode", "comms-relay-url", "comms-firehose-url":
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
	case "comms-relay-url":
		comms.RelayUrls = splitList(value)
	case "comms-firehose-url":
		comms.FirehoseUrls = splitList(value)
	}
	return comms, true, nil
}
