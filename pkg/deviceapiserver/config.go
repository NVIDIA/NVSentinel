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

package deviceapiserver

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds the server configuration.
type Config struct {
	// GRPCAddress is the TCP address for gRPC (e.g., ":50051").
	// Default is "127.0.0.1:50051" (localhost only) for security.
	// Set to ":50051" to bind to all interfaces (use with caution - unauthenticated).
	GRPCAddress string

	// ProviderAddress is the TCP address for provider gRPC connections (e.g., "localhost:9001").
	// This is used by containerized providers (sidecars) to connect to the server.
	// If empty, providers must use the main GRPCAddress or UnixSocket.
	// Default is "localhost:9001" when a separate provider endpoint is needed.
	ProviderAddress string

	// UnixSocket is the path to the Unix socket for node-local communication.
	// If empty, Unix socket is disabled.
	UnixSocket string

	// UnixSocketPermissions is the file mode for the Unix socket.
	UnixSocketPermissions os.FileMode

	// HealthPort is the port for HTTP health endpoints.
	HealthPort int

	// MetricsPort is the port for Prometheus metrics.
	MetricsPort int

	// ShutdownTimeout is the maximum time to wait for graceful shutdown.
	ShutdownTimeout time.Duration

	// ShutdownDelay is the time to wait before starting shutdown (for k8s readiness).
	ShutdownDelay time.Duration

	// LogFormat is the log output format ("text" or "json").
	LogFormat string

	// NodeName is the Kubernetes node name (from downward API).
	NodeName string

	// NVML Provider configuration
	// NVMLEnabled enables the built-in NVML provider for device enumeration and health monitoring.
	NVMLEnabled bool

	// NVMLDriverRoot is the root path where NVIDIA driver libraries are located.
	// Common values: "/run/nvidia/driver" (container with RuntimeClass), "/" (bare metal)
	NVMLDriverRoot string

	// NVMLIgnoredXids is a comma-separated list of additional XID error codes to ignore.
	NVMLIgnoredXids string

	// NVMLHealthCheckEnabled enables XID event monitoring for health checks.
	NVMLHealthCheckEnabled bool
}

// DefaultConfig returns a Config with default values.
func DefaultConfig() Config {
	return Config{
		// Bind to localhost only by default for security (unauthenticated API).
		// Use ":50051" to bind to all interfaces if network access is required.
		GRPCAddress: "127.0.0.1:50051",
		// Provider address is empty by default (disabled).
		// Set to "localhost:9001" when using containerized provider sidecars.
		ProviderAddress:        "",
		UnixSocket:             "/var/run/device-api/device.sock",
		UnixSocketPermissions: 0660,
		HealthPort:            8081,
		MetricsPort:           9090,
		ShutdownTimeout:       30 * time.Second,
		ShutdownDelay:         5 * time.Second,
		LogFormat:             "text",
		NodeName:              os.Getenv("NODE_NAME"),

		// NVML defaults - disabled by default for safety
		NVMLEnabled:            false,
		NVMLDriverRoot:         "/run/nvidia/driver",
		NVMLIgnoredXids:        "",
		NVMLHealthCheckEnabled: true,
	}
}

// BindFlags binds configuration flags to the given flag set.
func (c *Config) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.GRPCAddress, "grpc-address", c.GRPCAddress,
		"TCP address for gRPC server (e.g., :50051 for all interfaces). "+
			"WARNING: The gRPC API is unauthenticated. Default binds to localhost only.")
	fs.StringVar(&c.ProviderAddress, "provider-address", c.ProviderAddress,
		"TCP address for provider gRPC connections (e.g., localhost:9001). "+
			"Used by containerized provider sidecars. Empty to disable.")
	fs.StringVar(&c.UnixSocket, "unix-socket", c.UnixSocket,
		"Path to Unix socket for node-local IPC (empty to disable)")
	fs.IntVar(&c.HealthPort, "health-port", c.HealthPort,
		"Port for HTTP health endpoints (/healthz, /readyz)")
	fs.IntVar(&c.MetricsPort, "metrics-port", c.MetricsPort,
		"Port for Prometheus metrics (/metrics)")

	// Duration flags need special handling
	shutdownTimeout := int(c.ShutdownTimeout.Seconds())
	fs.IntVar(&shutdownTimeout, "shutdown-timeout", shutdownTimeout,
		"Maximum time in seconds to wait for graceful shutdown")

	shutdownDelay := int(c.ShutdownDelay.Seconds())
	fs.IntVar(&shutdownDelay, "shutdown-delay", shutdownDelay,
		"Time in seconds to wait before starting shutdown (for k8s readiness propagation)")

	fs.StringVar(&c.LogFormat, "log-format", c.LogFormat,
		"Log output format: text or json")
	fs.StringVar(&c.NodeName, "node-name", c.NodeName,
		"Kubernetes node name (defaults to NODE_NAME env var)")

	// Parse hook to convert int to duration
	fs.Func("", "", func(string) error {
		c.ShutdownTimeout = time.Duration(shutdownTimeout) * time.Second
		c.ShutdownDelay = time.Duration(shutdownDelay) * time.Second

		return nil
	})
}

// ApplyEnvironment overrides config from environment variables.
func (c *Config) ApplyEnvironment() {
	c.applyServerEnv()
	c.applyNVMLEnv()
}

// applyServerEnv applies server-related environment variables.
func (c *Config) applyServerEnv() {
	if v := os.Getenv("DEVICE_API_GRPC_ADDRESS"); v != "" {
		c.GRPCAddress = v
	}

	if v := os.Getenv("DEVICE_API_PROVIDER_ADDRESS"); v != "" {
		c.ProviderAddress = v
	}

	if v := os.Getenv("DEVICE_API_UNIX_SOCKET"); v != "" {
		c.UnixSocket = v
	}

	if v := os.Getenv("DEVICE_API_HEALTH_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			c.HealthPort = port
		}
	}

	if v := os.Getenv("DEVICE_API_METRICS_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			c.MetricsPort = port
		}
	}

	if v := os.Getenv("DEVICE_API_LOG_FORMAT"); v != "" {
		c.LogFormat = v
	}

	if v := os.Getenv("NODE_NAME"); v != "" && c.NodeName == "" {
		c.NodeName = v
	}
}

// applyNVMLEnv applies NVML-related environment variables.
func (c *Config) applyNVMLEnv() {
	if v := os.Getenv("DEVICE_API_NVML_ENABLED"); v == "true" || v == "1" {
		c.NVMLEnabled = true
	}

	if v := os.Getenv("DEVICE_API_NVML_DRIVER_ROOT"); v != "" {
		c.NVMLDriverRoot = v
	}

	if v := os.Getenv("DEVICE_API_NVML_IGNORED_XIDS"); v != "" {
		c.NVMLIgnoredXids = v
	}

	if v := os.Getenv("DEVICE_API_NVML_HEALTH_CHECK"); v == "false" || v == "0" {
		c.NVMLHealthCheckEnabled = false
	}
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	var errs []error

	if c.GRPCAddress == "" && c.UnixSocket == "" {
		errs = append(errs, errors.New("at least one of grpc-address or unix-socket must be specified"))
	}

	if err := validatePort("health-port", c.HealthPort); err != nil {
		errs = append(errs, err)
	}

	if err := validatePort("metrics-port", c.MetricsPort); err != nil {
		errs = append(errs, err)
	}

	if c.ShutdownTimeout < 0 {
		errs = append(errs, errors.New("shutdown-timeout must be non-negative"))
	}

	if c.LogFormat != "text" && c.LogFormat != "json" {
		errs = append(errs, fmt.Errorf("log-format must be 'text' or 'json', got %q", c.LogFormat))
	}

	return errors.Join(errs...)
}

// validatePort checks if a port number is valid.
func validatePort(name string, port int) error {
	if port < 0 || port > 65535 {
		return fmt.Errorf("%s must be 0-65535, got %d", name, port)
	}

	return nil
}
