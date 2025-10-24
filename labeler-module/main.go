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
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/nvidia/nvsentinel/labeler-module/pkg/labeler"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"

	kubeconfig     = flag.String("kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	metricsPort    = flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")
	dcgmAppLabel   = flag.String("dcgm-app-label", "nvidia-dcgm", "App label value for DCGM pods")
	driverAppLabel = flag.String("driver-app-label", "nvidia-driver-daemonset", "App label value for driver pods")
)

func main() {
	// Initialize klog flags to allow command-line control (e.g., -v=3)
	klog.InitFlags(nil)
	flag.Parse()

	logger := textlogger.NewLogger(textlogger.NewConfig()).WithValues(
		"version", version,
		"module", "labeler-module",
	)

	klog.SetLogger(logger)
	klog.InfoS("Starting labeler-module", "version", version, "commit", commit, "date", date)
	defer klog.Flush()

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		klog.Fatalf("Failed to create kubernetes config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create clientset: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	labelerInstance, err := labeler.NewLabeler(clientset, 30*time.Second, *dcgmAppLabel, *driverAppLabel)
	if err != nil {
		klog.Fatalf("Failed to create labeler: %v", err)
	}

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		klog.Infof("Starting metrics server on port %s", *metricsPort)

		//nolint:gosec // G114: Ignoring the use of http.ListenAndServe without timeouts
		if err := http.ListenAndServe(":"+*metricsPort, nil); err != nil {
			klog.Errorf("Failed to start metrics server: %v", err)
		}
	}()

	if err := labelerInstance.Run(ctx); err != nil {
		klog.Fatalf("Failed to run labeler: %v", err)
	}

	klog.Info("Node Labeler Module stopped")
}
