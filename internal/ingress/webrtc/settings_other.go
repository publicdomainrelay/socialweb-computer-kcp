//go:build !js

package webrtc

import (
	pion "github.com/pion/webrtc/v4"
)

func tuneSettings(settings *pion.SettingEngine) {
	settings.SetNetworkTypes([]pion.NetworkType{pion.NetworkTypeUDP4, pion.NetworkTypeUDP6})
	settings.SetSTUNGatherTimeout(stunGatherTimeout)
}
