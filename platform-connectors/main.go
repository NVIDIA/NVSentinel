package main

import (
	"context"
	"flag"
	"net"
	"os"
	"os/signal"
	"syscall"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/kubernetes"

	"k8s.io/apimachinery/pkg/util/json"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/ringbuffer"
	"k8s.io/klog/v2"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/example"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/server"
	"google.golang.org/grpc"
)

func main() {
	socket := flag.String("socket", "", "unix socket path")
	configFilePath := flag.String("config", "/etc/config/config.json", "path to the config file")
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

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		klog.Fatalf("Failed to fetch nodename")
	}

	var ringBuffer *ringbuffer.RingBuffer
	ringBuffer = nil

	if enableK8sPlatformConnector == "true" {
		ringBuffer = ringbuffer.NewRingBuffer("kubernetes", ctx)
		server.InitializeAndAttachRingBufferForConnectors(ringBuffer)
		k8sConnector := kubernetes.InitializeK8sConnector(ringBuffer, string(nodeName), stopCh)

		go k8sConnector.FetchAndProcessHealthMetric(ctx)
	} else {
		ringBuffer := ringbuffer.NewRingBuffer("example", ctx)
		exampleConnector := example.InitializeExampleConnector(ringBuffer, string(nodeName))

		go exampleConnector.FetchAndProcessHealthMetric(ctx)
	}

	err = os.RemoveAll(*socket)
	if err != nil {
		klog.Fatalf("failed to remove existing socket with error %s", err)
	}

	lis, err := net.Listen("unix", *socket)
	if err != nil {
		klog.Fatalf(err.Error())
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

	if ringBuffer != nil {
		ringBuffer.ShutDownHealthMetricQueue()
	}

	lis.Close()
	os.Remove(*socket)
	cancel()
}
