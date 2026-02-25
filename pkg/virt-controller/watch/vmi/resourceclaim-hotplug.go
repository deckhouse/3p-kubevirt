package vmi

import (
	k8sv1 "k8s.io/api/core/v1"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/virt-controller/watch/common"
)

func needsHandleResourceClaimHotplug(hotplugResourceClaims []*v1.VirtualMachineInstanceResourceClaim, hotplugAttachmentPods []*k8sv1.Pod) bool {
	if len(hotplugAttachmentPods) > 1 {
		return true
	}
	// Determine if the ready volumes have changed compared to the current pod
	if len(hotplugAttachmentPods) == 1 && podResourceClaimsMatchesReadyResourceClaims(hotplugAttachmentPods[0], hotplugResourceClaims) {
		return false
	}

	return len(hotplugResourceClaims) > 0 || len(hotplugAttachmentPods) > 0
}

func GetActiveAndOldAttachmentPodsForResourceClaims(hotplugResourceClaims []*v1.VirtualMachineInstanceResourceClaim, hotplugAttachmentPods []*k8sv1.Pod) (*k8sv1.Pod, []*k8sv1.Pod) {
	return getActiveAndOldAttachmentPods(hotplugAttachmentPods, func(attachmentPod *k8sv1.Pod) bool {
		return podResourceClaimsMatchesReadyResourceClaims(attachmentPod, hotplugResourceClaims)
	})
}

func podResourceClaimsMatchesReadyResourceClaims(attachmentPod *k8sv1.Pod, hotplugResourceClaims []*v1.VirtualMachineInstanceResourceClaim) bool {
	if len(attachmentPod.Spec.ResourceClaims) != len(hotplugResourceClaims) {
		return false
	}

	resourceClaimMap := make(map[string]struct{})
	for _, resourceClaim := range attachmentPod.Spec.ResourceClaims {
		resourceClaimMap[resourceClaim.Name] = struct{}{}
	}

	for _, resourceClaim := range hotplugResourceClaims {
		delete(resourceClaimMap, resourceClaim.Name)
	}

	return len(resourceClaimMap) == 0
}

// For now, we consider all resource claims to be ready immediately, but this may change in the future
func (c *Controller) getReadyHotplugResourceClaims(resourceClaims []*v1.VirtualMachineInstanceResourceClaim, _ *v1.VirtualMachineInstance, _ *k8sv1.Pod) ([]*v1.VirtualMachineInstanceResourceClaim, common.SyncError) {
	return resourceClaims, nil
}

func deviceReadyForPodDelete(phase v1.DevicePhase) bool {
	switch phase {
	case v1.DeviceReady, v1.DeviceAttachedToPod:
		return false
	}
	return true
}

// podContainsResourceClaimsToPreserve is the resource claim counterpart of podContainsVolumesToPreserve:
// it returns true if the pod holds a hotplug claim that is still in the VMI spec, or one that is being
// detached but whose device phase still blocks pod deletion.
func podContainsResourceClaimsToPreserve(pod *k8sv1.Pod, statusMap map[string]v1.DeviceStatusInfo, specHotplugResourceClaims map[string]struct{}) bool {
	for _, podClaim := range pod.Spec.ResourceClaims {
		if _, inSpec := specHotplugResourceClaims[podClaim.Name]; inSpec {
			return true
		}
		if ds, ok := statusMap[podClaim.Name]; ok && !deviceReadyForPodDelete(ds.Phase) {
			return true
		}
	}
	return false
}
