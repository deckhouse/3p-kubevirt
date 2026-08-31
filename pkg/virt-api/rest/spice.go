package rest

import (
	restful "github.com/emicklei/go-restful/v3"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
)

// SPICERequestHandler opens a websocket connection to the SPICE display of a VM.
//
// Unlike VNC, a client establishes several such connections per session, one per SPICE
// channel (main, display, inputs, cursor, audio, usbredir). Every request is proxied
// independently here; grouping the connections into sessions, and evicting the client
// that held the display before, happens in virt-handler where the link message is read.
func (app *SubresourceAPIApp) SPICERequestHandler(request *restful.Request, response *restful.Response) {
	// No active-connection metric on purpose: for VNC such a counter equals sessions,
	// while for SPICE it does not and would mislead. A real "SPICE sessions" metric
	// needs connections grouped into sessions, which this change does not attempt.
	streamer := NewRawStreamer(
		app.FetchVirtualMachineInstance,
		validateVMIForVNC,
		app.virtHandlerDialer(func(vmi *v1.VirtualMachineInstance, conn kubecli.VirtHandlerConn) (string, error) {
			return conn.SPICEURI(vmi)
		}),
	)

	streamer.Handle(request, response)
}
