package checksum_controller

import (
	"fmt"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe("Controller", func() {
	It("enqueues all objects concurrently with map mutations without a data race", func() {
		c := NewController(nil, nil)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				key := types.NamespacedName{Namespace: "default", Name: fmt.Sprintf("vmi-%d", i)}
				c.Set(VMIControl{NamespacedName: key})
				c.delete(key)
			}
		}()

		for i := 0; i < 1000; i++ {
			c.enqueueAll()
		}
		wg.Wait()
	})
})
