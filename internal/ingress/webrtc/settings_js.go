//go:build js

package webrtc

import (
	pion "github.com/pion/webrtc/v4"
)

// ponytail: pion's js build has no SetNetworkTypes or SetSTUNGatherTimeout; the browser gathers
// whatever the browser allows, so the UDP-only restriction and the gather timeout do not apply there.
func tuneSettings(*pion.SettingEngine) {}
