//go:build js

package oauthclient

func SetTokenRefresher(refresher TokenRefresher) {
	if refresher == nil {
		defaultRefresher.Store(nil)
		return
	}
	defaultRefresher.Store(&refresherSlot{refresher: refresher})
}
