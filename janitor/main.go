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
	"crypto/tls"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	janitordgxcnvidiacomv1alpha1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/config"
	"github.com/nvidia/nvsentinel/janitor/pkg/controller"
	webhookv1alpha1 "github.com/nvidia/nvsentinel/janitor/pkg/webhook/v1alpha1"
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

// nolint:cyclop,gocyclo,gocognit
func run() error {
	var (
		metricsAddr                                      string
		metricsCertPath, metricsCertName, metricsCertKey string
		webhookCertPath, webhookCertName, webhookCertKey string
		probeAddr                                        string
		configAddr                                       string
		enableLeaderElection                             bool
		secureMetrics                                    bool
		enableHTTP2                                      bool
		configFile                                       string
		// Leader election tuning parameters
		leaseDuration time.Duration
		renewDeadline time.Duration
		retryPeriod   time.Duration
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&configAddr, "config-bind-address", ":8082", "The address the config endpoint binds to.")
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
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&configFile, "config", "", "The path to the configuration file.")

	// Leader election flags
	// Defaulting to pretty high values, we were hitting some crashes
	// with the default values. Janitor as a project is not sensitive
	// to slow leader transitions so high defaults are okay.
	flag.DurationVar(&leaseDuration, "lease-duration", 90*time.Second,
		"The duration that non-leader candidates will wait to force acquire leadership.")
	flag.DurationVar(&renewDeadline, "renew-deadline", 60*time.Second,
		"The duration that the acting controlplane will retry refreshing leadership before giving up.")
	flag.DurationVar(&retryPeriod, "retry-period", 5*time.Second,
		"The duration the LeaderElector clients should wait between tries of actions.")

	flag.Parse()

	slog.Info("Parsed flags",
		"metrics-bind-address", metricsAddr,
		"health-probe-bind-address", probeAddr,
		"config-bind-address", configAddr,
		"leader-elect", enableLeaderElection,
		"config", configFile,
		"secure-metrics", secureMetrics)

	// Load configuration from file
	cfg, err := config.LoadConfig(configFile)
	if err != nil {
		slog.Error("Unable to load configuration", "error", err)
		return err
	}

	slog.Info("Loaded configuration",
		"rebootNode.enabled", cfg.RebootNode.Enabled,
		"rebootNode.timeout", cfg.RebootNode.Timeout,
		"terminateNode.enabled", cfg.TerminateNode.Enabled,
		"terminateNode.timeout", cfg.TerminateNode.Timeout,
		"global.manualMode", cfg.Global.ManualMode)

	// Start a simple HTTP server for the /config endpoint
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")

			if err := json.NewEncoder(w).Encode(cfg); err != nil {
				slog.Error("Failed to encode configuration as JSON", "error", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)

				return
			}
		})

		slog.Info("Starting config HTTP server", "address", configAddr)

		// nolint:gosec // G114: Config endpoint is for internal debugging/monitoring
		if err := http.ListenAndServe(configAddr, mux); err != nil {
			slog.Error("Config HTTP server failed", "error", err)
		}
	}()

	// Setup TLS options
	var tlsOpts []func(*tls.Config)

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	if !enableHTTP2 {
		disableHTTP2 := func(c *tls.Config) {
			c.NextProtos = []string{"http/1.1"}
		}
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		slog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath,
			"webhook-cert-name", webhookCertName,
			"webhook-cert-key", webhookCertKey)

		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			slog.Error("Failed to initialize webhook certificate watcher", "error", err)
			return err
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	// Metrics endpoint configuration
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in the Helm chart.
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	if len(metricsCertPath) > 0 {
		slog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath,
			"metrics-cert-name", metricsCertName,
			"metrics-cert-key", metricsCertKey)

		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			slog.Error("Failed to initialize metrics certificate watcher", "error", err)
			return err
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	// Setup controller manager
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "janitor.dgxc.nvidia.com",
		LeaseDuration:          &leaseDuration,
		RenewDeadline:          &renewDeadline,
		RetryPeriod:            &retryPeriod,
	})
	if err != nil {
		slog.Error("Unable to create manager", "error", err)
		return err
	}

	slog.Info("Manager created successfully")

	// Setup RebootNode controller
	if err = (&controller.RebootNodeReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Config: &cfg.RebootNode,
	}).SetupWithManager(mgr); err != nil {
		slog.Error("Unable to create controller", "controller", "RebootNode", "error", err)
		return err
	}

	// Setup TerminateNode controller
	if err = (&controller.TerminateNodeReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Config: &cfg.TerminateNode,
	}).SetupWithManager(mgr); err != nil {
		slog.Error("Unable to create controller", "controller", "TerminateNode", "error", err)
		return err
	}

	slog.Info("RebootNode and TerminateNode controllers registered")

	// Setup unified webhook for all Janitor CRDs
	if err = webhookv1alpha1.SetupJanitorWebhookWithManager(mgr, cfg); err != nil {
		slog.Error("Unable to create webhook", "webhook", "Janitor", "error", err)
		return err
	}

	slog.Info("Janitor validation webhook registered for all CRDs")

	// Add certificate watchers to manager if configured
	if metricsCertWatcher != nil {
		slog.Info("Adding metrics certificate watcher to manager")

		if err := mgr.Add(metricsCertWatcher); err != nil {
			slog.Error("Unable to add metrics certificate watcher to manager", "error", err)
			return err
		}
	}

	if webhookCertWatcher != nil {
		slog.Info("Adding webhook certificate watcher to manager")

		if err := mgr.Add(webhookCertWatcher); err != nil {
			slog.Error("Unable to add webhook certificate watcher to manager", "error", err)
			return err
		}
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
