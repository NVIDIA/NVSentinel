// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/kubernetes"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/store"

	"k8s.io/apimachinery/pkg/util/json"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/ringbuffer"
	"k8s.io/klog/v2"

	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/nodehealtheventsudsconnector/nodehealtheventsudsconnectorserver"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	True = "true"
	TCP  = "tcp"
	UDS  = "uds"
)

//nolint:cyclop
func main() {
	socket := flag.String("socket", "", "unix socket path")
	nodeHealthEventsSocket := flag.String("nodehealtheventssocket", "", "device plugin socket path")
	configFilePath := flag.String("config", "/etc/config/config.json", "path to the config file")
	transport := flag.String("transport", UDS, "transport mode: uds (Unix Domain Socket) or tcp")

	var metricsPort = flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")

	var mongoClientCertMountPath = flag.String("mongo-client-cert-mount-path", "/etc/ssl/mongo-client",
		"path where the mongodb client cert is mounted")

	listenAddr := flag.String("listen-addr", "",
		"TCP listen address for PlatformConnector gRPC (e.g., :9090). If empty, TCP is disabled")

	serverCertPath := flag.String("server-cert", "", "Path to TLS server certificate (PEM)")
	serverKeyPath := flag.String("server-key", "", "Path to TLS server key (PEM)")
	caCertPath := flag.String("ca-cert", "", "Path to CA certificate for client verification (PEM)")

	flag.Parse()

	// Validate transport
	if *transport != UDS && *transport != TCP {
		klog.Fatalf("Invalid transport: %s. Must be 'uds' or 'tcp'", *transport)
	}

	klog.Infof("Starting platform-connectors with %s transport", *transport)

	// Branch based on transport type
	switch *transport {
	case UDS:
		runUDSMode(socket, nodeHealthEventsSocket, configFilePath, metricsPort, mongoClientCertMountPath,
			listenAddr, serverCertPath, serverKeyPath, caCertPath)
	case TCP:
		runTCPMode(configFilePath, metricsPort, mongoClientCertMountPath,
			listenAddr, serverCertPath, serverKeyPath, caCertPath)
	}
}

func runUDSMode(socket, nodeHealthEventsSocket,
	configFilePath, metricsPort, mongoClientCertMountPath *string,
	listenAddr *string, serverCertPath, serverKeyPath, caCertPath *string) {
	klog.Infof("Running with Unix Domain Socket (UDS) transport")

	runPlatformConnector(socket, nodeHealthEventsSocket, configFilePath, metricsPort, mongoClientCertMountPath,
		listenAddr, serverCertPath, serverKeyPath, caCertPath)
}

func runTCPMode(configFilePath, metricsPort, mongoClientCertMountPath *string,
	listenAddr *string, serverCertPath, serverKeyPath, caCertPath *string) {
	klog.Infof("Running with TCP transport - centralized deployment with TLS")
	klog.Infof("Using --listen-addr for TCP endpoint")

	// In TCP mode, we don't use Unix sockets
	emptySocket := ""
	emptyNodeHealthEventsSocket := ""

	runPlatformConnector(&emptySocket,
		&emptyNodeHealthEventsSocket, configFilePath, metricsPort, mongoClientCertMountPath,
		listenAddr, serverCertPath, serverKeyPath, caCertPath)
}

//nolint:gocognit,cyclop
func runPlatformConnector(socket, nodeHealthEventsSocket,
	configFilePath, metricsPort, mongoClientCertMountPath *string,
	listenAddr *string, serverCertPath, serverKeyPath, caCertPath *string) {
	if *socket == "" && (listenAddr == nil || *listenAddr == "") {
		klog.Fatalf("either --socket (UDS) or --listen-addr (TCP) must be provided")
	}

	sigs := make(chan os.Signal, 1)
	stopCh := make(chan struct{})

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	data, readErr := os.ReadFile(*configFilePath)
	if readErr != nil {
		klog.Fatalf("Failed to read platform-connector-configmap with err %s", readErr)
	}

	result := make(map[string]interface{})

	if unmarshalErr := json.Unmarshal(data, &result); unmarshalErr != nil {
		klog.Fatalf("Failed to unmarshal the configmap data with error %s", unmarshalErr)
	}

	enableK8sPlatformConnector := result["enableK8sPlatformConnector"]
	enableMongoDBStorePlatformConnector := result["enableMongoDBStorePlatformConnector"]
	enableNodeHealthEventsUDSConnector := result["enableNodeHealthEventsUDSConnector"]

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

	if enableK8sPlatformConnector == True {
		k8sRingBuffer = ringbuffer.NewRingBuffer("kubernetes", ctx)
		server.InitializeAndAttachRingBufferForConnectors(k8sRingBuffer)

		qpsTemp, ok := result["K8sConnectorQps"].(float64)
		if !ok {
			klog.Fatalf("failed to convert K8sConnectorQps to float")
		}

		qps := float32(qpsTemp)

		burst, ok := result["K8sConnectorBurst"].(int64)
		if !ok {
			klog.Fatalf("failed to convert K8sConnectorBurst to int")
		}

		k8sConnector := kubernetes.InitializeK8sConnector(ctx, k8sRingBuffer, qps, int(burst), stopCh)

		go k8sConnector.FetchAndProcessHealthMetric(ctx)
	}

	if enableMongoDBStorePlatformConnector == True {
		ringBuffer := ringbuffer.NewRingBuffer("mongodbStore", ctx)
		server.InitializeAndAttachRingBufferForConnectors(ringBuffer)
		storeConnector := store.InitializeMongoDbStoreConnector(ctx, ringBuffer, *mongoClientCertMountPath)

		go storeConnector.FetchAndProcessHealthMetric(ctx)
	}

	var nodeHealthEventsRingBuffer *ringbuffer.RingBuffer = nil

	var nodeHealthEventsListener net.Listener = nil

	if enableNodeHealthEventsUDSConnector == "true" && *nodeHealthEventsSocket != "" {
		nodeHealthEventsRingBuffer = ringbuffer.NewRingBuffer("nodeHealthEventsRingBuffer", ctx)

		nodehealtheventsudsconnectorserver.CreateAndStartNodeHealthEventsUDSServer(nodeHealthEventsSocket,
			&nodeHealthEventsListener, nodeHealthEventsRingBuffer, stopCh, ctx)
	}

	// Create listener based on transport type
	var lis net.Listener

	var err error

	var opts []grpc.ServerOption

	if *socket != "" {
		_ = os.RemoveAll(*socket)

		lis, err = net.Listen("unix", *socket)
		if err != nil {
			klog.Fatalf("Error creating platform-connector unixsocket %s", err)
		}

		klog.Infof("Platform connector UDS listening on %s", *socket)
	} else if listenAddr != nil && *listenAddr != "" {
		lis, err = net.Listen(TCP, *listenAddr)
		if err != nil {
			klog.Fatalf("Failed to listen on %s: %v", *listenAddr, err)
		}

		klog.Infof("Platform connector TCP listening on %s with TLS", *listenAddr)

		tcfg, err := getTLSConfig(serverCertPath, serverKeyPath, caCertPath)
		if err != nil {
			klog.Fatalf("Failed to get TLS config: %v", err)
		}

		opts = append(opts, grpc.Creds(credentials.NewTLS(tcfg)))
	}

	grpcServer := grpc.NewServer(opts...)
	pb.RegisterPlatformConnectorServer(grpcServer, &server.PlatformConnectorServer{})

	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			klog.Errorf("gRPC server error: %v", err)
		}
	}()

	klog.Infof("Platform connector started")
	klog.Infof("Waiting for signal")
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigs
	klog.Infof("Received signal %v", sig)

	close(stopCh)

	// Graceful shutdown
	grpcServer.GracefulStop()

	// Cleanup listener
	if lis != nil {
		lis.Close()

		if lis.Addr().Network() == "unix" {
			os.Remove(*socket)
		}
	}

	if k8sRingBuffer != nil {
		k8sRingBuffer.ShutDownHealthMetricQueue()
	}

	if nodeHealthEventsListener != nil {
		nodeHealthEventsRingBuffer.ShutDownHealthMetricQueue()
		nodeHealthEventsListener.Close()

		if *nodeHealthEventsSocket != "" {
			os.Remove(*nodeHealthEventsSocket)
		}
	}

	cancel()
}

func getTLSConfig(serverCertPath, serverKeyPath, caCertPath *string) (*tls.Config, error) {
	if *serverCertPath == "" || *serverKeyPath == "" || *caCertPath == "" {
		return nil, fmt.Errorf("TLS enabled but cert/key/ca not provided: %v, %v, %v",
			*serverCertPath, *serverKeyPath, *caCertPath)
	}

	cert, lerr := tls.LoadX509KeyPair(*serverCertPath, *serverKeyPath)
	if lerr != nil {
		return nil, fmt.Errorf("Failed to load server certificates: %w", lerr)
	}

	caBytes, rerr := os.ReadFile(*caCertPath)
	if rerr != nil {
		return nil, fmt.Errorf("Failed to read CA cert: %w", rerr)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("Failed to append CA cert")
	}

	tcfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.NoClientCert,
		MinVersion:   tls.VersionTLS13,
	}

	return tcfg, nil
}
