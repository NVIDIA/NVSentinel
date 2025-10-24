// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/kubernetes"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"k8s.io/apimachinery/pkg/util/json"

	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"

	pb "github.com/nvidia/nvsentinel/platform-connectors/pkg/protos"

	"github.com/nvidia/nvsentinel/platform-connectors/pkg/server"
	"google.golang.org/grpc"
)

const (
	True = "true"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

//nolint:cyclop
func main() {
	// Initialize klog flags to allow command-line control (e.g., -v=3)
	klog.InitFlags(nil)

	socket := flag.String("socket", "", "unix socket path")
	configFilePath := flag.String("config", "/etc/config/config.json", "path to the config file")

	var metricsPort = flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")

	var mongoClientCertMountPath = flag.String("mongo-client-cert-mount-path", "/etc/ssl/mongo-client",
		"path where the mongodb client cert is mounted")

	flag.Parse()

	logger := textlogger.NewLogger(textlogger.NewConfig()).WithValues(
		"version", version,
		"module", "platform-connectors",
	)

	klog.SetLogger(logger)
	klog.InfoS("Starting platform-connectors", "version", version, "commit", commit, "date", date)
	defer klog.Flush()

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
	enableMongoDBStorePlatformConnector := result["enableMongoDBStorePlatformConnector"]

	var k8sRingBuffer *ringbuffer.RingBuffer
	k8sRingBuffer = nil

	if enableK8sPlatformConnector == True {
		k8sRingBuffer = ringbuffer.NewRingBuffer("kubernetes", ctx)
		server.InitializeAndAttachRingBufferForConnectors(k8sRingBuffer)

		qpsTemp, ok := result["K8sConnectorQps"].(float64)
		if !ok {
			klog.Fatalf("failed to convert K8sConnectorQps to float: %v", result["K8sConnectorQps"])
		}

		qps := float32(qpsTemp)

		burst, ok := result["K8sConnectorBurst"].(int64)
		if !ok {
			klog.Fatalf("failed to convert K8sConnectorBurst to int: %v", result["K8sConnectorBurst"])
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

	err = os.Remove(*socket)
	if err != nil && !os.IsNotExist(err) {
		klog.Fatalf("failed to remove existing socket with error %s", err)
	}

	lis, err := net.Listen("unix", *socket)
	if err != nil {
		klog.Fatalf("Error creating platform-connector unixsocket %s", err)
	}

	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)

	pb.RegisterPlatformConnectorServer(grpcServer, &server.PlatformConnectorServer{})

	go func() {
		err = grpcServer.Serve(lis)
		if err != nil {
			klog.Fatalf("Not able to accept incoming connections. Error is %s", err)
		}
	}()

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		//nolint:gosec // G114: Ignoring the use of http.ListenAndServe without timeouts
		err := http.ListenAndServe(":"+*metricsPort, nil)
		if err != nil {
			klog.Fatalf("Failed to start metrics server: %v", err)
		}
	}()

	klog.Infof("Waiting for signal")
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigs
	klog.Infof("Received signal %v", sig)

	close(stopCh)

	if lis != nil {
		if k8sRingBuffer != nil {
			k8sRingBuffer.ShutDownHealthMetricQueue()
		}

		lis.Close()
		os.Remove(*socket)
	}

	cancel()
}
