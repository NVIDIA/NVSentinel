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
	"log/slog"
	"os"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	janitordgxcnvidiacomv1alpha1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/config"
	"github.com/nvidia/nvsentinel/janitor/pkg/controller"
)

var (
	scheme = runtime.NewScheme()
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(janitordgxcnvidiacomv1alpha1.AddToScheme(scheme))
}

func main() {
	logger.SetDefaultStructuredLogger("janitor", version)
	slog.Info("Starting janitor", "version", version, "commit", commit, "date", date)

	// Bridge slog to logr for controller-runtime
	// This ensures that controllers using log.FromContext(ctx) get slog
	slogHandler := slog.Default().Handler()
	logrLogger := logr.FromSlogHandler(slogHandler)
	ctrllog.SetLogger(logrLogger)

	if err := run(); err != nil {
		slog.Error("Application encountered a fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var metricsAddr string

	var probeAddr string

	var enableLeaderElection bool

	var manualMode bool

	var rebootTimeout time.Duration

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.BoolVar(&manualMode, "manual-mode", false, "Run in manual mode (don't send reboot signals)")
	flag.DurationVar(&rebootTimeout, "reboot-timeout", 30*time.Minute, "Timeout for reboot operations")

	flag.Parse()

	slog.Info("Parsed flags",
		"metrics-bind-address", metricsAddr,
		"health-probe-bind-address", probeAddr,
		"leader-elect", enableLeaderElection,
		"manual-mode", manualMode,
		"reboot-timeout", rebootTimeout)

	// Setup controller manager
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "janitor.dgxc.nvidia.com",
	})
	if err != nil {
		slog.Error("Unable to create manager", "error", err)
		return err
	}

	// Setup RebootNode controller
	rebootNodeConfig := &config.RebootNodeControllerConfig{
		ManualMode: manualMode,
		Timeout:    rebootTimeout,
	}

	if err = (&controller.RebootNodeReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Config: rebootNodeConfig,
	}).SetupWithManager(mgr); err != nil {
		slog.Error("Unable to create controller", "controller", "RebootNode", "error", err)
		return err
	}

	// Setup health checks
	if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		slog.Error("Unable to set up health check", "error", err)
		return err
	}

	if err = mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		slog.Error("Unable to set up ready check", "error", err)
		return err
	}

	slog.Info("Starting manager")

	if err = mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		slog.Error("Problem running manager", "error", err)
		return err
	}

	return nil
}
