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
	"os"
	"time"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/commons/pkg/flags"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/bootstrap"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/central"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/nodelocal"
	_ "github.com/nvidia/nvsentinel/platform-connectors/pkg/transformers/dedup"
	_ "github.com/nvidia/nvsentinel/platform-connectors/pkg/transformers/metadata"
	_ "github.com/nvidia/nvsentinel/platform-connectors/pkg/transformers/overrides"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// flagValues are the process settings: where to listen, where the config
// file is, and the datastore certificate flags shared with the other
// components. Everything tunable lives in the config file.
type flagValues struct {
	// mode selects the role: "node-local" (the DaemonSet, serving a Unix
	// socket) or "deployment" (the central Deployment, serving TCP with TLS).
	mode           string
	socket         string
	configPath     string
	metricsPort    int
	kubeconfigPath string
	// The datastore client certificate directory, from the shared flags.
	certMountPath string
	// The deployment role's listener (-mode=deployment).
	listenAddr                 string
	tlsCertDir                 string
	tlsInsecureDevelopmentMode bool
}

func parseFlags() flagValues {
	var f flagValues

	flag.StringVar(&f.mode, "mode", modeNodeLocal,
		"role of this process: node-local (the DaemonSet, a Unix socket) or deployment (the central Deployment)")
	flag.StringVar(&f.socket, "socket", "", "unix socket path the node-local role serves on")
	flag.StringVar(&f.configPath, "config", "/etc/config/config.json", "path to the config file")
	flag.IntVar(&f.metricsPort, "metrics-port", 2112, "port to expose Prometheus metrics and the probes on")
	flag.StringVar(&f.kubeconfigPath, "kubeconfig", "", "path to a kubeconfig file for out-of-cluster Kubernetes auth")
	flag.StringVar(&f.listenAddr, "listen-addr", ":50051", "TCP address the deployment role serves on")
	flag.StringVar(&f.tlsCertDir, "tls-cert-dir", "",
		"directory with tls.crt and tls.key for the deployment role's listener")
	flag.BoolVar(&f.tlsInsecureDevelopmentMode, "tls-insecure-development-mode", false,
		"serve the deployment role in plaintext; never use this outside development")

	certConfig := flags.RegisterDatabaseCertFlags()

	flag.Parse()

	f.certMountPath = certConfig.ResolveCertPath()

	return f
}

const (
	modeNodeLocal  = "node-local"
	modeDeployment = "deployment"
)

// newRole builds the role -mode selects.
func newRole(f flagValues) (bootstrap.Role, error) {
	switch f.mode {
	case modeDeployment:
		return central.New(central.Options{
			ListenAddr:              f.listenAddr,
			TLSCertDir:              f.tlsCertDir,
			InsecureDevelopmentMode: f.tlsInsecureDevelopmentMode,
		})
	case modeNodeLocal:
		return nodelocal.New(nodelocal.Options{Socket: f.socket})
	default:
		return nil, fmt.Errorf("unknown -mode %q: use %q or %q", f.mode, modeNodeLocal, modeDeployment)
	}
}

// One image, two roles. The process setup here is shared; the roles differ
// in what they hand bootstrap.Run. The DaemonSet keeps the tracing service
// name it has always had.
func main() {
	f := parseFlags()

	appName, tracingName := "platform-connectors", "platform-connector"
	if f.mode == modeDeployment {
		appName, tracingName = central.AppName, central.AppName
	}

	// The logger comes first so a configuration error is reported the same
	// way as every other line.
	logger.SetDefaultStructuredLoggerWithTraceCorrelation(appName, version)
	// controller-runtime's certificate watchers log through logr; without a
	// sink they drop their lines and print a "SetLogger(...) was never
	// called" warning with a stack trace.
	ctrllog.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))

	ctx := context.Background()

	role, err := newRole(f)
	if err != nil {
		slog.ErrorContext(ctx, "Invalid configuration", "error", err)
		os.Exit(1)
	}

	slog.InfoContext(ctx, "Starting "+appName, "version", version, "commit", commit, "date", date)

	if err := auditlogger.InitAuditLogger(appName); err != nil {
		slog.WarnContext(ctx, "Failed to initialize audit logger", "error", err)
	}

	if err := tracing.InitTracing(tracingName); err != nil {
		slog.WarnContext(ctx, "Failed to initialize tracing", "error", err)
	}

	err = bootstrap.Run(ctx, role, bootstrap.Options{
		ConfigPath:     f.configPath,
		MetricsPort:    f.metricsPort,
		KubeconfigPath: f.kubeconfigPath,
		CertMountPath:  f.certMountPath,
	})
	if err != nil {
		slog.ErrorContext(ctx, appName+" exited with error", "error", err)
	}

	shutdownTracing(ctx)

	if err := auditlogger.CloseAuditLogger(); err != nil {
		slog.WarnContext(ctx, "Failed to close audit logger", "error", err)
	}

	if err != nil {
		os.Exit(1)
	}
}

func shutdownTracing(ctx context.Context) {
	tracingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := tracing.ShutdownTracing(tracingCtx); err != nil {
		slog.WarnContext(ctx, "Failed to shutdown tracing", "error", err)
	}
}
