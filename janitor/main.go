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
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"
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

	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	metrics "github.com/nvidia/nvsentinel/commons/pkg/metrics"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
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

// runFlags holds all CLI flags for the janitor process.
type runFlags struct {
	metricsAddr                                      string
	metricsCertPath, metricsCertName, metricsCertKey string
	webhookCertPath, webhookCertName, webhookCertKey string
	probeAddr                                        string
	configAddr                                       string
	enableLeaderElection                             bool
	secureMetrics                                    bool
	enableHTTP2                                      bool
	configFile                                       string
	leaseDuration                                    time.Duration
	renewDeadline                                    time.Duration
	retryPeriod                                      time.Duration
}

// serverSetup holds the webhook server, metrics options, and optional cert watchers
// produced by setupTLSAndServers.
type serverSetup struct {
	webhookServer        webhook.Server
	metricsServerOptions metricsserver.Options
	metricsCertWatcher   *certwatcher.CertWatcher
	webhookCertWatcher   *certwatcher.CertWatcher
}

func main() {
	logger.SetDefaultStructuredLogger("janitor", version)
	slog.Info("Starting janitor", "version", version, "commit", commit, "date", date)

	if err := auditlogger.InitAuditLogger("janitor"); err != nil {
		slog.Warn("Failed to initialize audit logger", "error", err)
	}

	slogHandler := slog.Default().Handler()
	logrLogger := logr.FromSlogHandler(slogHandler)
	ctrllog.SetLogger(logrLogger)

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

//nolint:cyclop // main wiring: linear setup; complexity from sequential error checks
func run() error {
	// 1. Parse flags and resolve pod namespace
	flags := parseFlags()
	podNamespace := resolvePodNamespace()

	// 2. Load configuration
	cfg, err := config.LoadConfig(flags.configFile, podNamespace)
	if err != nil {
		slog.Error("Unable to load configuration", "error", err)

		return err
	}

	ff := metrics.NewRegistry("janitor",
		metrics.WithRegisterer(crmetrics.Registry),
	)
	ff.Set("manual_mode", cfg.Global.ManualMode != nil && *cfg.Global.ManualMode)
	ff.Set("controller_reboot_node_enabled", cfg.RebootNode.Enabled)
	ff.Set("controller_terminate_node_enabled", cfg.TerminateNode.Enabled)
	ff.Set("controller_gpu_reset_enabled", cfg.GPUReset.Enabled)
	ff.Set("csp_provider_auth_enabled", cfg.Global.CSPProviderTokenPath != "")

	// 3. Setup config server (port, handler, server)
	configServer, configPort, err := setupConfigServer(cfg, flags.configAddr)
	if err != nil {
		return err
	}

	// 4. Setup TLS options, webhook server, and metrics options (including cert watchers)
	setup, err := setupTLSAndServers(flags)
	if err != nil {
		return err
	}

	// 5. Create controller manager
	restConfig := ctrl.GetConfigOrDie()
	restConfig.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return auditlogger.NewAuditingRoundTripper(rt)
	})

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                setup.metricsServerOptions,
		WebhookServer:          setup.webhookServer,
		HealthProbeBindAddress: flags.probeAddr,
		LeaderElection:         flags.enableLeaderElection,
		LeaderElectionID:       "janitor.dgxc.nvidia.com",
		LeaseDuration:          &flags.leaseDuration,
		RenewDeadline:          &flags.renewDeadline,
		RetryPeriod:            &flags.retryPeriod,
	})
	if err != nil {
		slog.Error("Unable to create manager", "error", err)

		return err
	}

	slog.Info("Manager created successfully")

	// 6. Configure field indexers and register controllers
	if err = controller.ConfigureFieldIndexers(mgr, cfg); err != nil {
		slog.Error("Unable to configure field indexers", "error", err)

		return err
	}

	if err = (&controller.RebootNodeReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Config:        &cfg.RebootNode,
		LockNamespace: podNamespace,
	}).SetupWithManager(mgr); err != nil {
		slog.Error("Unable to create controller", "controller", "RebootNode", "error", err)

		return err
	}

	if err = (&controller.TerminateNodeReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Config:        &cfg.TerminateNode,
		LockNamespace: podNamespace,
	}).SetupWithManager(mgr); err != nil {
		slog.Error("Unable to create controller", "controller", "TerminateNode", "error", err)

		return err
	}

	if err = (&controller.GPUResetReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Config:        &cfg.GPUReset,
		LockNamespace: podNamespace,
	}).SetupWithManager(mgr); err != nil {
		slog.Error("unable to create controller", "controller", "GPUReset", "error", err)

		return err
	}

	slog.Info("RebootNode, TerminateNode, and GPUReset controllers registered")

	// 7. Register webhook
	if err = webhookv1alpha1.SetupJanitorWebhookWithManager(mgr, cfg); err != nil {
		slog.Error("Unable to create webhook", "webhook", "Janitor", "error", err)

		return err
	}

	slog.Info("Janitor validation webhook registered for all CRDs")

	// 8. Add certificate watchers to manager
	if setup.metricsCertWatcher != nil {
		slog.Info("Adding metrics certificate watcher to manager")

		if err := mgr.Add(setup.metricsCertWatcher); err != nil {
			slog.Error("Unable to add metrics certificate watcher to manager", "error", err)

			return err
		}
	}

	if setup.webhookCertWatcher != nil {
		slog.Info("Adding webhook certificate watcher to manager")

		if err := mgr.Add(setup.webhookCertWatcher); err != nil {
			slog.Error("Unable to add webhook certificate watcher to manager", "error", err)

			return err
		}
	}

	// 9. Setup health checks
	if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		slog.Error("Unable to set up health check", "error", err)

		return err
	}

	if err = mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		slog.Error("Unable to set up ready check", "error", err)

		return err
	}

	// 10. Start config server and manager, wait for shutdown
	ctx := ctrl.SetupSignalHandler()
	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		slog.Info("Starting config server", "port", configPort)

		return configServer.Serve(gCtx)
	})

	g.Go(func() error {
		slog.Info("Starting manager")

		return mgr.Start(gCtx)
	})

	return g.Wait()
}

func parseFlags() runFlags {
	var rf runFlags

	flag.StringVar(&rf.metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&rf.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&rf.configAddr, "config-bind-address", ":8082", "The address the config endpoint binds to.")
	flag.BoolVar(&rf.enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&rf.secureMetrics, "metrics-secure", false,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&rf.webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&rf.webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&rf.webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&rf.metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&rf.metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&rf.metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&rf.enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&rf.configFile, "config", "", "The path to the configuration file.")

	// Leader election flags: defaulting to high values; janitor is not sensitive to slow leader transitions.
	flag.DurationVar(&rf.leaseDuration, "lease-duration", 90*time.Second,
		"The duration that non-leader candidates will wait to force acquire leadership.")
	flag.DurationVar(&rf.renewDeadline, "renew-deadline", 60*time.Second,
		"The duration that the acting controlplane will retry refreshing leadership before giving up.")
	flag.DurationVar(&rf.retryPeriod, "retry-period", 5*time.Second,
		"The duration the LeaderElector clients should wait between tries of actions.")

	flag.Parse()

	slog.Info("Parsed flags",
		"metrics-bind-address", rf.metricsAddr,
		"health-probe-bind-address", rf.probeAddr,
		"config-bind-address", rf.configAddr,
		"leader-elect", rf.enableLeaderElection,
		"config", rf.configFile,
		"secure-metrics", rf.secureMetrics,
		"enable-http2", rf.enableHTTP2,
		"webhook-cert-path", rf.webhookCertPath,
		"webhook-cert-name", rf.webhookCertName,
		"webhook-cert-key", rf.webhookCertKey,
		"metrics-cert-path", rf.metricsCertPath,
		"metrics-cert-name", rf.metricsCertName,
		"metrics-cert-key", rf.metricsCertKey,
		"lease-duration", rf.leaseDuration,
		"renew-deadline", rf.renewDeadline,
		"retry-period", rf.retryPeriod)

	return rf
}

func resolvePodNamespace() string {
	podNamespace := os.Getenv("POD_NAMESPACE")

	if podNamespace == "" {
		slog.Warn("POD_NAMESPACE not set, defaulting to 'nvsentinel'")

		podNamespace = "nvsentinel"
	}

	slog.Info("Using namespace for distributed locking and GPU reset resources", "namespace", podNamespace)

	return podNamespace
}

func setupConfigServer(cfg *config.Config, configAddr string) (server.Server, int, error) {
	_, portStr, err := net.SplitHostPort(configAddr)
	if err != nil {
		portStr = configAddr

		if portStr != "" && portStr[0] == ':' {
			portStr = portStr[1:]
		}
	}

	configPort, err := strconv.Atoi(portStr)
	if err != nil {
		slog.Error("Invalid config-bind-address port", "error", err, "address", configAddr)

		return nil, 0, fmt.Errorf("invalid config-bind-address port %q: %w", configAddr, err)
	}

	configHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(cfg); err != nil {
			slog.Error("Failed to encode configuration as JSON", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)

			return
		}
	})

	configServer := server.NewServer(
		server.WithPort(configPort),
		server.WithHandler("/config", configHandler),
	)

	return configServer, configPort, nil
}

func setupTLSAndServers(rf runFlags) (serverSetup, error) {
	var tlsOpts []func(*tls.Config)

	if !rf.enableHTTP2 {
		disableHTTP2 := func(c *tls.Config) { c.NextProtos = []string{"http/1.1"} }
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	var result serverSetup

	webhookTLSOpts := append([]func(*tls.Config){}, tlsOpts...)

	if len(rf.webhookCertPath) > 0 {
		slog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", rf.webhookCertPath,
			"webhook-cert-name", rf.webhookCertName,
			"webhook-cert-key", rf.webhookCertKey)

		webhookCertWatcher, err := certwatcher.New(
			filepath.Join(rf.webhookCertPath, rf.webhookCertName),
			filepath.Join(rf.webhookCertPath, rf.webhookCertKey),
		)
		if err != nil {
			slog.Error("Failed to initialize webhook certificate watcher", "error", err)

			return serverSetup{}, fmt.Errorf("failed to initialize webhook certificate watcher: %w", err)
		}

		result.webhookCertWatcher = webhookCertWatcher

		webhookTLSOpts = append(webhookTLSOpts, func(c *tls.Config) {
			c.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	result.webhookServer = webhook.NewServer(webhook.Options{TLSOpts: webhookTLSOpts})

	result.metricsServerOptions = metricsserver.Options{
		BindAddress:   rf.metricsAddr,
		SecureServing: rf.secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if rf.secureMetrics {
		result.metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if len(rf.metricsCertPath) > 0 {
		slog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", rf.metricsCertPath,
			"metrics-cert-name", rf.metricsCertName,
			"metrics-cert-key", rf.metricsCertKey)

		metricsCertWatcher, err := certwatcher.New(
			filepath.Join(rf.metricsCertPath, rf.metricsCertName),
			filepath.Join(rf.metricsCertPath, rf.metricsCertKey),
		)
		if err != nil {
			slog.Error("Failed to initialize metrics certificate watcher", "error", err)

			return serverSetup{}, fmt.Errorf("failed to initialize metrics certificate watcher: %w", err)
		}

		result.metricsCertWatcher = metricsCertWatcher
		result.metricsServerOptions.TLSOpts = append(result.metricsServerOptions.TLSOpts,
			func(c *tls.Config) { c.GetCertificate = metricsCertWatcher.GetCertificate })
	}

	return result, nil
}
