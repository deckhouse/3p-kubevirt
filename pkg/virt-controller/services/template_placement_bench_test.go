package services

import (
	"testing"

	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"

	"kubevirt.io/kubevirt/pkg/testutils"
)

// The search for a migration target and the NodePlacement condition need the placement rules of the
// virt-launcher pod and nothing else, while they used to render the whole manifest. Both run on every
// reconcile of every running VirtualMachineInstance, so the two are measured side by side.

func benchVMI() *v1.VirtualMachineInstance {
	return &v1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "benchvmi", Namespace: k8sv1.NamespaceDefault, UID: "bench-uid"},
		Spec: v1.VirtualMachineInstanceSpec{
			Domain: v1.DomainSpec{
				Devices: v1.Devices{
					Disks: []v1.Disk{{Name: "root"}},
				},
			},
			Volumes: []v1.Volume{{
				Name: "root",
				VolumeSource: v1.VolumeSource{
					ContainerDisk: &v1.ContainerDiskSource{Image: "registry.example/disk:latest"},
				},
			}},
			Tolerations: []k8sv1.Toleration{{
				Key:      "dedicated",
				Operator: k8sv1.TolerationOpEqual,
				Value:    "virtualization",
				Effect:   k8sv1.TaintEffectNoSchedule,
			}},
			Affinity: &k8sv1.Affinity{
				NodeAffinity: &k8sv1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &k8sv1.NodeSelector{
						NodeSelectorTerms: []k8sv1.NodeSelectorTerm{{
							MatchExpressions: []k8sv1.NodeSelectorRequirement{{
								Key:      "topology.example/zone",
								Operator: k8sv1.NodeSelectorOpIn,
								Values:   []string{"zone-0", "zone-1"},
							}},
						}},
					},
				},
			},
		},
	}
}

func benchTemplateService(b *testing.B) TemplateService {
	b.Helper()

	config, _, _ := testutils.NewFakeClusterConfigUsingKVConfig(&v1.KubeVirtConfiguration{})
	virtClient := kubecli.NewMockKubevirtClient(nil)

	return NewTemplateService("kubevirt/virt-launcher",
		240,
		"/var/run/kubevirt",
		"/var/run/kubevirt-ephemeral-disks",
		"/var/run/kubevirt/container-disks",
		v1.HotplugDiskDir,
		"pull-secret-1",
		cache.NewIndexer(cache.DeletionHandlingMetaNamespaceKeyFunc, cache.Indexers{}),
		virtClient,
		config,
		107,
		"kubevirt/vmexport",
		cache.NewIndexer(cache.DeletionHandlingMetaNamespaceKeyFunc, cache.Indexers{}),
		cache.NewIndexer(cache.DeletionHandlingMetaNamespaceKeyFunc, cache.Indexers{}),
	)
}

func BenchmarkRenderLaunchManifest(b *testing.B) {
	svc := benchTemplateService(b)
	vmi := benchVMI()

	b.ReportAllocs()
	for b.Loop() {
		if _, err := svc.RenderLaunchManifest(vmi); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRenderPodPlacement(b *testing.B) {
	config, _, _ := testutils.NewFakeClusterConfigUsingKVConfig(&v1.KubeVirtConfiguration{})
	vmi := benchVMI()

	b.ReportAllocs()
	for b.Loop() {
		if pod := RenderPodPlacement(config, vmi); pod == nil {
			b.Fatal("no placement rendered")
		}
	}
}
