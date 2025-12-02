package vmi

import (
	k8sv1 "k8s.io/api/core/v1"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/virt-controller/watch/common"
)

func needsHandleResourceClaimHotplug(hotplugResourceClaims []*v1.ResourceClaim, hotplugAttachmentPods []*k8sv1.Pod) bool {
	if len(hotplugAttachmentPods) > 1 {
		return true
	}
	// Determine if the ready volumes have changed compared to the current pod
	if len(hotplugAttachmentPods) == 1 && podResourceClaimsMatchesReadyResourceClaims(hotplugAttachmentPods[0], hotplugResourceClaims) {
		return false
	}

	return len(hotplugResourceClaims) > 0 || len(hotplugAttachmentPods) > 0
}

func getActiveAndOldAttachmentPodsForResourceClaims(hotplugResourceClaims []*v1.ResourceClaim, hotplugAttachmentPods []*k8sv1.Pod) (*k8sv1.Pod, []*k8sv1.Pod) {
	return getActiveAndOldAttachmentPods(hotplugAttachmentPods, func(attachmentPod *k8sv1.Pod) bool {
		return podResourceClaimsMatchesReadyResourceClaims(attachmentPod, hotplugResourceClaims)
	})
}

func podResourceClaimsMatchesReadyResourceClaims(attachmentPod *k8sv1.Pod, hotplugResourceClaims []*v1.ResourceClaim) bool {
	resourceClaimMap := make(map[string]struct{})
	for _, resourceClaim := range attachmentPod.Spec.ResourceClaims {
		resourceClaimMap[resourceClaim.Name] = struct{}{}
	}

	for _, resourceClaim := range hotplugResourceClaims {
		delete(resourceClaimMap, resourceClaim.Name)
	}

	return len(resourceClaimMap) == 0
}

func findAttachmentPodByResourceClaimName(resourceClaimName string, attachmentPods []*k8sv1.Pod) *k8sv1.Pod {
	for _, pod := range attachmentPods {
		for _, podResourceClaim := range pod.Spec.ResourceClaims {
			if podResourceClaim.Name == resourceClaimName {
				return pod
			}
		}
	}
	return nil
}

// TODO: check ready claims
func (c *Controller) getReadyHotplugResourceClaims(resourceClaims []*v1.ResourceClaim, _ *v1.VirtualMachineInstance, _ *k8sv1.Pod) ([]*v1.ResourceClaim, common.SyncError) {
	return resourceClaims, nil
}
