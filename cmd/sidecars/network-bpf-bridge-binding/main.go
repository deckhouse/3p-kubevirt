/*
Network binding sidecar for KubeVirt: creates a TAP in the pod netns, loads bpf_bridge.o,
attaches TC (clsact ingress) on the TAP and the pod-side interface (eth0 by default) so
the BPF program acts as an L2 proxy between them, and sets the libvirt ethernet target
to the TAP.
*/
package main

import (
	"flag"
	"net"
	"os"
	"path/filepath"

	"google.golang.org/grpc"

	"kubevirt.io/client-go/log"
	"kubevirt.io/kubevirt/pkg/hooks"
	hooksInfo "kubevirt.io/kubevirt/pkg/hooks/info"
	hooksV1alpha3 "kubevirt.io/kubevirt/pkg/hooks/v1alpha3"

	srv "kubevirt.io/kubevirt/cmd/sidecars/network-bpf-bridge-binding/server"
)

const hookSocket = "bpfbridge.sock"

func main() {
	bpfObj := flag.String("bpf-obj", envOrDefault("BPF_BRIDGE_OBJ", ""), "path to compiled bpf_bridge.o (default: /opt/network-bpf-bridge-binding/bpf_bridge.o)")
	tapName := flag.String("tap-name", envOrDefault("BPF_BRIDGE_TAP", ""), "TAP interface name (default kvbpf0)")
	podIface := flag.String("pod-iface", envOrDefault("BPF_BRIDGE_POD_IFACE", ""), "pod-side interface bridged with the TAP (default eth0)")
	flag.Parse()

	socketPath := filepath.Join(hooks.HookSocketsSharedDirectory, hookSocket)
	socket, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Log.Reason(err).Errorf("Failed to listen on socket %s", socketPath)
		os.Exit(1)
	}
	defer func() { _ = os.Remove(socketPath) }()

	grpcServer := grpc.NewServer()
	hooksInfo.RegisterInfoServer(grpcServer, srv.InfoServer{Version: "v1alpha3"})

	shutdownChan := make(chan struct{})
	hooksV1alpha3.RegisterCallbacksServer(grpcServer, &srv.V1alpha3Server{
		Done:     shutdownChan,
		BPFObj:   *bpfObj,
		TapName:  *tapName,
		PodIface: *podIface,
	})
	log.Log.Infof("bpf-bridge-binding sidecar on %s (API v1alpha3)", socketPath)
	srv.Serve(grpcServer, socket, shutdownChan)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
