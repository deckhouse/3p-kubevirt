package topology

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	g "github.com/onsi/gomega"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"kubevirt.io/client-go/kubecli"
)

var _ = Describe("Nodetopologyupdater", func() {

	var topologyUpdater *nodeTopologyUpdater
	var ctrl *gomock.Controller
	var hinter *MockHinter
	var virtClient *kubecli.MockKubevirtClient
	var kubeClient *fake.Clientset

	BeforeEach(func() {
		ctrl = gomock.NewController(GinkgoT())
		hinter = NewMockHinter(ctrl)
		virtClient = kubecli.NewMockKubevirtClient(ctrl)
		topologyUpdater = &nodeTopologyUpdater{
			hinter: hinter,
			client: virtClient,
		}
		kubeClient = fake.NewSimpleClientset()
		virtClient.EXPECT().CoreV1().Return(kubeClient.CoreV1()).AnyTimes()
	})

	Context("with no VMs with TSC frequency set running", func() {

		BeforeEach(func() {
			hinter.EXPECT().TSCFrequenciesInUse().Return(nil)
		})

		It("should add the node's own frequency to a node", func() {
			nodes := []*v1.Node{NodeWithTSC("mynode", 123, true)}
			trackNodes(kubeClient, nodes...)
			stats := topologyUpdater.sync(nodes)
			expectUpdates(stats, 0, 0, 1)
			node, err := kubeClient.CoreV1().Nodes().Get(context.Background(), "mynode", metav1.GetOptions{})
			g.Expect(err).ToNot(g.HaveOccurred())
			g.Expect(node.Labels).To(g.HaveKeyWithValue(ToTSCSchedulableLabel(123), "true"))
		})

		It("should continue if it encounters invalid nodes", func() {
			nodes := []*v1.Node{
				NodeWithTSC("mynode1", 123, true),
				NodeWithTSC("syncednode", 123, true, 123),
				NodeWithInvalidTSC("invalid"),
				NodeWithTSC("mynode2", 123, true),
			}
			trackNodes(kubeClient, nodes...)
			stats := topologyUpdater.sync(nodes)
			expectUpdates(stats, 1, 1, 2)
		})

		It("should do nothing if all frequencies are already present", func() {
			nodes := []*v1.Node{NodeWithTSC("mynode", 123, true, 123)}
			stats := topologyUpdater.sync(nodes)
			expectUpdates(stats, 0, 1, 0)
		})

		It("should remove inappropriate labels", func() {
			nodes := []*v1.Node{NodeWithTSC("mynode", 123, true, 99, 200, 123)}
			trackNodes(kubeClient, nodes...)
			stats := topologyUpdater.sync(nodes)
			expectUpdates(stats, 0, 0, 1)
			node, err := kubeClient.CoreV1().Nodes().Get(context.Background(), "mynode", metav1.GetOptions{})
			g.Expect(err).ToNot(g.HaveOccurred())
			g.Expect(node.Labels).To(g.HaveKeyWithValue(ToTSCSchedulableLabel(123), "true"))
			g.Expect(node.Labels).ToNot(g.HaveKeyWithValue(ToTSCSchedulableLabel(99), "true"))
			g.Expect(node.Labels).ToNot(g.HaveKeyWithValue(ToTSCSchedulableLabel(200), "true"))
		})

	})

	Context("with repeated labels", func() {
		BeforeEach(func() {
			hinter.EXPECT().TSCFrequenciesInUse().Return([]int64{80, 80, 80, 60})
		})

		It("should do nothing if all frequencies are already present", func() {
			nodes := []*v1.Node{NodeWithTSC("mynode", 123, true, 123, 80, 60)}
			stats := topologyUpdater.sync(nodes)
			expectUpdates(stats, 0, 1, 0)
		})
	})

	Context("with VMs with TSC frequency running", func() {
		BeforeEach(func() {
			hinter.EXPECT().TSCFrequenciesInUse().Return([]int64{99, 101})
		})

		It("should keep frequencies still used by VMs", func() {
			nodes := []*v1.Node{NodeWithTSC("mynode", 123, true, 98, 99, 101, 200, 123)}
			trackNodes(kubeClient, nodes...)
			stats := topologyUpdater.sync(nodes)
			expectUpdates(stats, 0, 0, 1)
			node, err := kubeClient.CoreV1().Nodes().Get(context.Background(), "mynode", metav1.GetOptions{})
			g.Expect(err).ToNot(g.HaveOccurred())
			g.Expect(node.Labels).To(g.HaveKeyWithValue(ToTSCSchedulableLabel(123), "true"))
			g.Expect(node.Labels).ToNot(g.HaveKeyWithValue(ToTSCSchedulableLabel(98), "true"))
			g.Expect(node.Labels).To(g.HaveKeyWithValue(ToTSCSchedulableLabel(99), "true"))
			g.Expect(node.Labels).To(g.HaveKeyWithValue(ToTSCSchedulableLabel(101), "true"))
			g.Expect(node.Labels).ToNot(g.HaveKeyWithValue(ToTSCSchedulableLabel(200), "true"))
		})
	})
})

func trackNodes(clientset *fake.Clientset, nodes ...*v1.Node) {
	for i := range nodes {
		g.ExpectWithOffset(1, clientset.Tracker().Add(nodes[i])).To(g.Succeed())
	}
}

func expectUpdates(stats *updateStats, errors int, skipped int, updated int) {
	g.ExpectWithOffset(1, stats.error).To(g.Equal(errors), "errors")
	g.ExpectWithOffset(1, stats.skipped).To(g.Equal(skipped), "skipped")
	g.ExpectWithOffset(1, stats.updated).To(g.Equal(updated), "updated")
}
