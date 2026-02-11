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

	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
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

func parseFlags() runFlags {
	var f runFlags

	flag.StringVar(&f.metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&f.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&f.configAddr, "config-bind-address", ":8082", "The address the config endpoint binds to.")
	flag.BoolVar(&f.enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&f.secureMetrics, "metrics-secure", false,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&f.webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&f.webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&f.webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&f.metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&f.metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&f.metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&f.enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&f.configFile, "config", "", "The path to the configuration file.")

	// Leader election flags: defaulting to high values; janitor is not sensitive to slow leader transitions.
	flag.DurationVar(&f.leaseDuration, "lease-duration", 90*time.Second,
		"The duration that non-leader candidates will wait to force acquire leadership.")
	flag.DurationVar(&f.renewDeadline, "renew-deadline", 60*time.Second,
		"The duration that the acting controlplane will retry refreshing leadership before giving up.")
	flag.DurationVar(&f.retryPeriod, "retry-period", 5*time.Second,
		"The duration the LeaderElector clients should wait between tries of actions.")

	flag.Parse()

	slog.Info("Parsed flags",
		"metrics-bind-address", f.metricsAddr,
		"health-probe-bind-address", f.probeAddr,
		"config-bind-address", f.configAddr,
		"leader-elect", f.enableLeaderElection,
		"config", f.configFile,
		"secure-metrics", f.secureMetrics)

	return f
}

func resolvePodNamespace() string {
	ns := os.Getenv("POD_NAMESPACE")

	if ns == "" {
		slog.Warn("POD_NAMESPACE not set, defaulting to 'nvsentinel'")

		ns = "nvsentinel"
	}

	slog.Info("Using namespace for distributed locking and GPU reset resources", "namespace", ns)

	return ns
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

	configHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// tlsAndServers holds the webhook server, metrics options, and optional cert watchers.
type tlsAndServers struct {
	webhookServer        webhook.Server
	metricsServerOptions metricsserver.Options
	metricsCertWatcher   *certwatcher.CertWatcher
	webhookCertWatcher   *certwatcher.CertWatcher
}

func setupTLSAndServers(f runFlags) (tlsAndServers, error) {
	var tlsOpts []func(*tls.Config)

	if !f.enableHTTP2 {
		disableHTTP2 := func(c *tls.Config) { c.NextProtos = []string{"http/1.1"} }
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	var out tlsAndServers

	webhookTLSOpts := tlsOpts

	if len(f.webhookCertPath) > 0 {
		slog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", f.webhookCertPath,
			"webhook-cert-name", f.webhookCertName,
			"webhook-cert-key", f.webhookCertKey)

		w, err := certwatcher.New(
			filepath.Join(f.webhookCertPath, f.webhookCertName),
			filepath.Join(f.webhookCertPath, f.webhookCertKey),
		)
		if err != nil {
			slog.Error("Failed to initialize webhook certificate watcher", "error", err)

			return tlsAndServers{}, err
		}

		out.webhookCertWatcher = w

		webhookTLSOpts = append(webhookTLSOpts, func(c *tls.Config) { c.GetCertificate = w.GetCertificate })
	}

	out.webhookServer = webhook.NewServer(webhook.Options{TLSOpts: webhookTLSOpts})

	out.metricsServerOptions = metricsserver.Options{
		BindAddress:   f.metricsAddr,
		SecureServing: f.secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if f.secureMetrics {
		out.metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if len(f.metricsCertPath) > 0 {
		slog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", f.metricsCertPath,
			"metrics-cert-name", f.metricsCertName,
			"metrics-cert-key", f.metricsCertKey)

		m, err := certwatcher.New(
			filepath.Join(f.metricsCertPath, f.metricsCertName),
			filepath.Join(f.metricsCertPath, f.metricsCertKey),
		)
		if err != nil {
			slog.Error("Failed to initialize metrics certificate watcher", "error", err)

			return tlsAndServers{}, err
		}

		out.metricsCertWatcher = m
		out.metricsServerOptions.TLSOpts = append(out.metricsServerOptions.TLSOpts,
			func(c *tls.Config) { c.GetCertificate = m.GetCertificate })
	}

	return out, nil
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

	// 3. Setup config server (port, handler, server)
	configServer, configPort, err := setupConfigServer(cfg, flags.configAddr)
	if err != nil {
		return err
	}

	// 4. Setup TLS options, webhook server, and metrics options (including cert watchers)
	tlsAndSrv, err := setupTLSAndServers(flags)
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
		Metrics:                tlsAndSrv.metricsServerOptions,
		WebhookServer:          tlsAndSrv.webhookServer,
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
	if tlsAndSrv.metricsCertWatcher != nil {
		slog.Info("Adding metrics certificate watcher to manager")

		if err := mgr.Add(tlsAndSrv.metricsCertWatcher); err != nil {
			slog.Error("Unable to add metrics certificate watcher to manager", "error", err)

			return err
		}
	}

	if tlsAndSrv.webhookCertWatcher != nil {
		slog.Info("Adding webhook certificate watcher to manager")

		if err := mgr.Add(tlsAndSrv.webhookCertWatcher); err != nil {
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

	if err := g.Wait(); err != nil {
		return err
	}

	return nil
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
