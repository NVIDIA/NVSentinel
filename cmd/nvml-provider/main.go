// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

//go:build nvml

// Command nvml-provider is a standalone NVML-based GPU health provider that
// connects to a device-api-server instance via gRPC.
//
// This is designed to run as a sidecar container alongside device-api-server,
// providing GPU enumeration and health monitoring via NVML.
//
// Usage:
//
//	nvml-provider --server-address=localhost:9001 --driver-root=/run/nvidia/driver
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/klog/v2"

	v1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"
)

const (
	// DefaultProviderID is the default identifier for this provider.
	DefaultProviderID = "nvml-provider-sidecar"

	// ConditionTypeNVMLReady is the condition type for NVML health status.
	ConditionTypeNVMLReady = "NVMLReady"

	// Condition status values.
	ConditionStatusTrue    = "True"
	ConditionStatusFalse   = "False"
	ConditionStatusUnknown = "Unknown"

	// HeartbeatInterval is how often to send heartbeats.
	HeartbeatInterval = 10 * time.Second

	// HealthCheckPort is the HTTP port for health checks.
	HealthCheckPort = 8082

	// EventTimeout is the timeout for NVML event wait (in milliseconds).
	EventTimeout = 5000

	// DefaultServerAddress is the default device-api-server address.
	DefaultServerAddress = "localhost:9001"

	// ConnectionRetryInterval is how long to wait between connection attempts.
	ConnectionRetryInterval = 5 * time.Second

	// MaxConnectionRetries is the maximum number of connection attempts.
	MaxConnectionRetries = 60
)

// Config holds the provider configuration.
type Config struct {
	ServerAddress      string
	ProviderID         string
	DriverRoot         string
	HealthCheckEnabled bool
	HealthCheckPort    int
	IgnoredXids        []uint64
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		ServerAddress:      DefaultServerAddress,
		ProviderID:         DefaultProviderID,
		DriverRoot:         "/run/nvidia/driver",
		HealthCheckEnabled: true,
		HealthCheckPort:    HealthCheckPort,
	}
}

// Provider is the standalone NVML provider that connects to device-api-server.
type Provider struct {
	config Config
	logger klog.Logger

	// gRPC clients
	conn         *grpc.ClientConn
	gpuClient    v1alpha1.GpuServiceClient
	healthClient grpc_health_v1.HealthClient

	// NVML
	nvmllib  nvml.Interface
	eventSet nvml.EventSet

	// State
	mu             sync.RWMutex
	gpuUUIDs       []string
	initialized    bool
	connected      bool
	healthy        bool
	monitorRunning bool

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewProvider creates a new standalone NVML provider.
func NewProvider(cfg Config, logger klog.Logger) *Provider {
	return &Provider{
		config: cfg,
		logger: logger.WithName("nvml-provider"),
	}
}

func main() {
	// Initialize logging flags first
	klog.InitFlags(nil)

	cfg := parseFlags()
	// flag.Parse() is called inside parseFlags()

	logger := klog.Background()
	logger.Info("Starting NVML provider sidecar",
		"serverAddress", cfg.ServerAddress,
		"providerID", cfg.ProviderID,
		"driverRoot", cfg.DriverRoot,
		"healthCheckEnabled", cfg.HealthCheckEnabled,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("Received signal, shutting down", "signal", sig)
		cancel()
	}()

	// Create and run provider
	provider := NewProvider(cfg, logger)
	if err := provider.Run(ctx); err != nil {
		logger.Error(err, "Provider failed")
		os.Exit(1)
	}

	logger.Info("NVML provider shutdown complete")
}

func parseFlags() Config {
	cfg := DefaultConfig()

	flag.StringVar(&cfg.ServerAddress, "server-address", cfg.ServerAddress,
		"Address of device-api-server gRPC endpoint")
	flag.StringVar(&cfg.ProviderID, "provider-id", cfg.ProviderID,
		"Unique identifier for this provider")
	flag.StringVar(&cfg.DriverRoot, "driver-root", cfg.DriverRoot,
		"Root path for NVIDIA driver libraries")
	flag.BoolVar(&cfg.HealthCheckEnabled, "health-check", cfg.HealthCheckEnabled,
		"Enable XID event monitoring for health checks")
	flag.IntVar(&cfg.HealthCheckPort, "health-port", cfg.HealthCheckPort,
		"HTTP port for health check endpoints")

	// Parse flags
	flag.Parse()

	// Check environment variables as fallback (override flags if set)
	if addr := os.Getenv("PROVIDER_SERVER_ADDRESS"); addr != "" {
		cfg.ServerAddress = addr
	}
	if id := os.Getenv("PROVIDER_ID"); id != "" {
		cfg.ProviderID = id
	}
	if root := os.Getenv("NVML_DRIVER_ROOT"); root != "" {
		cfg.DriverRoot = root
	}

	return cfg
}

// Run starts the provider and blocks until the context is cancelled.
func (p *Provider) Run(ctx context.Context) error {
	p.ctx, p.cancel = context.WithCancel(ctx)
	defer p.cancel()

	// Start health check server
	p.wg.Add(1)
	go p.runHealthServer()

	// Initialize NVML
	if err := p.initNVML(); err != nil {
		return fmt.Errorf("failed to initialize NVML: %w", err)
	}
	defer p.shutdownNVML()

	// Connect to server with retry
	if err := p.connectWithRetry(); err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}
	defer p.disconnect()

	// Enumerate and register GPUs (or reconcile if reconnecting)
	if err := p.enumerateAndRegisterGPUs(); err != nil {
		return fmt.Errorf("failed to enumerate GPUs: %w", err)
	}

	// Reconcile state (handles restart/reconnection scenarios)
	if err := p.ReconcileState(p.ctx); err != nil {
		// Reconciliation failure is not fatal - log and continue
		p.logger.Error(err, "State reconciliation failed, continuing")
	}

	// Start heartbeat loop
	p.wg.Add(1)
	go p.runHeartbeatLoop()

	// Start health monitoring if enabled
	if p.config.HealthCheckEnabled && len(p.gpuUUIDs) > 0 {
		p.wg.Add(1)
		go p.runHealthMonitor()
	}

	// Mark as healthy
	p.setHealthy(true)

	// Wait for shutdown
	<-p.ctx.Done()

	// Graceful shutdown
	p.setHealthy(false)
	p.wg.Wait()

	return nil
}

// initNVML initializes the NVML library.
func (p *Provider) initNVML() error {
	// Find NVML library
	libraryPath := p.findDriverLibrary()
	if libraryPath != "" {
		p.logger.V(2).Info("Using NVML library", "path", libraryPath)
		p.nvmllib = nvml.New(nvml.WithLibraryPath(libraryPath))
	} else {
		p.logger.V(2).Info("Using system default NVML library")
		p.nvmllib = nvml.New()
	}

	// Initialize
	ret := p.nvmllib.Init()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("NVML init failed: %v", nvml.ErrorString(ret))
	}

	// Log driver version
	if version, ret := p.nvmllib.SystemGetDriverVersion(); ret == nvml.SUCCESS {
		p.logger.Info("NVML initialized", "driverVersion", version)
	}

	p.initialized = true
	return nil
}

// shutdownNVML shuts down the NVML library.
func (p *Provider) shutdownNVML() {
	if !p.initialized {
		return
	}

	if p.eventSet != nil {
		p.eventSet.Free()
		p.eventSet = nil
	}

	p.nvmllib.Shutdown()
	p.initialized = false
	p.logger.V(1).Info("NVML shutdown complete")
}

// findDriverLibrary locates the NVML library in the driver root.
func (p *Provider) findDriverLibrary() string {
	if p.config.DriverRoot == "" {
		return ""
	}

	paths := []string{
		filepath.Join(p.config.DriverRoot, "usr/lib64/libnvidia-ml.so.1"),
		filepath.Join(p.config.DriverRoot, "usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1"),
		filepath.Join(p.config.DriverRoot, "usr/lib/libnvidia-ml.so.1"),
		filepath.Join(p.config.DriverRoot, "lib64/libnvidia-ml.so.1"),
		filepath.Join(p.config.DriverRoot, "lib/libnvidia-ml.so.1"),
	}

	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	return ""
}

// connectWithRetry connects to the device-api-server with retry logic.
func (p *Provider) connectWithRetry() error {
	var lastErr error

	for i := 0; i < MaxConnectionRetries; i++ {
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		default:
		}

		conn, err := grpc.NewClient(
			p.config.ServerAddress,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			lastErr = err
			p.logger.V(1).Info("Connection attempt failed, retrying",
				"attempt", i+1,
				"error", err,
			)
			time.Sleep(ConnectionRetryInterval)
			continue
		}

		p.conn = conn
		p.gpuClient = v1alpha1.NewGpuServiceClient(conn)
		p.healthClient = grpc_health_v1.NewHealthClient(conn)

		// Wait for server to be ready
		if err := p.waitForServerReady(); err != nil {
			conn.Close()
			lastErr = err
			p.logger.V(1).Info("Server not ready, retrying",
				"attempt", i+1,
				"error", err,
			)
			time.Sleep(ConnectionRetryInterval)
			continue
		}

		p.connected = true
		p.logger.Info("Connected to device-api-server", "address", p.config.ServerAddress)
		return nil
	}

	return fmt.Errorf("failed to connect after %d attempts: %w", MaxConnectionRetries, lastErr)
}

// waitForServerReady waits for the server to report healthy.
func (p *Provider) waitForServerReady() error {
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()

	resp, err := p.healthClient.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}

	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		return fmt.Errorf("server not serving: %v", resp.Status)
	}

	return nil
}

// disconnect closes the gRPC connection.
func (p *Provider) disconnect() {
	if p.conn != nil {
		p.conn.Close()
		p.conn = nil
	}
	p.connected = false
}

// enumerateAndRegisterGPUs discovers GPUs via NVML and registers them.
func (p *Provider) enumerateAndRegisterGPUs() error {
	count, ret := p.nvmllib.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to get device count: %v", nvml.ErrorString(ret))
	}

	if count == 0 {
		p.logger.Info("No GPUs found on this node")
		return nil
	}

	p.logger.Info("Enumerating GPUs", "count", count)
	p.gpuUUIDs = make([]string, 0, count)

	for i := 0; i < count; i++ {
		device, ret := p.nvmllib.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			p.logger.Error(nil, "Failed to get device handle", "index", i, "error", nvml.ErrorString(ret))
			continue
		}

		uuid, ret := device.GetUUID()
		if ret != nvml.SUCCESS {
			p.logger.Error(nil, "Failed to get device UUID", "index", i, "error", nvml.ErrorString(ret))
			continue
		}

		// Get device info for registration
		productName, _ := device.GetName()
		var memoryBytes uint64
		if memInfo, ret := device.GetMemoryInfo(); ret == nvml.SUCCESS {
			memoryBytes = memInfo.Total
		}

		// Register GPU with server
		if err := p.registerGPU(uuid, productName, memoryBytes); err != nil {
			p.logger.Error(err, "Failed to register GPU", "uuid", uuid)
			continue
		}

		p.gpuUUIDs = append(p.gpuUUIDs, uuid)
		p.logger.Info("Registered GPU",
			"uuid", uuid,
			"productName", productName,
			"memory", formatBytes(memoryBytes),
		)
	}

	p.logger.Info("GPU enumeration complete", "registered", len(p.gpuUUIDs))
	return nil
}

// registerGPU registers a single GPU with the device-api-server using CreateGpu.
func (p *Provider) registerGPU(uuid, productName string, memoryBytes uint64) error {
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()

	req := &v1alpha1.CreateGpuRequest{
		Gpu: &v1alpha1.Gpu{
			Metadata: &v1alpha1.ObjectMeta{Name: uuid},
			Spec:     &v1alpha1.GpuSpec{Uuid: uuid},
			Status: &v1alpha1.GpuStatus{
				Conditions: []*v1alpha1.Condition{
					{
						Type:               ConditionTypeNVMLReady,
						Status:             ConditionStatusTrue,
						Reason:             "Initialized",
						Message:            fmt.Sprintf("GPU enumerated via NVML: %s (%s)", productName, formatBytes(memoryBytes)),
						LastTransitionTime: timestamppb.Now(),
					},
				},
			},
		},
	}

	_, err := p.gpuClient.CreateGpu(ctx, req)
	return err
}

// runHeartbeatLoop sends periodic heartbeats to the server.
func (p *Provider) runHeartbeatLoop() {
	defer p.wg.Done()

	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			if err := p.sendHeartbeat(); err != nil {
				p.logger.Error(err, "Failed to send heartbeat")
			}
		}
	}
}

// sendHeartbeat performs a health check on the server connection.
// Note: The Heartbeat RPC was removed. We now just verify the server is reachable.
func (p *Provider) sendHeartbeat() error {
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()

	// Verify server connectivity by checking gRPC health
	resp, err := p.healthClient.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		return err
	}

	if resp.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		return fmt.Errorf("server not serving: %v", resp.Status)
	}

	p.mu.RLock()
	gpuCount := len(p.gpuUUIDs)
	p.mu.RUnlock()

	p.logger.V(4).Info("Health check passed", "gpuCount", gpuCount)
	return nil
}

// runHealthMonitor monitors NVML events for GPU health changes.
func (p *Provider) runHealthMonitor() {
	defer p.wg.Done()

	p.mu.Lock()
	p.monitorRunning = true
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.monitorRunning = false
		p.mu.Unlock()
	}()

	// Create event set
	eventSet, ret := p.nvmllib.EventSetCreate()
	if ret != nvml.SUCCESS {
		p.logger.Error(nil, "Failed to create event set", "error", nvml.ErrorString(ret))
		return
	}
	p.eventSet = eventSet

	// Register devices for XID events
	deviceCount, ret := p.nvmllib.DeviceGetCount()
	if ret != nvml.SUCCESS {
		p.logger.Error(nil, "Failed to get device count", "error", nvml.ErrorString(ret))
		return
	}

	for i := 0; i < deviceCount; i++ {
		device, ret := p.nvmllib.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			continue
		}
		ret = device.RegisterEvents(nvml.EventTypeXidCriticalError|nvml.EventTypeSingleBitEccError|nvml.EventTypeDoubleBitEccError, eventSet)
		if ret != nvml.SUCCESS {
			p.logger.V(1).Info("Failed to register events for device", "index", i, "error", nvml.ErrorString(ret))
		}
	}

	p.logger.Info("Health monitor started")

	// Event loop
	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}

		data, ret := eventSet.Wait(EventTimeout)
		if ret == nvml.ERROR_TIMEOUT {
			continue
		}
		if ret != nvml.SUCCESS {
			p.logger.V(1).Info("Event wait error", "error", nvml.ErrorString(ret))
			continue
		}

		p.handleXIDEvent(data)
	}
}

// handleXIDEvent processes an XID error event.
func (p *Provider) handleXIDEvent(data nvml.EventData) {
	uuid, ret := data.Device.GetUUID()
	if ret != nvml.SUCCESS {
		p.logger.Error(nil, "Failed to get device UUID from event")
		return
	}

	xid := data.EventData
	p.logger.Info("XID event received",
		"uuid", uuid,
		"xid", xid,
		"eventType", data.EventType,
	)

	// Update GPU status
	status := ConditionStatusTrue
	reason := "Healthy"
	message := "GPU is healthy"

	if isCriticalXID(xid) {
		status = ConditionStatusFalse
		reason = "XIDError"
		message = fmt.Sprintf("Critical XID error: %d", xid)
	}

	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()

	req := &v1alpha1.UpdateGpuStatusRequest{
		Name: uuid,
		Status: &v1alpha1.GpuStatus{
			Conditions: []*v1alpha1.Condition{
				{
					Type:               ConditionTypeNVMLReady,
					Status:             status,
					Reason:             reason,
					Message:            message,
					LastTransitionTime: timestamppb.Now(),
				},
			},
		},
	}

	if _, err := p.gpuClient.UpdateGpuStatus(ctx, req); err != nil {
		p.logger.Error(err, "Failed to update GPU status", "uuid", uuid)
	}
}

// runHealthServer runs the HTTP health check server.
func (p *Provider) runHealthServer() {
	defer p.wg.Done()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", p.handleHealthz)
	mux.HandleFunc("/readyz", p.handleReadyz)
	mux.HandleFunc("/livez", p.handleHealthz)

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", p.config.HealthCheckPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}

	go func() {
		<-p.ctx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()

	p.logger.Info("Health server started", "port", p.config.HealthCheckPort)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		p.logger.Error(err, "Health server error")
	}
}

func (p *Provider) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok\n"))
}

func (p *Provider) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	p.mu.RLock()
	healthy := p.healthy
	p.mu.RUnlock()

	if healthy {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("not ready\n"))
	}
}

func (p *Provider) setHealthy(healthy bool) {
	p.mu.Lock()
	p.healthy = healthy
	p.mu.Unlock()
}

// Critical XIDs that indicate hardware failure.
var criticalXIDs = map[uint64]bool{
	48: true, 63: true, 64: true, 74: true, 79: true, 94: true, 95: true, 119: true, 120: true,
}

func isCriticalXID(xid uint64) bool {
	return criticalXIDs[xid]
}

func formatBytes(bytes uint64) string {
	const GB = 1024 * 1024 * 1024
	const MB = 1024 * 1024
	const KB = 1024

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
