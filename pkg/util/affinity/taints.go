/*
Copyright The KubeVirt Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package affinity

import (
	v1 "k8s.io/api/core/v1"
)

// ToleratesNodeTaints reports whether the tolerations of the pod tolerate all the taints of the
// node which prevent the pod from being scheduled to it.
func ToleratesNodeTaints(node *v1.Node, pod *v1.Pod) bool {
	if node == nil {
		return false
	}
	return ToleratesTaints(node.Spec.Taints, pod)
}

// ToleratesTaints reports whether the tolerations of the pod tolerate all the given taints which
// prevent the pod from being scheduled.
func ToleratesTaints(taints []v1.Taint, pod *v1.Pod) bool {
	if pod == nil {
		return false
	}

	for i := range taints {
		taint := &taints[i]
		if taint.Effect != v1.TaintEffectNoSchedule && taint.Effect != v1.TaintEffectNoExecute {
			continue
		}
		if !tolerationsTolerateTaint(pod.Spec.Tolerations, taint) {
			return false
		}
	}
	return true
}

func tolerationsTolerateTaint(tolerations []v1.Toleration, taint *v1.Taint) bool {
	for i := range tolerations {
		if tolerations[i].ToleratesTaint(taint) {
			return true
		}
	}
	return false
}
