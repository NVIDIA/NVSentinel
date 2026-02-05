// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Command controller-test runs the NVSentinel controllers locally for integration testing.
// It connects to a remote Kubernetes cluster and processes HealthEvent CRDs.
//
// Usage:
//
//	KUBECONFIG=/path/to/kubeconfig controller-test
//	controller-test --kubeconfig=/path/to/kubeconfig
package main

import (
	"context"
	"flag"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	nvsentinelv1alpha1 "github.com/nvidia/nvsentinel/api/nvsentinel/v1alpha1"
	"github.com/nvidia/nvsentinel/pkg/controllers/healthevents"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(nvsentinelv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr          string
		enableLeaderElection bool
		probeAddr            string
		dryRun               bool
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.BoolVar(&dryRun, "dry-run", false, "Dry run mode - don't actually cordon/drain/remediate")

	klog.InitFlags(nil)
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	log := ctrl.Log.WithName("setup")
	log.Info("Starting NVSentinel Controller Test Runner",
		"dryRun", dryRun,
	)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                ctrl.Options{}.Metrics,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "nvsentinel-controllers.nvidia.com",
	})
	if err != nil {
		log.Error(err, "Unable to create manager")
		os.Exit(1)
	}

	// Add field index for spec.nodeName on Pods - required for DrainController
	// to list pods by node name using field selectors
	ctx := context.Background()
	if err := mgr.GetFieldIndexer().IndexField(ctx, &corev1.Pod{}, "spec.nodeName",
		func(obj client.Object) []string {
			pod := obj.(*corev1.Pod)
			if pod.Spec.NodeName == "" {
				return nil
			}
			return []string{pod.Spec.NodeName}
		}); err != nil {
		log.Error(err, "Unable to create field index for pods")
		os.Exit(1)
	}
	log.Info("Pod field index for spec.nodeName created")

	// Set up QuarantineController
	if err := (&healthevents.QuarantineController{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		Recorder:                mgr.GetEventRecorderFor("quarantine-controller"),
		MaxConcurrentReconciles: 2,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "Unable to create controller", "controller", "QuarantineController")
		os.Exit(1)
	}
	log.Info("QuarantineController registered")

	// Set up DrainController
	if err := (&healthevents.DrainController{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		Recorder:                mgr.GetEventRecorderFor("drain-controller"),
		IgnoreDaemonSets:        true,
		DeleteEmptyDirData:      true,
		GracePeriodSeconds:      30,
		MaxConcurrentReconciles: 2,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "Unable to create controller", "controller", "DrainController")
		os.Exit(1)
	}
	log.Info("DrainController registered")

	// Set up RemediationController
	if err := (&healthevents.RemediationController{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		Recorder:                mgr.GetEventRecorderFor("remediation-controller"),
		DefaultStrategy:         healthevents.StrategyManual, // Default to manual for safety
		RebootJobNamespace:      "nvsentinel-system",
		RebootJobImage:          "busybox:latest",
		RebootJobTTL:            time.Hour,
		MaxConcurrentReconciles: 1,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "Unable to create controller", "controller", "RemediationController")
		os.Exit(1)
	}
	log.Info("RemediationController registered")

	// Set up TTLController
	if err := (&healthevents.TTLController{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		Recorder:                mgr.GetEventRecorderFor("ttl-controller"),
		DefaultTTL:              168 * time.Hour, // 7 days
		MaxConcurrentReconciles: 1,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "Unable to create controller", "controller", "TTLController")
		os.Exit(1)
	}
	log.Info("TTLController registered")

	// Add health checks
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "Unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "Unable to set up ready check")
		os.Exit(1)
	}

	log.Info("All controllers registered, starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "Problem running manager")
		os.Exit(1)
	}
}
