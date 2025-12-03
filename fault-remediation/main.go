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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/server"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/initializer"
)

var (
	scheme = runtime.NewScheme()

	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"

	// These variables are populated by parsing flags
	enableLeaderElection        bool
	enableControllerRuntime     bool
	leaderElectionLeaseDuration time.Duration
	leaderElectionRenewDeadline time.Duration
	leaderElectionRetryPeriod   time.Duration
	leaderElectionNamespace     string
	metricsAddr                 string
	healthAddr                  string
	kubeconfigPath              string
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

	if !enableControllerRuntime && enableLeaderElection {
		return errors.New("leader-election requires controller-runtime")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	params := initializer.InitializationParams{
		KubeconfigPath:     kubeconfigPath,
		TomlConfigPath:     tomlConfigPath,
		DryRun:             dryRun,
		EnableLogCollector: enableLogCollector,
	}

	components, err := initializer.InitializeAll(ctx, params)
	if err != nil {
		return fmt.Errorf("initialization failed: %w", err)
	}

	reconciler := components.FaultRemediationReconciler

	defer func() {
		if err := reconciler.CloseAll(ctx); err != nil {
			slog.Error("failed to close datastore components", "error", err)
		}
	}()

	if enableControllerRuntime {
		err = setupCtrlRuntimeManagement(ctx, components)
		if err != nil {
			return err
		}
	} else {
		err = setupNonCtrlRuntimeManaged(ctx, components)
		if err != nil {
			return err
		}
	}

	return nil
}

func setupNonCtrlRuntimeManaged(ctx context.Context, components *initializer.Components) error {
	slog.Info("Running without controller runtime management")

	metricsAddr = strings.TrimPrefix(metricsAddr, ":")

	portInt, err := strconv.Atoi(metricsAddr)
	if err != nil {
		return fmt.Errorf("invalid metrics port: %w", err)
	}

	srv := server.NewServer(
		server.WithPort(portInt),
		server.WithPrometheusMetricsCtrlRuntime(),
		server.WithSimpleHealth(),
	)

	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		slog.Info("Starting metrics server", "port", portInt)

		if err := srv.Serve(gCtx); err != nil {
			slog.Error("Metrics server failed - continuing without metrics", "error", err)
		}

		return nil
	})

	g.Go(func() error {
		components.FaultRemediationReconciler.StartWatcherStream(gCtx)

		slog.Info("Listening for events on the channel...")

		for event := range components.FaultRemediationReconciler.Watcher.Events() {
			slog.Info("Event received", "event", event)
			_, _ = components.FaultRemediationReconciler.Reconcile(gCtx, &event)
		}

		return nil
	})

	return g.Wait()
}

func setupCtrlRuntimeManagement(ctx context.Context, components *initializer.Components) error {
	slog.Info("Running in controller runtime managed mode")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
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
		return err
	}

	if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		slog.Error("Unable to set up health check", "error", err)
		return err
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		slog.Error("Unable to set up ready check", "error", err)
		return err
	}

	err = components.FaultRemediationReconciler.SetupWithManager(ctx, mgr)
	if err != nil {
		return fmt.Errorf("SetupWithManager failed: %w", err)
	}

	slog.Info("Starting controller runtime controller")

	if err = mgr.Start(ctx); err != nil {
		slog.Error("Problem running manager", "error", err)
		return err
	}

	return nil
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

	flag.StringVar(&kubeconfigPath, "kubeconfig-path", "", "path to kubeconfig file")

	flag.StringVar(&tomlConfigPath, "config-path", "/etc/config/config.toml",
		"path where the fault remediation config file is present")

	flag.BoolVar(&dryRun, "dry-run", false, "flag to run fault remediation module in dry-run mode.")

	flag.BoolVar(
		&enableControllerRuntime,
		"controller-runtime",
		false,
		"Enable controller runtime management of the reconciler.",
	)

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
