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
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/initializer"
)

func init() {
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(batchv1.AddToScheme(scheme))
}

var (
	scheme = runtime.NewScheme()

	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"

	// These variables are populated by parsing flags
	enableLeaderElection        bool
	leaderElectionLeaseDuration time.Duration
	leaderElectionRenewDeadline time.Duration
	leaderElectionRetryPeriod   time.Duration
	leaderElectionNamespace     string
	metricsAddr                 string
	healthAddr                  string
	tomlConfigPath              string
	dryRun                      bool
	enableLogCollector          bool
)

func main() {
	logger.SetDefaultStructuredLogger("fault-remediation", version)
	slog.Info("Starting fault-remediation", "version", version, "commit", commit, "date", date)

	if err := auditlogger.InitAuditLogger("fault-remediation"); err != nil {
		slog.Warn("Failed to initialize audit logger", "error", err)
	}

	if err := run(); err != nil {
		slog.Error("Application encountered a fatal error", "error", err)

		if closeErr := auditlogger.CloseAuditLogger(); closeErr != nil {
			slog.Warn("Failed to close audit logger", "error", closeErr)
		}

		os.Exit(1)
	}

	if err := auditlogger.CloseAuditLogger(); err != nil {
		slog.Warn("Failed to close audit logger", "error", err)
	}
}

func run() error {
	parseFlags()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err := setupCtrlRuntimeManagement(ctx)
	if err != nil {
		return err
	}

	return nil
}

func setupCtrlRuntimeManagement(ctx context.Context) error {
	slog.Info("Running in controller runtime managed mode")

	mgr, err := createManager()
	if err != nil {
		return err
	}

	params := initializer.InitializationParams{
		TomlConfigPath:     tomlConfigPath,
		DryRun:             dryRun,
		EnableLogCollector: enableLogCollector,
		Config:             mgr.GetConfig(),
	}

	g, gCtx := errgroup.WithContext(ctx)

	// Start the manager first so health/metrics endpoints are live immediately.
	// This prevents Kubernetes liveness probes from killing the pod while MongoDB
	// initialization (which may be slow due to stale resume tokens or connectivity
	// issues) is still in progress.
	g.Go(func() error {
		slog.Info("Starting controller runtime controller")

		if err := mgr.Start(gCtx); err != nil {
			slog.Error("Problem running manager", "error", err)
			return err
		}

		return nil
	})

	// Initialize datastore and reconciler concurrently — the manager is already
	// serving health probes, so the pod won't be killed during this phase.
	// cleanupReconciler is set once the reconciler is created so cleanup can run
	// after g.Wait() (i.e., after the manager has fully drained).
	var cleanupReconciler func()

	g.Go(func() error {
		cleanup, initErr := initializeAndWatch(gCtx, params, mgr)
		cleanupReconciler = cleanup

		return initErr
	})

	err = g.Wait()

	if cleanupReconciler != nil {
		cleanupReconciler()
	}

	return err
}

func createManager() (ctrl.Manager, error) {
	cfg := ctrl.GetConfigOrDie()
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return auditlogger.NewAuditingRoundTripper(rt)
	})

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress:  healthAddr,
		LeaderElection:          enableLeaderElection,
		LeaseDuration:           &leaderElectionLeaseDuration,
		RenewDeadline:           &leaderElectionRenewDeadline,
		RetryPeriod:             &leaderElectionRetryPeriod,
		LeaderElectionID:        "controller-leader-elect-fault-remediation",
		LeaderElectionNamespace: leaderElectionNamespace,
	})
	if err != nil {
		slog.Error("Unable to start manager", "error", err)
		return nil, err
	}

	if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		slog.Error("Unable to set up health check", "error", err)
		return nil, err
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		slog.Error("Unable to set up ready check", "error", err)
		return nil, err
	}

	return mgr, nil
}

const reconcilerCloseTimeout = 30 * time.Second

// initializeAndWatch performs MongoDB initialization, registers the reconciler, and
// blocks until shutdown or unexpected stream death. It returns a cleanup function that
// the caller must invoke after the manager has fully stopped (after g.Wait) so that
// datastore resources are not torn down under in-flight reconciles.
func initializeAndWatch(
	ctx context.Context, params initializer.InitializationParams, mgr ctrl.Manager,
) (cleanup func(), err error) {
	components, err := initializer.InitializeAll(ctx, params, mgr.GetClient())
	if err != nil {
		return nil, fmt.Errorf("initialization failed: %w", err)
	}

	reconciler := components.FaultRemediationReconciler

	cleanup = func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), reconcilerCloseTimeout)
		defer cancel()

		if closeErr := reconciler.CloseAll(closeCtx); closeErr != nil {
			slog.Error("failed to close datastore components", "error", closeErr)
		}
	}

	watcherDone, setupErr := reconciler.SetupWithManager(ctx, mgr)
	if setupErr != nil {
		return cleanup, fmt.Errorf("SetupWithManager failed: %w", setupErr)
	}

	slog.Info("Initialization completed, reconciler registered with manager")

	select {
	case <-ctx.Done():
		return cleanup, nil
	case <-watcherDone:
		if ctx.Err() == nil {
			return cleanup, fmt.Errorf("change stream watcher terminated unexpectedly, event processing has stopped")
		}

		return cleanup, nil
	}
}

func parseFlags() {
	flag.StringVar(
		&metricsAddr,
		"metrics-address",
		":2112",
		"address/port to expose Prometheus metrics on.",
	)
	flag.StringVar(
		&healthAddr,
		"health-address",
		":9440",
		"address/port to expose healthchecks on. Requires controller-runtime mode"+
			" (otherwise metrics and health are on same port).",
	)

	flag.StringVar(&tomlConfigPath, "config-path", "/etc/config/config.toml",
		"path where the fault remediation config file is present")

	flag.BoolVar(&dryRun, "dry-run", false, "flag to run fault remediation module in dry-run mode.")

	flag.BoolVar(
		&enableLeaderElection,
		"leader-elect",
		false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager. "+
			"Requires controller-runtime enabled.",
	)

	flag.DurationVar(
		&leaderElectionLeaseDuration,
		"leader-elect-lease-duration",
		15*time.Second,
		"Interval at which non-leader candidates will wait to force acquire leadership (duration string). "+
			"Requires controller-runtime enabled.",
	)

	flag.DurationVar(
		&leaderElectionRenewDeadline,
		"leader-elect-renew-deadline",
		10*time.Second,
		"Duration that the leading controller manager will retry refreshing leadership "+
			"before giving up (duration string). Requires controller-runtime enabled.",
	)

	flag.DurationVar(
		&leaderElectionRetryPeriod,
		"leader-elect-retry-period",
		2*time.Second,
		"Duration the LeaderElector clients should wait between tries of actions (duration string). "+
			"Requires controller-runtime enabled.",
	)

	flag.StringVar(
		&leaderElectionNamespace,
		"leader-elect-namespace",
		"",
		"Namespace that the controller performs leader election in. "+
			"If unspecified, the controller will discover which namespace it is running in. "+
			"Requires controller-runtime enabled.",
	)

	flag.BoolVar(&enableLogCollector, "enable-log-collector", false,
		"enable log collector feature for gathering logs from affected nodes")

	flag.Parse()
}
