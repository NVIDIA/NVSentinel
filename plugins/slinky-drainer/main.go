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
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	drainv1alpha1 "github.com/nvidia/nvsentinel/plugins/slinky-drainer/api/v1alpha1"
	"github.com/nvidia/nvsentinel/plugins/slinky-drainer/pkg/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(drainv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		podCheckInterval time.Duration
		drainTimeout     time.Duration
		metricsAddr      string
		probeAddr        string
		slinkyNamespace  string
	)

	flag.DurationVar(&podCheckInterval, "pod-check-interval", 5*time.Second, "Polling interval for pod conditions")
	flag.DurationVar(&drainTimeout, "drain-timeout", 30*time.Minute, "Overall drain operation timeout")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address for metrics endpoint")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address for health probe endpoint")
	flag.StringVar(&slinkyNamespace, "slinky-namespace", "slinky", "Namespace where Slinky workload pods run")

	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "Unable to create manager")
		os.Exit(1)
	}

	if err := controller.NewDrainRequestReconciler(mgr,
		podCheckInterval,
		drainTimeout, slinkyNamespace).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up health check")
		os.Exit(1)
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting Slinky Drainer controller",
		"slinkyNamespace", slinkyNamespace,
		"podCheckInterval", podCheckInterval,
		"drainTimeout", drainTimeout)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Problem running manager")
		os.Exit(1)
	}
}
