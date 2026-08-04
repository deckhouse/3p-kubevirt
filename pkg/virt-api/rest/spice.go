package rest

import (
	restful "github.com/emicklei/go-restful/v3"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
)

// SPICERequestHandler открывает websocket-соединение к SPICE-дисплею ВМ.
//
// В отличие от VNC, клиент устанавливает несколько таких соединений на одну сессию —
// по одному на канал SPICE (main, display, inputs, cursor, звук, usbredir). Каждый
// запрос обслуживается независимо, поэтому здесь нет ни счётчика сессий, ни вытеснения
// предыдущего соединения.
func (app *SubresourceAPIApp) SPICERequestHandler(request *restful.Request, response *restful.Response) {
	// ponytail: без метрик активных соединений — у VNC они считают сессии, а для SPICE
	// счётчик соединений сессиям не равен и вводил бы в заблуждение. Отдельная метрика
	// «сессий SPICE» нужна, но требует группировки соединений, что за рамками PoC.
	streamer := NewRawStreamer(
		app.FetchVirtualMachineInstance,
		validateVMIForVNC,
		app.virtHandlerDialer(func(vmi *v1.VirtualMachineInstance, conn kubecli.VirtHandlerConn) (string, error) {
			return conn.SPICEURI(vmi)
		}),
	)

	streamer.Handle(request, response)
}
