package computevm

import (
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"
)

func GuestReportOf(o *v1alpha1.ComputeVM) *v1alpha1.ComputeVMGuestReport {
	return guestidentity.GuestReportOf(o)
}

func PublishedOf(o *v1alpha1.ComputeVM) *v1alpha1.ComputeVMPublished {
	if o == nil || o.Status.Market == nil {
		return nil
	}
	return o.Status.Market.Published
}

func CheckHostKeys(keys []v1alpha1.ComputeVMHostKey) ([]v1alpha1.ComputeVMHostKey, error) {
	return guestidentity.CheckHostKeys(keys)
}
