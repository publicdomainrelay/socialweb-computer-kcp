package requester

import (
	"context"
	"errors"
	"fmt"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestimage"
)

var errGuestImage = errors.New("requester: the guest did not boot the requested image")

func contractImage(t v1alpha1.ComputeContractTemplate) *guestimage.Requested {
	if t.Image == nil {
		return nil
	}
	return &guestimage.Requested{URL: t.Image.URL, SHA256: t.Image.Sha256}
}

func payloadImage(t v1alpha1.ComputeContractTemplate) *market.ComputeVMImage {
	if t.Image == nil {
		return nil
	}
	return &market.ComputeVMImage{URL: t.Image.URL, Sha256: t.Image.Sha256}
}

func checkImage(t v1alpha1.ComputeContractTemplate) error {
	req := contractImage(t)
	if req == nil {
		return nil
	}
	if t.ComputeKind == v1alpha1.ComputeKindDeno {
		return errors.New("image is a computeKind vm option")
	}
	return req.Valid()
}

func (r *contractRunner) expectedImageMarker(ctx context.Context) (string, error) {
	req := contractImage(r.snapshot().Spec.ComputeContractTemplate)
	if req == nil {
		return "", nil
	}
	m, _, err := guestimage.FetchManifest(ctx, guestimage.PublicClient(), *req)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errGuestImage, err)
	}
	if m.Marker == "" {
		return "", fmt.Errorf("%w: its image.json names no marker to compare", errGuestImage)
	}
	return m.Marker, nil
}

func (r *contractRunner) guestImage(state *v1alpha1.NodeClaimContract, parsed *GuestEvent, expected string) error {
	ok := expected == "" || parsed.ImageMarker == expected
	r.log().Info("requester_guest_image", "marker", parsed.ImageMarker, "expected", expected, "ok", ok)
	if !ok {
		return fmt.Errorf("%w: the guest reports marker %q and the image.json names %q", errGuestImage, parsed.ImageMarker, expected)
	}
	state.GuestImageMarker = parsed.ImageMarker
	return nil
}

func (r *contractRunner) releaseWrongImage(ctx context.Context, state *v1alpha1.NodeClaimContract, bidderDID string, cause error) error {
	r.log().Warn("requester_guest_image_release", "err", cause.Error())
	if err := r.releaseContract(ctx, state, bidderDID); err != nil {
		return err
	}
	r.writeStatus(ctx, v1alpha1.ComputeContractFailed, "released: "+cause.Error(), state, r.currentBids(), r.currentChosen(), "imageMismatch", 1)
	return nil
}
