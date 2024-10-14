package main

import (
	"context"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/kubernetes"

	"k8s.io/apimachinery/pkg/util/json"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/ringbuffer"
	"k8s.io/klog/v2"

	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/nodehealtheventsudsconnector/nodehealtheventsudsconnectorserver"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/server"
	"google.golang.org/grpc"
)

//nolint:cyclop
func main() {
	socket := flag.String("socket", "", "unix socket path")
	nodeHealthEventsSocket := flag.String("nodehealtheventssocket", "", "device plugin socket path")
	configFilePath := flag.String("config", "/etc/config/config.json", "path to the config file")

	var metricsPort = flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")

	flag.Parse()

	if *socket == "" {
		klog.Fatalf("socket is not present")
	}

	sigs := make(chan os.Signal, 1)
	stopCh := make(chan struct{})

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	data, err := os.ReadFile(*configFilePath)
	if err != nil {
		klog.Fatalf("Failed to read platform-connector-configmap with err %s", err)
	}

	result := make(map[string]interface{})

	err = json.Unmarshal(data, &result)
	if err != nil {
		klog.Fatalf("Failed to unmarshal the configmap data with error %s", err)
	}

	enableK8sPlatformConnector := result["enableK8sPlatformConnector"]
	enableNodeHealthEventsUDSConnector := result["enableNodeHealthEventsUDSConnector"]

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		klog.Fatalf("Failed to fetch nodename")
	}

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		//nolint:gosec // G114: Ignoring the use of http.ListenAndServe without timeouts
		err := http.ListenAndServe(":"+*metricsPort, nil)
		if err != nil {
			klog.Fatalf("Failed to start metrics server: %v", err)
		}
	}()

	var k8sRingBuffer *ringbuffer.RingBuffer
	k8sRingBuffer = nil

	if enableK8sPlatformConnector == "true" {
		k8sRingBuffer = ringbuffer.NewRingBuffer("kubernetes", ctx)
		server.InitializeAndAttachRingBufferForConnectors(k8sRingBuffer)
		k8sConnector := kubernetes.InitializeK8sConnector(k8sRingBuffer, string(nodeName), stopCh, ctx)

		go k8sConnector.FetchAndProcessHealthMetric(ctx)
	}

	var nodeHealthEventsRingBuffer *ringbuffer.RingBuffer = nil

	var nodeHealthEventsListener net.Listener = nil

	if enableNodeHealthEventsUDSConnector == "true" {
		nodeHealthEventsRingBuffer = ringbuffer.NewRingBuffer("nodeHealthEventsRingBuffer", ctx)

		nodehealtheventsudsconnectorserver.CreateAndStartNodeHealthEventsUDSServer(nodeHealthEventsSocket,
			&nodeHealthEventsListener, nodeHealthEventsRingBuffer, nodeName, stopCh, ctx)
	}

	err = os.RemoveAll(*socket)
	if err != nil {
		klog.Fatalf("failed to remove existing socket with error %s", err)
	}

	lis, err := net.Listen("unix", *socket)
	if err != nil {
		klog.Fatalf("Error creating platform-connector unixsocket %s", err)
	}

	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)

	pb.RegisterPlatformConnectorServer(grpcServer, &server.PlatformConnectorServer{})

	err = grpcServer.Serve(lis)
	if err != nil {
		klog.Fatalf("Not able to accept incoming connections.Error is %s", err)
	}

	klog.Infof("Waiting for signal")
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigs
	klog.Infof("Received signal %v", sig)

	close(stopCh)

	if lis != nil {
		k8sRingBuffer.ShutDownHealthMetricQueue()
		lis.Close()
		os.Remove(*socket)
	}

	if nodeHealthEventsListener != nil {
		nodeHealthEventsRingBuffer.ShutDownHealthMetricQueue()
		nodeHealthEventsListener.Close()
		os.Remove(*nodeHealthEventsSocket)
	}

	cancel()
}
