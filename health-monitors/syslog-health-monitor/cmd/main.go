// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
	"crypto/tls"
	"crypto/x509"
	"flag"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/klog/v2"

	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/protos"
	fd "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/syslog-monitor"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/yaml.v3"
)

const (
	defaultAgentName         = "syslog-health-monitor"
	defaultComponentClass    = "GPU"                                       // Or a more specific class if applicable
	defaultPollingInterval   = "30m"                                       // Added default polling interval
	defaultStateFilePath     = "/var/run/syslog_monitor/state.json"        // Added default state file path
	defaultXidMappingPath    = "/etc/syslog-monitor/xiderrormappings.csv"  // Default path for XID error mappings
	defaultSXidMappingPath   = "/etc/syslog-monitor/sxiderrormappings.csv" // Default path for SXID error mappings
	defaultActionMappingPath = "/etc/syslog-monitor/actionmapping.ini"     // Default path for action mappings
)

// ConfigFile matches the top-level structure of the YAML config file
type ConfigFile struct {
	Checks []fd.CheckDefinition `yaml:"checks"`
}

// createTLSCredentials creates TLS credentials if appropriate, returns credentials and whether to use TLS
func createTLSCredentials(endpoint string) (credentials.TransportCredentials, bool) {
	// Use TLS for TCP endpoints (kata mode) when CA certificate is available
	if strings.HasPrefix(endpoint, "unix://") {
		// Unix socket - no TLS needed
		return nil, false
	}

	// Check if TLS CA certificate exists (mounted in kata mode)
	caCertPath := "/etc/nvsentinel/certs/ca.crt"
	if _, err := os.Stat(caCertPath); err != nil {
		klog.Warningf("TLS CA certificate not found at %s, falling back to insecure connection", caCertPath)
		return nil, false
	}

	// Load CA certificate
	caCert, err := os.ReadFile(caCertPath)
	if err != nil {
		klog.Errorf("Failed to read CA certificate from %s: %v, falling back to insecure connection", caCertPath, err)
		return nil, false
	}

	// Create certificate pool and add our CA
	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		klog.Errorf("Failed to parse CA certificate from %s, falling back to insecure connection", caCertPath)
		return nil, false
	}

	// Create TLS credentials with custom CA
	tlsConfig := &tls.Config{
		RootCAs:    caCertPool,
		MinVersion: tls.VersionTLS13,
	}

	klog.Infof("Created TLS credentials with custom CA")

	return credentials.NewTLS(tlsConfig), true
}

//nolint:cyclop,gocognit // todo
func main() {
	// Initialize klog early to ensure logging works
	klog.InitFlags(nil)
	defer klog.Flush()

	// Early startup logging to help diagnose issues
	klog.Infof("Starting syslog-health-monitor...")

	configFile := flag.String("config-file", "/etc/config/config.yaml",
		"Path to the YAML configuration file for log checks.")
	platformConnectorSocket := flag.String("platform-connector-socket", "unix:///var/run/nvsentinel.sock",
		"Path to the platform-connector UDS socket.")
	platformConnectorEndpoint := flag.String("platform-connector-endpoint", "",
		"Platform connector endpoint (supports unix:// for socket or tcp:// for network). If specified, "+
			"overrides platform-connector-socket.")
	nodeNameEnv := flag.String("node-name", os.Getenv("NODE_NAME"), "Node name. Defaults to NODE_NAME env var.")
	pollingIntervalFlag := flag.String("polling-interval", defaultPollingInterval,
		"Polling interval for health checks (e.g., 15m, 1h).")
	stateFileFlag := flag.String("state-file", defaultStateFilePath,
		"Path to state file for cursor persistence.")
	sxidMappingFlag := flag.String("sxid-mapping-file", defaultSXidMappingPath, "Path to SXID errors mapping CSV file.")
	actionMappingFlag := flag.String("action-mapping-file", defaultActionMappingPath,
		"Path to action mapping INI file.")
	metricsPort := flag.String("metrics-port", "2112", "Port to expose Prometheus metrics on")
	xidAnalyserEndpoint := flag.String("xid-analyser-endpoint", "http://localhost:8080",
		"Endpoint to the XID analyser service.")

	flag.Parse()

	klog.Infof("Parsed command line flags successfully")

	nodeName := *nodeNameEnv
	if nodeName == "" {
		klog.Fatalf("NODE_NAME env not set and --node-name flag not provided, cannot run.")
	}

	klog.Infof("Using node name: %s", nodeName)

	// Determine which endpoint to use - new endpoint flag takes precedence
	var endpoint string
	if *platformConnectorEndpoint != "" {
		endpoint = *platformConnectorEndpoint
	} else {
		endpoint = *platformConnectorSocket
	}

	var opts []grpc.DialOption

	// Determine if TLS should be used based on endpoint and available certificates
	creds, useTLS := createTLSCredentials(endpoint)

	if useTLS {
		klog.Infof("Configuring TLS credentials for platform connector connection")

		endpoint = strings.TrimPrefix(endpoint, "tcp://")

		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		klog.Infof("Using insecure credentials for platform connector connection")

		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	klog.Infof("Creating gRPC client to platform connector at: %s", endpoint)

	// Add retry logic for platform connector endpoint with detailed diagnostics
	var conn *grpc.ClientConn

	var err error

	maxRetries := 10
	isUnixSocket := strings.HasPrefix(endpoint, "unix://")

	for attempt := 1; attempt <= maxRetries; attempt++ {
		klog.Infof("Attempt %d/%d: Checking platform connector availability at %s", attempt, maxRetries, endpoint)

		// For unix sockets, check if socket file exists before attempting connection
		if isUnixSocket {
			socketPath := strings.TrimPrefix(endpoint, "unix://")
			if _, statErr := os.Stat(socketPath); statErr != nil {
				klog.Warningf("Attempt %d/%d: Platform connector socket file does not exist: %v", attempt, maxRetries, statErr)

				if attempt < maxRetries {
					time.Sleep(time.Duration(attempt) * time.Second)
					continue
				}

				klog.Fatalf("Platform connector socket file not found after %d attempts: %s", maxRetries, socketPath)
			}
		}

		conn, err = grpc.NewClient(endpoint, opts...)
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

	filePaths := fd.FilePaths{
		StateFilePath:     *stateFileFlag,
		SxidMappingPath:   *sxidMappingFlag,
		ActionMappingPath: *actionMappingFlag,
	}

	fdHealthMonitor, err := fd.NewSyslogMonitor(nodeName, config.Checks, client, defaultAgentName,
		defaultComponentClass, *pollingIntervalFlag, filePaths, *xidAnalyserEndpoint)
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
