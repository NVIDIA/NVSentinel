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
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	klog "k8s.io/klog/v2"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/csp-health-monitor/pkg/datastore"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/csp-health-monitor/pkg/metrics"
	trigger "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/csp-health-monitor/pkg/triggerengine"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
)

const (
	defaultConfigPathSidecar    = "/etc/config/config.toml"
	defaultMongoCertPathSidecar = "/etc/ssl/mongo-client"
	defaultUdsPathSidecar       = "/run/nvsentinel/nvsentinel.sock"
	defaultMetricsPortSidecar   = "2113"
)

// nolint: cyclop
func main() {
	// Command-line flags
	configPath := flag.String("config", defaultConfigPathSidecar, "Path to the TOML configuration file.")
	udsPath := flag.String("uds-path", defaultUdsPathSidecar, "Path to the Platform Connector UDS socket.")
	mongoClientCertMountPath := flag.String(
		"mongo-client-cert-mount-path",
		defaultMongoCertPathSidecar,
		"Directory where MongoDB client tls.crt, tls.key, and ca.crt are mounted.",
	)
	metricsPort := flag.String("metrics-port", defaultMetricsPortSidecar, "Port for the sidecar Prometheus metrics.")

	// Initialise klog and parse flags
	klog.InitFlags(nil)

	// Parse flags after initialising klog
	flag.Parse()

	defer klog.Flush()

	klog.Infof("Starting Quarantine Trigger Engine Sidecar...")
	klog.Infof("Using configuration file: %s", *configPath)
	klog.Infof("Platform Connector UDS Path: %s", *udsPath)
	klog.Infof("MongoDB Client Cert Mount Path: %s", *mongoClientCertMountPath)
	klog.Infof("Exposing sidecar metrics on port: %s", *metricsPort)
	klog.V(2).Infof("Klog verbosity level is set based on the -v flag for sidecar.")

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		klog.Fatalf("Failed to load configuration: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start metrics endpoint in a separate goroutine
	go func() {
		listenAddress := fmt.Sprintf(":%s", *metricsPort)
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())

		server := &http.Server{
			Addr:         listenAddress,
			Handler:      mux,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  15 * time.Second,
		}

		klog.Infof("Metrics server (sidecar) starting to listen on %s", listenAddress)

		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			klog.Errorf("Metrics server (sidecar) failed: %v", err)
		}

		klog.Info("Metrics server (sidecar) stopped.")
	}()

	// Initialise datastore and UDS connection
	store, err := datastore.NewStore(ctx, mongoClientCertMountPath)
	if err != nil {
		klog.Fatalf("Failed to initialize datastore for sidecar: %v", err)
	}

	klog.Info("Datastore initialized successfully for sidecar.")

	klog.Infof("Sidecar attempting to connect to Platform Connector UDS at: unix:%s", *udsPath)
	target := fmt.Sprintf("unix:%s", *udsPath)

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}

	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		metrics.TriggerUDSSendErrors.Inc()
		klog.Fatalf("Sidecar failed to dial Platform Connector UDS %s: %v", target, err)
	}

	defer func() {
		klog.Info("Closing UDS connection for sidecar.")

		if errClose := conn.Close(); errClose != nil {
			klog.Errorf("Error closing sidecar UDS connection: %v", errClose)
		}
	}()
	klog.Info("Sidecar successfully connected to Platform Connector UDS.")

	platformConnectorClient := pb.NewPlatformConnectorClient(conn)

	var k8sClient kubernetes.Interface

	var restCfg *rest.Config

	if cfg != nil && cfg.KubeconfigPath != "" {
		restCfg, err = clientcmd.BuildConfigFromFlags("", cfg.KubeconfigPath)
		if err != nil {
			klog.Errorf("Trigger Engine: failed to build kubeconfig from %s: %v", cfg.KubeconfigPath, err)
		}
	} else {
		restCfg, err = rest.InClusterConfig()
		if err != nil {
			klog.Warningf("Trigger Engine: failed to obtain in-cluster Kubernetes config: %v", err)
		}
	}

	if err == nil && restCfg != nil {
		if k8sClient, err = kubernetes.NewForConfig(restCfg); err != nil {
			klog.Errorf("Trigger Engine: failed to create Kubernetes clientset: %v", err)

			k8sClient = nil
		} else {
			klog.Info("Trigger Engine: Kubernetes clientset initialized successfully for node readiness checks.")
		}
	} else {
		klog.Error("Trigger Engine: failed to initialize Kubernetes clientset.")
	}

	// Initialise and start the trigger engine (blocking)
	engine := trigger.NewEngine(cfg, store, platformConnectorClient, k8sClient)

	klog.Info("Trigger engine starting...")
	engine.Start(ctx) // This is blocking

	klog.Info("Quarantine Trigger Engine Sidecar shut down.")
}
