package assets

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/versions"
)

// BuiltSuffix names the build record that scripts/stage-assets.sh writes beside the blob of an asset built from this
// repository's own commit (fetch kind go or script): blobs/<goos>_<goarch>/<asset>.built.json. versions.json never
// pins the output hashes of such an asset (a change to the Go code under internal/ would re-pin it on every commit);
// the build that produced the blob records them, the record is embedded with the blob in the release binary, and the
// binary checks the unpacked tree against it before every use. Third-party assets keep their versions.json pins.
const BuiltSuffix = ".built.json"

type Built struct {
	Asset        string `json:"asset"`
	Platform     string `json:"platform"`
	TreeSHA256   string `json:"treeSha256"`
	RawBytes     int64  `json:"rawBytes"`
	ZstdBytes    int64  `json:"zstdBytes"`
	BinarySHA256 string `json:"binarySha256,omitempty"`
}

func BuiltName(asset string) string { return asset + BuiltSuffix }

func ParseBuilt(b []byte, asset, platform string) (Built, error) {
	var r Built
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Built{}, fmt.Errorf("assets: the build record of %s is not valid: %w", asset, err)
	}
	if r.Asset != asset || r.Platform != platform {
		return Built{}, fmt.Errorf("assets: the build record beside the blob of %s on %s is for %s on %s", asset, platform, r.Asset, r.Platform)
	}
	if !digestName.MatchString(r.TreeSHA256) {
		return Built{}, fmt.Errorf("assets: the build record of %s on %s has no tree sha256", asset, platform)
	}
	if r.BinarySHA256 != "" && !digestName.MatchString(r.BinarySHA256) {
		return Built{}, fmt.Errorf("assets: the build record of %s on %s has a malformed binary sha256", asset, platform)
	}
	return r, nil
}

// PlatformPins is versions.PlatformPins with the tree digest, sizes of every built asset filled in from the build
// record embedded beside its blob. Without a record (a build that embeds no blobs, or a platform other than the one
// this binary embeds) a built asset's TreeSHA256 stays empty; a release build refuses to start in that case (Ready).
func PlatformPins(platform string) ([]versions.Pin, error) {
	pins, err := versions.PlatformPins(platform)
	if err != nil {
		return nil, err
	}
	if platform != Platform() {
		return pins, nil
	}
	for i, p := range pins {
		if !p.Built {
			continue
		}
		rec, ok, err := embeddedBuilt(p.Name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		pins[i].TreeSHA256, pins[i].RawBytes, pins[i].ZstdBytes = rec.TreeSHA256, rec.RawBytes, rec.ZstdBytes
	}
	return pins, nil
}

// BuiltBinarySHA256 is the sha256 of the primary binary of a built asset, from the build record that came with this
// binary (the sha256 an OpenBao plugin catalog registers); false when this build embeds no record for it.
func BuiltBinarySHA256(asset string) (string, bool) {
	rec, ok, err := embeddedBuilt(asset)
	if err != nil || !ok || rec.BinarySHA256 == "" {
		return "", false
	}
	return rec.BinarySHA256, true
}

func IsBuilt(asset string) (bool, error) {
	all, err := versions.Assets()
	if err != nil {
		return false, err
	}
	a, ok := all[asset]
	if !ok {
		return false, fmt.Errorf("assets: no asset %s", asset)
	}
	return a.Built(), nil
}
