package vmi

import (
	"fmt"
	"testing"

	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	virtv1 "kubevirt.io/api/core/v1"

	kvcontroller "kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/testutils"
)

// BenchmarkFindMigrationTarget measures the search for a migration target, which runs on every
// reconcile of every running VirtualMachineInstance and once more for each of them on every event of
// a node.
func BenchmarkFindMigrationTarget(b *testing.B) {
	const nodeCount = 100

	clusterConfig, _, _ := testutils.NewFakeClusterConfigUsingKVConfig(&virtv1.KubeVirtConfiguration{})

	nodeInformer, _ := testutils.NewFakeInformerFor(&k8sv1.Node{})
	nodeIndexer := nodeInformer.GetIndexer()
	for i := range nodeCount {
		name := fmt.Sprintf("node-%03d", i)
		if err := nodeIndexer.Add(&k8sv1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					virtv1.NodeSchedulable:  "true",
					k8sv1.LabelHostname:     name,
					"topology.example/zone": fmt.Sprintf("zone-%d", i%3),
				},
			},
		}); err != nil {
			b.Fatal(err)
		}
	}

	vmInformer, _ := testutils.NewFakeInformerWithIndexersFor(
		&virtv1.VirtualMachine{}, kvcontroller.GetVirtualMachineInformerIndexers())

	vmi := &virtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "benchvmi", Namespace: k8sv1.NamespaceDefault},
		Spec: virtv1.VirtualMachineInstanceSpec{
			Domain: virtv1.DomainSpec{},
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
		Status: virtv1.VirtualMachineInstanceStatus{
			Phase:    virtv1.Running,
			NodeName: "node-000",
		},
	}

	c := &Controller{
		clusterConfig: clusterConfig,
		nodeIndexer:   nodeIndexer,
		vmStore:       vmInformer.GetStore(),
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := c.findMigrationTarget(vmi); err != nil {
			b.Fatal(err)
		}
	}
}
