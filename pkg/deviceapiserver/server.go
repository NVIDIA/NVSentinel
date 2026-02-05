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

// Package deviceapiserver implements the Device API Server.
//
// The Device API Server is a node-local gRPC cache server that acts as an
// intermediary between providers (health monitors) and consumers (device
// plugins, DRA drivers).
package deviceapiserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
	"k8s.io/klog/v2"

	v1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"
	"github.com/nvidia/nvsentinel/pkg/deviceapiserver/cache"
	"github.com/nvidia/nvsentinel/pkg/deviceapiserver/metrics"
	"github.com/nvidia/nvsentinel/pkg/deviceapiserver/nvml"
	"github.com/nvidia/nvsentinel/pkg/deviceapiserver/service"
	"github.com/nvidia/nvsentinel/pkg/version"
)

// HTTP server timeout configuration.
// These values follow security best practices to prevent slowloris attacks
// and resource exhaustion from slow clients.
const (
	// httpReadHeaderTimeout is the time allowed to read request headers.
	httpReadHeaderTimeout = 5 * time.Second

	// httpReadTimeout is the maximum duration for reading the entire request.
	httpReadTimeout = 10 * time.Second

	// httpWriteTimeout is the maximum duration before timing out writes of the response.
	httpWriteTimeout = 10 * time.Second

	// httpIdleTimeout is the maximum time to wait for the next request when keep-alives are enabled.
	httpIdleTimeout = 120 * time.Second
)

// Server is the Device API Server.
type Server struct {
	config Config
	logger klog.Logger

	// Components
	cache        *cache.GpuCache
	metrics      *metrics.Metrics
	grpcServer   *grpc.Server
	healthServer *health.Server
	gpuService   *service.GpuService
	nvmlProvider *nvml.Provider

	// Listeners
	tcpListener      net.Listener
	providerListener net.Listener
	unixListener     net.Listener

	// HTTP servers
	healthHTTPServer  *http.Server
	metricsHTTPServer *http.Server

	// Lifecycle
	mu           sync.Mutex
	started      bool
	stopped      bool
	ready        atomic.Bool
	shuttingDown atomic.Bool
}

// New creates a new Device API Server.
func New(config Config, logger klog.Logger) *Server {
	logger = logger.WithName("server")

	// Create metrics first (needed by cache)
	m := metrics.New()

	// Set server info metric
	versionInfo := version.Get()
	m.SetServerInfo(versionInfo.Version, runtime.Version(), config.NodeName)

	// Create cache with metrics integration
	gpuCache := cache.New(logger, m)

	// Wire up dropped event metrics
	gpuCache.Broadcaster().SetOnEventDrop(m.RecordWatchEventDropped)

	// Create gRPC health server
	healthServer := health.NewServer()

	// Create gRPC server with reflection for debugging
	grpcServer := grpc.NewServer()
	reflection.Register(grpcServer)

	// Create unified GpuService
	gpuService := service.NewGpuService(gpuCache)

	// Register services
	v1alpha1.RegisterGpuServiceServer(grpcServer, gpuService)
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)

	// Note: NVML provider is created in Start() after gRPC server is listening,
	// because it needs a loopback gRPC client connection.

	return &Server{
		config:       config,
		logger:       logger,
		cache:        gpuCache,
		metrics:      m,
		grpcServer:   grpcServer,
		healthServer: healthServer,
		gpuService:   gpuService,
	}
}

// Start starts the server and blocks until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()

	if s.started {
		s.mu.Unlock()

		return errors.New("server already started")
	}

	s.started = true
	s.mu.Unlock()

	s.logger.Info("Starting server")

	// Start listeners
	if err := s.startListeners(); err != nil {
		return fmt.Errorf("failed to start listeners: %w", err)
	}

	// Start HTTP servers
	if err := s.startHTTPServers(); err != nil {
		s.stopListeners()
		return fmt.Errorf("failed to start HTTP servers: %w", err)
	}

	// Start gRPC server
	errCh := make(chan error, 2)
	s.startGRPCServers(errCh)

	// Create and start NVML provider (if enabled)
	// This is done after gRPC server is listening so we can create a loopback client.
	if s.config.NVMLEnabled {
		if err := s.createAndStartNVMLProvider(ctx); err != nil {
			// NVML failure is not fatal - log and continue
			s.logger.Error(err, "Failed to start NVML provider, continuing without NVML support")
		} else {
			s.logger.Info("NVML provider started",
				"gpuCount", s.nvmlProvider.GPUCount(),
				"healthMonitorRunning", s.nvmlProvider.IsHealthMonitorRunning(),
			)
		}
	}

	// Mark as ready (for gRPC health and HTTP readiness)
	s.setReady(true)
	s.logger.Info("Server is ready",
		"grpcAddress", s.config.GRPCAddress,
		"providerAddress", s.config.ProviderAddress,
		"unixSocket", s.config.UnixSocket,
		"healthPort", s.config.HealthPort,
		"metricsPort", s.config.MetricsPort,
	)

	// Start background metrics updater
	metricsCtx, metricsCancel := context.WithCancel(ctx)
	go s.runMetricsUpdater(metricsCtx)

	// Wait for shutdown signal or error
	select {
	case <-ctx.Done():
		s.logger.Info("Received shutdown signal")
	case err := <-errCh:
		s.logger.Error(err, "Server error")
		metricsCancel()

		return err
	}

	metricsCancel()

	// Graceful shutdown
	return s.shutdown()
}

// setReady updates the server readiness state.
func (s *Server) setReady(ready bool) {
	s.ready.Store(ready)

	var status grpc_health_v1.HealthCheckResponse_ServingStatus
	if ready {
		status = grpc_health_v1.HealthCheckResponse_SERVING
	} else {
		status = grpc_health_v1.HealthCheckResponse_NOT_SERVING
	}

	s.healthServer.SetServingStatus("", status)
	s.healthServer.SetServingStatus("nvidia.device.v1alpha1.GpuService", status)
}

// startListeners starts the TCP and Unix socket listeners.
func (s *Server) startListeners() error {
	lc := &net.ListenConfig{}

	if err := s.startTCPListener(lc); err != nil {
		return err
	}

	if err := s.startProviderListener(lc); err != nil {
		return err
	}

	if err := s.startUnixListener(lc); err != nil {
		return err
	}

	return nil
}

// startTCPListener starts the TCP listener if configured.
func (s *Server) startTCPListener(lc *net.ListenConfig) error {
	if s.config.GRPCAddress == "" {
		return nil
	}

	var err error

	s.tcpListener, err = lc.Listen(context.Background(), "tcp", s.config.GRPCAddress)
	if err != nil {
		return fmt.Errorf("failed to listen on TCP %s: %w", s.config.GRPCAddress, err)
	}

	s.logger.V(1).Info("TCP listener started", "address", s.tcpListener.Addr())

	return nil
}

// startProviderListener starts the provider TCP listener if configured.
// This provides a separate endpoint for provider (sidecar) connections.
func (s *Server) startProviderListener(lc *net.ListenConfig) error {
	if s.config.ProviderAddress == "" {
		return nil
	}

	var err error

	s.providerListener, err = lc.Listen(context.Background(), "tcp", s.config.ProviderAddress)
	if err != nil {
		s.stopListeners()

		return fmt.Errorf("failed to listen on provider TCP %s: %w", s.config.ProviderAddress, err)
	}

	s.logger.V(1).Info("Provider TCP listener started", "address", s.providerListener.Addr())

	return nil
}

// startUnixListener starts the Unix socket listener if configured.
func (s *Server) startUnixListener(lc *net.ListenConfig) error {
	if s.config.UnixSocket == "" {
		return nil
	}

	// Create socket directory if needed
	socketDir := filepath.Dir(s.config.UnixSocket)
	if err := os.MkdirAll(socketDir, 0755); err != nil {
		s.stopListeners()

		return fmt.Errorf("failed to create socket directory %s: %w", socketDir, err)
	}

	// Remove stale socket file
	if err := os.Remove(s.config.UnixSocket); err != nil && !os.IsNotExist(err) {
		s.stopListeners()

		return fmt.Errorf("failed to remove stale socket %s: %w", s.config.UnixSocket, err)
	}

	var err error

	s.unixListener, err = lc.Listen(context.Background(), "unix", s.config.UnixSocket)
	if err != nil {
		s.stopListeners()

		return fmt.Errorf("failed to listen on Unix socket %s: %w", s.config.UnixSocket, err)
	}

	// Set socket permissions
	if err := os.Chmod(s.config.UnixSocket, s.config.UnixSocketPermissions); err != nil {
		s.stopListeners()

		return fmt.Errorf("failed to set socket permissions: %w", err)
	}

	s.logger.V(1).Info("Unix socket listener started",
		"path", s.config.UnixSocket,
		"permissions", fmt.Sprintf("%04o", s.config.UnixSocketPermissions),
	)

	return nil
}

// startHTTPServers starts the health and metrics HTTP servers.
func (s *Server) startHTTPServers() error {
	// Health server
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/healthz", s.handleHealthz)
	healthMux.HandleFunc("/readyz", s.handleReadyz)
	healthMux.HandleFunc("/livez", s.handleHealthz) // Alias for liveness

	s.healthHTTPServer = &http.Server{
		Addr:              fmt.Sprintf(":%d", s.config.HealthPort),
		Handler:           healthMux,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
	}

	go func() {
		s.logger.V(1).Info("Health HTTP server started", "port", s.config.HealthPort)

		if err := s.healthHTTPServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error(err, "Health HTTP server error")
		}
	}()

	// Metrics server using Prometheus handler
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.InstrumentMetricHandler(
		s.metrics.Registry(),
		promhttp.HandlerFor(
			s.metrics.Registry(),
			promhttp.HandlerOpts{
				EnableOpenMetrics: true,
			},
		),
	))

	s.metricsHTTPServer = &http.Server{
		Addr:              fmt.Sprintf(":%d", s.config.MetricsPort),
		Handler:           metricsMux,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
	}

	go func() {
		s.logger.V(1).Info("Metrics HTTP server started", "port", s.config.MetricsPort)

		if err := s.metricsHTTPServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error(err, "Metrics HTTP server error")
		}
	}()

	return nil
}

// startGRPCServers starts serving gRPC on all listeners.
func (s *Server) startGRPCServers(errCh chan<- error) {
	// Serve on TCP
	if s.tcpListener != nil {
		go func() {
			s.logger.V(1).Info("gRPC server started on TCP", "address", s.tcpListener.Addr())

			if err := s.grpcServer.Serve(s.tcpListener); err != nil {
				errCh <- fmt.Errorf("gRPC TCP server error: %w", err)
			}
		}()
	}

	// Serve on Provider TCP (for sidecar provider connections)
	if s.providerListener != nil {
		go func() {
			s.logger.V(1).Info("gRPC server started on provider TCP", "address", s.providerListener.Addr())

			if err := s.grpcServer.Serve(s.providerListener); err != nil {
				errCh <- fmt.Errorf("gRPC provider TCP server error: %w", err)
			}
		}()
	}

	// Serve on Unix socket
	if s.unixListener != nil {
		go func() {
			s.logger.V(1).Info("gRPC server started on Unix socket", "path", s.config.UnixSocket)

			if err := s.grpcServer.Serve(s.unixListener); err != nil {
				errCh <- fmt.Errorf("gRPC Unix socket server error: %w", err)
			}
		}()
	}
}

// shutdown performs graceful shutdown following Kubernetes best practices.
//
// Shutdown sequence:
// 1. Mark as not ready (gRPC health + HTTP readyz)
// 2. Wait for shutdown delay (allow k8s to propagate endpoint removal)
// 3. Stop accepting new connections
// 4. Wait for existing requests to complete (up to timeout)
// 5. Force close remaining connections
// 6. Clean up resources
func (s *Server) shutdown() error {
	if !s.markStopped() {
		return nil
	}

	s.logger.Info("Starting graceful shutdown",
		"shutdownDelay", s.config.ShutdownDelay,
		"shutdownTimeout", s.config.ShutdownTimeout,
	)

	// Step 1: Mark as not ready
	s.setReady(false)
	s.logger.V(1).Info("Marked as not ready")

	// Step 2: Wait for shutdown delay
	s.waitShutdownDelay()

	// Step 3-5: Shutdown servers with timeout
	errs := s.shutdownServers()

	// Step 6: Clean up resources
	s.cleanupResources()

	if len(errs) > 0 {
		s.logger.Error(errs[0], "Shutdown completed with errors", "errorCount", len(errs))

		return errs[0]
	}

	s.logger.Info("Server shutdown complete")

	return nil
}

// markStopped marks the server as stopped, returns false if already stopped.
func (s *Server) markStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stopped {
		return false
	}

	s.stopped = true
	s.shuttingDown.Store(true)

	return true
}

// waitShutdownDelay waits for the configured shutdown delay.
func (s *Server) waitShutdownDelay() {
	if s.config.ShutdownDelay > 0 {
		s.logger.V(1).Info("Waiting for shutdown delay", "delay", s.config.ShutdownDelay)
		time.Sleep(s.config.ShutdownDelay)
	}
}

// shutdownServers shuts down HTTP and gRPC servers with timeout.
func (s *Server) shutdownServers() []error {
	ctx, cancel := context.WithTimeout(context.Background(), s.config.ShutdownTimeout)
	defer cancel()

	var wg sync.WaitGroup

	errCh := make(chan error, 4)

	// Shutdown HTTP servers
	wg.Add(1)

	go func() {
		defer wg.Done()

		if err := s.healthHTTPServer.Shutdown(ctx); err != nil {
			errCh <- fmt.Errorf("health HTTP shutdown error: %w", err)
		}
	}()

	wg.Add(1)

	go func() {
		defer wg.Done()

		if err := s.metricsHTTPServer.Shutdown(ctx); err != nil {
			errCh <- fmt.Errorf("metrics HTTP shutdown error: %w", err)
		}
	}()

	// Graceful stop gRPC server
	wg.Add(1)

	go func() {
		defer wg.Done()

		s.shutdownGRPCServer(ctx)
	}()

	// Wait for all shutdowns
	wg.Wait()
	close(errCh)

	// Collect any errors
	var errs []error

	for err := range errCh {
		errs = append(errs, err)
	}

	return errs
}

// shutdownGRPCServer gracefully stops the gRPC server.
func (s *Server) shutdownGRPCServer(ctx context.Context) {
	done := make(chan struct{})

	go func() {
		s.grpcServer.GracefulStop()
		close(done)
	}()

	select {
	case <-done:
		s.logger.V(1).Info("gRPC server stopped gracefully")
	case <-ctx.Done():
		s.logger.Info("gRPC graceful stop timeout, forcing stop")
		s.grpcServer.Stop()
	}
}

// createAndStartNVMLProvider creates the NVML provider with a loopback gRPC client
// and starts it.
//
// This is called after the gRPC server is listening so we can create a client
// connection to ourselves. The NVML provider uses this client to register GPUs
// and update health status via the standard GpuService API ("dogfooding").
func (s *Server) createAndStartNVMLProvider(ctx context.Context) error {
	// Determine the best endpoint for loopback connection
	// Prefer Unix socket for lower latency, fall back to TCP
	var target string
	if s.config.UnixSocket != "" {
		target = "unix://" + s.config.UnixSocket
	} else {
		target = s.config.GRPCAddress
	}

	s.logger.V(1).Info("Creating NVML provider with loopback gRPC client", "target", target)

	// Create loopback gRPC client connection
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("failed to create loopback gRPC connection: %w", err)
	}

	// Create GpuService client
	client := v1alpha1.NewGpuServiceClient(conn)

	// Create NVML provider configuration
	nvmlConfig := nvml.Config{
		DriverRoot:            s.config.NVMLDriverRoot,
		AdditionalIgnoredXids: nvml.ParseIgnoredXids(s.config.NVMLIgnoredXids),
		HealthCheckEnabled:    s.config.NVMLHealthCheckEnabled,
	}

	// Create and start NVML provider
	s.nvmlProvider = nvml.New(nvmlConfig, client, s.logger)
	if err := s.nvmlProvider.Start(ctx); err != nil {
		conn.Close()
		s.nvmlProvider = nil
		return fmt.Errorf("failed to start NVML provider: %w", err)
	}

	return nil
}

// cleanupResources cleans up server resources.
func (s *Server) cleanupResources() {
	// Stop NVML provider
	if s.nvmlProvider != nil {
		s.nvmlProvider.Stop()
		s.logger.V(1).Info("NVML provider stopped")
	}

	s.cache.Broadcaster().Close()

	// Clean up Unix socket
	if s.config.UnixSocket != "" {
		if err := os.Remove(s.config.UnixSocket); err != nil && !os.IsNotExist(err) {
			s.logger.V(1).Info("Failed to remove Unix socket",
				"path", s.config.UnixSocket,
				"error", err,
			)
		}
	}
}

// stopListeners closes all listeners.
func (s *Server) stopListeners() {
	if s.tcpListener != nil {
		s.tcpListener.Close()
	}

	if s.providerListener != nil {
		s.providerListener.Close()
	}

	if s.unixListener != nil {
		s.unixListener.Close()
	}
}

// handleHealthz handles liveness probe requests.
// Returns 200 OK if the process is alive (always true if we can respond).
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// handleReadyz handles readiness probe requests.
// Returns 200 OK if ready to serve traffic, 503 during shutdown.
func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	// Check if shutting down
	if s.shuttingDown.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("shutting down\n"))

		return
	}

	// Check gRPC health
	if !s.ready.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready\n"))

		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// runMetricsUpdater periodically updates the metrics gauges.
func (s *Server) runMetricsUpdater(ctx context.Context) {
	// Update interval for gauge metrics
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Initial update
	s.updateMetrics()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.updateMetrics()
		}
	}
}

// updateMetrics updates all gauge metrics from current state.
func (s *Server) updateMetrics() {
	// Update cache stats
	stats := s.cache.GetStats()
	s.metrics.UpdateCacheStats(
		stats.TotalGpus,
		stats.HealthyGpus,
		stats.UnhealthyGpus,
		stats.UnknownGpus,
		stats.ResourceVersion,
	)

	// Update watch streams
	s.metrics.UpdateWatchStreams(s.cache.Broadcaster().SubscriberCount())

	// Update NVML status
	if s.nvmlProvider != nil && s.nvmlProvider.IsInitialized() {
		s.metrics.UpdateNVMLStatus(
			true,
			s.nvmlProvider.GPUCount(),
			s.nvmlProvider.IsHealthMonitorRunning(),
		)
	} else {
		s.metrics.UpdateNVMLStatus(false, 0, false)
	}
}

// Cache returns the GPU cache (for testing).
func (s *Server) Cache() *cache.GpuCache {
	return s.cache
}

// IsReady returns whether the server is ready to serve traffic.
func (s *Server) IsReady() bool {
	return s.ready.Load()
}
