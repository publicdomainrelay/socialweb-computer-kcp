package market

import (
	"fmt"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

func ValidateComms(prefix string, comms *v1alpha1.MarketComms) error {
	if comms == nil || strings.TrimSpace(string(comms.Mode)) == "" {
		return nil
	}
	switch comms.Mode {
	case v1alpha1.MarketCommsModeXRPC, v1alpha1.MarketCommsModeFirehose, v1alpha1.MarketCommsModeBoth:
		return nil
	default:
		return fmt.Errorf("%s: unknown comms mode %q", prefix, comms.Mode)
	}
}
