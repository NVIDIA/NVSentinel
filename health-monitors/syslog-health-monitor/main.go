// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"flag"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"

	pb "github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/protos"
	fd "github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/syslog-monitor"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/yaml.v3"
)

const (
	defaultAgentName       = "syslog-health-monitor"
	defaultComponentClass  = "GPU"                                // Or a more specific class if applicable
	defaultPollingInterval = "30m"                                // Added default polling interval
	defaultStateFilePath   = "/var/run/syslog_monitor/state.json" // Added default state file path
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// ConfigFile matches the top-level structure of the YAML config file
type ConfigFile struct {
	Checks []fd.CheckDefinition `yaml:"checks"`
}

//nolint:cyclop,gocognit // todo
func main() {
	// Initialize klog flags to allow command-line control (e.g., -v=3)
	klog.InitFlags(nil)

	configFile := flag.String("config-file", "/etc/config/config.yaml",
		"Path to the YAML configuration file for log checks.")
	platformConnectorSocket := flag.String("platform-connector-socket", "unix:///var/run/nvsentinel.sock",
		"Path to the platform-connector UDS socket.")
	nodeNameEnv := flag.String("node-name", os.Getenv("NODE_NAME"), "Node name. Defaults to NODE_NAME env var.")
	pollingIntervalFlag := flag.String("polling-interval", defaultPollingInterval,
		"Polling interval for health checks (e.g., 15m, 1h).")
	stateFileFlag := flag.String("state-file", defaultStateFilePath,
		"Path to state file for cursor persistence.")
	metricsPort := flag.String("metrics-port", "2112", "Port to expose Prometheus metrics on")
	xidAnalyserEndpoint := flag.String("xid-analyser-endpoint", "",
		"Endpoint to the XID analyser service.")

	flag.Parse()

	logger := textlogger.NewLogger(textlogger.NewConfig()).WithValues(
		"version", version,
		"module", "syslog-health-monitor",
	)

	klog.SetLogger(logger)
	klog.InfoS("Starting syslog-health-monitor", "version", version, "commit", commit, "date", date)
	defer klog.Flush()

	klog.Infof("Parsed command line flags successfully")

	nodeName := *nodeNameEnv
	if nodeName == "" {
		klog.Fatalf("NODE_NAME env not set and --node-name flag not provided, cannot run.")
	}

	klog.Infof("Using node name: %s", nodeName)

	var opts []grpc.DialOption
	opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))

	klog.Infof("Creating gRPC client to platform connector at: %s", *platformConnectorSocket)

	// Add retry logic for platform connector socket with detailed diagnostics
	var conn *grpc.ClientConn

	var err error

	maxRetries := 10
	for attempt := 1; attempt <= maxRetries; attempt++ {
		klog.Infof("Attempt %d/%d: Checking platform connector socket availability at %s",
			attempt, maxRetries, *platformConnectorSocket)

		// Check if socket file exists before attempting connection
		socketPath := strings.TrimPrefix(*platformConnectorSocket, "unix://")
		if _, statErr := os.Stat(socketPath); statErr != nil {
			klog.Warningf("Attempt %d/%d: Platform connector socket file does not exist: %v", attempt, maxRetries, statErr)

			if attempt < maxRetries {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}

			klog.Errorf("Platform connector socket file not found after %d attempts: %s", maxRetries, socketPath)
			klog.Flush()

			//nolint:gocritic // Running klog.Flush() to ensure logs are flushed
			os.Exit(1)
		}

		conn, err = grpc.NewClient(*platformConnectorSocket, opts...)
		if err != nil {
			klog.Warningf("Attempt %d/%d: Error creating gRPC client: %v", attempt, maxRetries, err)

			if attempt < maxRetries {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}

			klog.Fatalf("Failed to create gRPC client after %d attempts: %v", maxRetries, err)
		}

		klog.Infof("Successfully connected to platform connector on attempt %d", attempt)

		break
	}

	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			klog.Errorf("Error closing gRPC connection: %v", closeErr)
		}
	}()

	client := pb.NewPlatformConnectorClient(conn)

	klog.Infof("Loading checks from config file: %s", *configFile)

	// Add retry logic for config file reading with detailed diagnostics
	var yamlFile []byte

	maxConfigRetries := 5
	for attempt := 1; attempt <= maxConfigRetries; attempt++ {
		klog.Infof("Attempt %d/%d: Reading config file: %s", attempt, maxConfigRetries, *configFile)

		// Check if config file exists
		if _, statErr := os.Stat(*configFile); statErr != nil {
			klog.Warningf("Attempt %d/%d: Config file does not exist: %v", attempt, maxConfigRetries, statErr)

			if attempt < maxConfigRetries {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}

			klog.Fatalf("Config file not found after %d attempts: %s", maxConfigRetries, *configFile)
		}

		yamlFile, err = os.ReadFile(*configFile)

		if err != nil {
			klog.Warningf("Attempt %d/%d: Error reading config file: %v", attempt, maxConfigRetries, err)

			if attempt < maxConfigRetries {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}

			klog.Fatalf("Failed to read config file after %d attempts: %v", maxConfigRetries, err)
		}

		klog.Infof("Successfully read config file on attempt %d", attempt)

		break
	}

	var config ConfigFile

	err = yaml.Unmarshal(yamlFile, &config)
	if err != nil {
		klog.Fatalf("Error unmarshalling config file '%s': %v", *configFile, err)
	}

	if len(config.Checks) == 0 {
		klog.Fatalln("Error: No checks defined in the config file.")
	}

	klog.Infof("Creating syslog monitor with %d checks", len(config.Checks))

	fdHealthMonitor, err := fd.NewSyslogMonitor(nodeName, config.Checks, client, defaultAgentName,
		defaultComponentClass, *pollingIntervalFlag, *stateFileFlag, *xidAnalyserEndpoint)
	if err != nil {
		klog.Fatalf("Error creating syslog health monitor: %v", err)
	}

	// Parse polling interval
	pollingInterval, err := time.ParseDuration(*pollingIntervalFlag)
	if err != nil {
		klog.Fatalf("Error parsing polling interval '%s': %v", *pollingIntervalFlag, err)
	}

	klog.Infof("Polling every %v", pollingInterval)

	// Start metrics server
	klog.Infof("Starting metrics server on port %s", *metricsPort)

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		//nolint:gosec // G114: Ignoring the use of http.ListenAndServe without timeouts
		err := http.ListenAndServe(":"+*metricsPort, nil)
		if err != nil {
			klog.Fatalf("Failed to start metrics server: %v", err)
		}
	}()

	ticker := time.NewTicker(pollingInterval)
	defer ticker.Stop()

	klog.Infof("config.checks: %v", config.Checks)

	klog.Infof("Syslog health monitor initialization complete, starting polling loop...")
	// Polling loop
	for range ticker.C {
		klog.Info("Performing scheduled health check run...")

		if err := fdHealthMonitor.Run(); err != nil {
			klog.Errorf("Error running syslog health monitor: %v", err)
		}
	}
}
