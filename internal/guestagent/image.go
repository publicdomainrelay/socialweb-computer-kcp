package guestagent

import (
	"bufio"
	"context"
	"os"
	"strings"
)

var imageMarkerPath = "/etc/swc-image"

type imageRecord struct {
	Type string `json:"$type"`

	Marker string `json:"marker"`

	CreatedAt string `json:"createdAt"`
}

func (a *Agent) reportImage(ctx context.Context) {
	f, err := os.Open(imageMarkerPath)
	if err != nil {
		return
	}
	defer f.Close()
	line := bufio.NewScanner(f)
	line.Scan()
	marker := strings.TrimSpace(line.Text())
	if marker == "" {
		a.log.Warn("the image marker file is empty; no guest.image event", "path", imageMarkerPath)
		return
	}
	a.log.Info("the guest reports its image", "marker", marker)
	if err := a.poster.Post(ctx, ImageNSID, "image", imageRecord{Type: ImageNSID, Marker: marker, CreatedAt: createdAt()}); err != nil && ctx.Err() == nil {
		a.log.Warn("posting the guest.image event failed", "err", err.Error())
	}
}
