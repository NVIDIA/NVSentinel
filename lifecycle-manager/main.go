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
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
	"github.com/nvidia/nvsentinel/lifecycle-manager/internal/controller"
	"github.com/nvidia/nvsentinel/lifecycle-manager/pkg/config"
	webhookv1alpha1 "github.com/nvidia/nvsentinel/lifecycle-manager/pkg/webhook/v1alpha1"
	// +kubebuilder:scaffold:imports
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
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

type serverSetup struct {
	webhookServer        webhook.Server
	metricsServerOptions metricsserver.Options
	metricsCertWatcher   *certwatcher.CertWatcher
	webhookCertWatcher   *certwatcher.CertWatcher
}

func setupTLSAndServers(enableHTTP2 bool, webhookCertPath, webhookCertName, webhookCertKey string, metricsAddr string,
	secureMetrics bool, metricsCertPath, metricsCertName, metricsCertKey string) (serverSetup, error) {
	var tlsOpts []func(*tls.Config)

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, func(c *tls.Config) {
			slog.Info("Disabling HTTP/2")

			c.NextProtos = []string{"http/1.1"}
		})
	}

	webhookTLSOpts := append([]func(*tls.Config){}, tlsOpts...)

	var result serverSetup

	if len(webhookCertPath) > 0 {
		slog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName,
			"webhook-cert-key", webhookCertKey)

		watcher, err := certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			return serverSetup{}, fmt.Errorf("failed to initialize webhook certificate watcher: %w", err)
		}

		result.webhookCertWatcher = watcher

		webhookTLSOpts = append(webhookTLSOpts, func(c *tls.Config) {
			c.GetCertificate = watcher.GetCertificate
		})
	}

	result.webhookServer = webhook.NewServer(webhook.Options{TLSOpts: webhookTLSOpts})

	result.metricsServerOptions = metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		result.metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if len(metricsCertPath) > 0 {
		slog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName,
			"metrics-cert-key", metricsCertKey)

		watcher, err := certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			return serverSetup{}, fmt.Errorf("failed to initialize metrics certificate watcher: %w", err)
		}

		result.metricsCertWatcher = watcher
		result.metricsServerOptions.TLSOpts = append(result.metricsServerOptions.TLSOpts, func(c *tls.Config) {
			c.GetCertificate = watcher.GetCertificate
		})
	}

	return result, nil
}

func addCertWatchers(mgr ctrl.Manager, setup serverSetup) error {
	if setup.metricsCertWatcher != nil {
		slog.Info("Adding metrics certificate watcher to manager")

		if err := mgr.Add(setup.metricsCertWatcher); err != nil {
			return fmt.Errorf("failed to add metrics certificate watcher to manager: %w", err)
		}
	}

	if setup.webhookCertWatcher != nil {
		slog.Info("Adding webhook certificate watcher to manager")

		if err := mgr.Add(setup.webhookCertWatcher); err != nil {
			return fmt.Errorf("failed to add webhook certificate watcher to manager: %w", err)
		}
	}

	return nil
}

func setupControllers(mgr ctrl.Manager, cfg *config.Config, enableValidationController bool) error {
	var validation *v1alpha1.ValidationConfiguration
	if cfg != nil {
		validation = cfg.Validation
	}

	if err := webhookv1alpha1.SetupWebhookWithManager(mgr, validation, enableValidationController); err != nil {
		return fmt.Errorf("failed to set up webhook: %w", err)
	}

	if enableValidationController {
		if err := (&controller.ValidationRequestReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
			Config: cfg,
		}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("failed to create ValidationRequest controller: %w", err)
		}
	}

	// +kubebuilder:scaffold:builder
	return nil
}

func run() error {
	var (
		metricsAddr                                      string
		metricsCertPath, metricsCertName, metricsCertKey string
		webhookCertPath, webhookCertName, webhookCertKey string
		enableLeaderElection                             bool
		probeAddr                                        string
		secureMetrics                                    bool
		enableHTTP2                                      bool
		leaseDuration, renewDeadline, retryPeriod        time.Duration
		configFile                                       string
		enableValidationController                       bool
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", false,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers.")
	flag.DurationVar(&leaseDuration, "lease-duration", 90*time.Second,
		"The duration that non-leader candidates will wait to force acquire leadership.")
	flag.DurationVar(&renewDeadline, "renew-deadline", 60*time.Second,
		"The duration that the acting leader will retry refreshing leadership before giving up.")
	flag.DurationVar(&retryPeriod, "retry-period", 5*time.Second,
		"The duration LeaderElector clients should wait between tries of actions.")
	flag.StringVar(&configFile, "config", "", "Path to a ValidationConfiguration file.")
	flag.BoolVar(&enableValidationController, "enable-validation-controller", true,
		"Enable the ValidationRequest controller and webhook.")

	flag.Parse()

	var cfg *config.Config

	if enableValidationController {
		var err error

		cfg, err = config.LoadConfig(configFile)
		if err != nil {
			slog.Error("Failed to load configuration", "error", err)

			return err
		}
	}

	setup, err := setupTLSAndServers(enableHTTP2, webhookCertPath, webhookCertName, webhookCertKey, metricsAddr,
		secureMetrics, metricsCertPath, metricsCertName, metricsCertKey)
	if err != nil {
		return err
	}

	restConfig := ctrl.GetConfigOrDie()
	restConfig.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return auditlogger.NewAuditingRoundTripper(rt)
	})

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                setup.metricsServerOptions,
		WebhookServer:          setup.webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "lifecycle-manager.nvsentinel.nvidia.com",
		LeaseDuration:          &leaseDuration,
		RenewDeadline:          &renewDeadline,
		RetryPeriod:            &retryPeriod,
	})
	if err != nil {
		slog.Error("Failed to start manager", "error", err)

		return err
	}

	if err := addCertWatchers(mgr, setup); err != nil {
		slog.Error("Failed to add certificate watchers", "error", err)

		return err
	}

	if err := setupControllers(mgr, cfg, enableValidationController); err != nil {
		slog.Error("Failed to set up controllers", "error", err)

		return err
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		slog.Error("Failed to set up health check", "error", err)

		return err
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		slog.Error("Failed to set up ready check", "error", err)

		return err
	}

	slog.Info("Starting manager")

	return mgr.Start(ctrl.SetupSignalHandler())
}

func main() {
	logger.SetDefaultStructuredLoggerWithTraceCorrelation("lifecycle-manager", version)
	slog.Info("Starting lifecycle-manager", "version", version, "commit", commit, "date", date)

	if err := auditlogger.InitAuditLogger("lifecycle-manager"); err != nil {
		slog.Warn("Failed to initialize audit logger", "error", err)
	}

	if err := tracing.InitTracing("lifecycle-manager"); err != nil {
		slog.Warn("Failed to initialize tracing", "error", err)
	}

	ctrllog.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))

	runErr := run()

	tracingCtx, tracingCancel := context.WithTimeout(context.Background(), 5*time.Second)

	if err := tracing.ShutdownTracing(tracingCtx); err != nil {
		slog.Warn("Failed to shutdown tracing", "error", err)
	}

	tracingCancel()

	if runErr != nil {
		slog.Error("Application encountered a fatal error", "error", runErr)

		if closeErr := auditlogger.CloseAuditLogger(); closeErr != nil {
			slog.Warn("Failed to close audit logger", "error", closeErr)
		}

		os.Exit(1)
	}

	if err := auditlogger.CloseAuditLogger(); err != nil {
		slog.Warn("Failed to close audit logger", "error", err)
	}
}
