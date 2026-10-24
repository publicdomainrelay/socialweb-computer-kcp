package gatewayhttp

import (
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway/nodeclaim"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/pinneddownloads"
)

const (
	MeshdDownloadPrefix = "socialweb-meshd-"

	NodeAgentDownloadPrefix = "socialweb-node-"
)

func MeshdPinsOf(downloads []pinneddownloads.PinnedDownload) map[string]string {
	return pinsOf(downloads, MeshdDownloadPrefix)
}

func NodeAgentPinsOf(downloads []pinneddownloads.PinnedDownload) map[string]string {
	return pinsOf(downloads, NodeAgentDownloadPrefix)
}

func pinsOf(downloads []pinneddownloads.PinnedDownload, prefix string) map[string]string {
	sha := map[string]string{}
	for _, arch := range nodeclaim.Arches {
		found := false
		for _, d := range downloads {
			if d.Name == prefix+arch {
				sha[arch] = d.Sha256
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}
	return sha
}
