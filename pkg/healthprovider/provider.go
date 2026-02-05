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

// Package healthprovider provides a modular GPU health monitoring service.
//
// The HealthProvider coordinates multiple health sources (NVML, syslog, DCGM, CSP)
// and sends health events to the device-api-server via gRPC.
//
// Architecture:
//
//	┌──────────────────────────────────────────────────────┐
//	│                  HealthProvider                       │
//	│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐ │
//	│  │   NVML   │ │  Syslog  │ │   DCGM   │ │   CSP    │ │
//	│  │  Source  │ │  Source  │ │  Source  │ │  Source  │ │
//	│  └────┬─────┘ └────┬─────┘ └────┬─────┘ └────┬─────┘ │
//	│       └────────────┴────────────┴────────────┘       │
//	│                        │                              │
//	│              HealthEventHandler                       │
//	└────────────────────────┼─────────────────────────────┘
//	                         │ gRPC
//	                         ▼
//	              ┌─────────────────────┐
//	              │  device-api-server  │
//	              └─────────────────────┘
package healthprovider

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/klog/v2"

	v1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"
)

const (
	// DefaultHeartbeatInterval is how often to send heartbeats.
	DefaultHeartbeatInterval = 10 * time.Second

	// DefaultHealthCheckPort is the HTTP port for health checks.
	DefaultHealthCheckPort = 8082

	// DefaultServerAddress is the default device-api-server address.
	DefaultServerAddress = "localhost:9001"

	// DefaultConnectionRetryInterval is how long to wait between connection attempts.
	DefaultConnectionRetryInterval = 5 * time.Second

	// DefaultMaxConnectionRetries is the maximum number of connection attempts.
	DefaultMaxConnectionRetries = 60
)

// HealthSource is an interface for pluggable health monitoring sources.
type HealthSource interface {
	// Name returns the source identifier (e.g., "nvml", "syslog", "dcgm").
	Name() string

	// Start begins health monitoring. Returns an error if initialization fails.
	Start(ctx context.Context, handler HealthEventHandler) error

	// Stop stops health monitoring and releases resources.
	Stop()

	// IsRunning returns true if the source is actively monitoring.
	IsRunning() bool
}

// HealthEventHandler is the interface for handling health events from sources.
type HealthEventHandler interface {
	// OnGPUDiscovered is called when a new GPU is discovered.
	OnGPUDiscovered(ctx context.Context, gpu *GPUInfo) error

	// OnGPUHealthChanged is called when GPU health status changes.
	OnGPUHealthChanged(ctx context.Context, event *HealthEvent) error

	// OnGPURemoved is called when a GPU is no longer present.
	OnGPURemoved(ctx context.Context, gpuUUID string) error
}

// GPUInfo contains information about a discovered GPU.
type GPUInfo struct {
	UUID        string
	ProductName string
	MemoryBytes uint64
	Index       int
	Source      string
}

// HealthEvent represents a health status change event.
type HealthEvent struct {
	GPUUID     string
	Source     string
	IsHealthy  bool
	IsFatal    bool
	ErrorCodes []string
	Reason     string
	Message    string
	DetectedAt time.Time
}

// Config holds configuration for the HealthProvider.
type Config struct {
	// ServerAddress is the device-api-server gRPC address.
	ServerAddress string

	// ProviderID is the unique identifier for this provider instance.
	ProviderID string

	// NodeName is the Kubernetes node name.
	NodeName string

	// HealthCheckPort is the HTTP port for liveness/readiness probes.
	HealthCheckPort int

	// HeartbeatInterval is how often to verify server connectivity.
	HeartbeatInterval time.Duration

	// ConnectionRetryInterval is how long to wait between connection attempts.
	ConnectionRetryInterval time.Duration

	// MaxConnectionRetries is the maximum number of connection attempts.
	MaxConnectionRetries int
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		ServerAddress:           DefaultServerAddress,
		ProviderID:              "health-provider",
		NodeName:                "",
		HealthCheckPort:         DefaultHealthCheckPort,
		HeartbeatInterval:       DefaultHeartbeatInterval,
		ConnectionRetryInterval: DefaultConnectionRetryInterval,
		MaxConnectionRetries:    DefaultMaxConnectionRetries,
	}
}

// Provider is the main health provider service.
type Provider struct {
	config Config
	logger klog.Logger

	// Health sources
	sources []HealthSource

	// gRPC connection
	conn         *grpc.ClientConn
	gpuClient    v1alpha1.GpuServiceClient
	healthClient grpc_health_v1.HealthClient

	// State
	mu         sync.RWMutex
	connected  bool
	healthy    bool
	gpuUUIDs   map[string]bool
	sourceInfo map[string]bool // tracks which sources are running

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a new HealthProvider.
func New(cfg Config, logger klog.Logger) *Provider {
	return &Provider{
		config:     cfg,
		logger:     logger.WithName("health-provider"),
		sources:    make([]HealthSource, 0),
		gpuUUIDs:   make(map[string]bool),
		sourceInfo: make(map[string]bool),
	}
}

// RegisterSource adds a health source to the provider.
// Must be called before Start().
func (p *Provider) RegisterSource(source HealthSource) {
	p.sources = append(p.sources, source)
	p.logger.Info("Registered health source", "source", source.Name())
}

// Run starts the provider and blocks until context is cancelled.
func (p *Provider) Run(ctx context.Context) error {
	p.ctx, p.cancel = context.WithCancel(ctx)
	defer p.cancel()

	// Start HTTP health server
	p.wg.Add(1)
	go p.runHealthServer()

	// Connect to device-api-server
	if err := p.connectWithRetry(); err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}
	defer p.disconnect()

	// Start all health sources
	sourcesStarted := 0
	for _, source := range p.sources {
		p.logger.Info("Starting health source", "source", source.Name())
		if err := source.Start(p.ctx, p); err != nil {
			p.logger.Error(err, "Failed to start health source", "source", source.Name())
			continue
		}
		p.sourceInfo[source.Name()] = true
		sourcesStarted++
	}

	if sourcesStarted == 0 && len(p.sources) > 0 {
		return fmt.Errorf("no health sources could be started")
	}

	// Start heartbeat loop
	p.wg.Add(1)
	go p.runHeartbeatLoop()

	// Mark as healthy
	p.setHealthy(true)
	p.logger.Info("Health provider running",
		"sources", sourcesStarted,
		"serverAddress", p.config.ServerAddress,
	)

	// Wait for shutdown
	<-p.ctx.Done()

	// Graceful shutdown
	p.setHealthy(false)
	for _, source := range p.sources {
		source.Stop()
	}
	p.wg.Wait()

	return nil
}

// connectWithRetry establishes connection to device-api-server with retry logic.
func (p *Provider) connectWithRetry() error {
	var lastErr error

	for i := 0; i < p.config.MaxConnectionRetries; i++ {
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
			p.logger.V(1).Info("Connection attempt failed",
				"attempt", i+1,
				"error", err,
			)
			time.Sleep(p.config.ConnectionRetryInterval)
			continue
		}

		p.conn = conn
		p.gpuClient = v1alpha1.NewGpuServiceClient(conn)
		p.healthClient = grpc_health_v1.NewHealthClient(conn)

		// Wait for server to be ready
		if err := p.waitForServerReady(); err != nil {
			conn.Close()
			lastErr = err
			p.logger.V(1).Info("Server not ready",
				"attempt", i+1,
				"error", err,
			)
			time.Sleep(p.config.ConnectionRetryInterval)
			continue
		}

		p.mu.Lock()
		p.connected = true
		p.mu.Unlock()

		p.logger.Info("Connected to device-api-server",
			"address", p.config.ServerAddress,
		)
		return nil
	}

	return fmt.Errorf("failed to connect after %d attempts: %w",
		p.config.MaxConnectionRetries, lastErr)
}

// waitForServerReady checks if the server is healthy.
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
	p.mu.Lock()
	p.connected = false
	p.mu.Unlock()
}

// HealthEventHandler interface implementation

// OnGPUDiscovered registers a newly discovered GPU with device-api-server.
func (p *Provider) OnGPUDiscovered(ctx context.Context, gpu *GPUInfo) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.gpuUUIDs[gpu.UUID] {
		// Already registered
		return nil
	}

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req := &v1alpha1.CreateGpuRequest{
		Gpu: &v1alpha1.Gpu{
			Metadata: &v1alpha1.ObjectMeta{Name: gpu.UUID},
			Spec:     &v1alpha1.GpuSpec{Uuid: gpu.UUID},
			Status: &v1alpha1.GpuStatus{
				Conditions: []*v1alpha1.Condition{
					{
						Type:               fmt.Sprintf("%sReady", gpu.Source),
						Status:             "True",
						Reason:             "Initialized",
						Message:            fmt.Sprintf("GPU discovered via %s: %s", gpu.Source, gpu.ProductName),
						LastTransitionTime: timestamppb.Now(),
					},
				},
			},
		},
	}

	_, err := p.gpuClient.CreateGpu(reqCtx, req)
	if err != nil {
		return fmt.Errorf("failed to create GPU: %w", err)
	}

	p.gpuUUIDs[gpu.UUID] = true
	p.logger.Info("GPU registered",
		"uuid", gpu.UUID,
		"productName", gpu.ProductName,
		"source", gpu.Source,
	)

	return nil
}

// OnGPUHealthChanged updates GPU health status in device-api-server.
func (p *Provider) OnGPUHealthChanged(ctx context.Context, event *HealthEvent) error {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	status := "True"
	if !event.IsHealthy {
		status = "False"
	}

	req := &v1alpha1.UpdateGpuStatusRequest{
		Name: event.GPUUID,
		Status: &v1alpha1.GpuStatus{
			Conditions: []*v1alpha1.Condition{
				{
					Type:               fmt.Sprintf("%sReady", event.Source),
					Status:             status,
					Reason:             event.Reason,
					Message:            event.Message,
					LastTransitionTime: timestamppb.New(event.DetectedAt),
				},
			},
		},
	}

	_, err := p.gpuClient.UpdateGpuStatus(reqCtx, req)
	if err != nil {
		return fmt.Errorf("failed to update GPU status: %w", err)
	}

	p.logger.Info("GPU health updated",
		"uuid", event.GPUUID,
		"healthy", event.IsHealthy,
		"fatal", event.IsFatal,
		"reason", event.Reason,
		"source", event.Source,
	)

	return nil
}

// OnGPURemoved handles GPU removal.
func (p *Provider) OnGPURemoved(ctx context.Context, gpuUUID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.gpuUUIDs[gpuUUID] {
		return nil
	}

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := p.gpuClient.DeleteGpu(reqCtx, &v1alpha1.DeleteGpuRequest{Name: gpuUUID})
	if err != nil {
		return fmt.Errorf("failed to delete GPU: %w", err)
	}

	delete(p.gpuUUIDs, gpuUUID)
	p.logger.Info("GPU removed", "uuid", gpuUUID)

	return nil
}

// runHeartbeatLoop periodically verifies server connectivity.
func (p *Provider) runHeartbeatLoop() {
	defer p.wg.Done()

	ticker := time.NewTicker(p.config.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			if err := p.waitForServerReady(); err != nil {
				p.logger.Error(err, "Heartbeat failed")
			}
		}
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
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

// IsConnected returns true if connected to device-api-server.
func (p *Provider) IsConnected() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.connected
}

// IsHealthy returns true if the provider is healthy.
func (p *Provider) IsHealthy() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.healthy
}

// GPUCount returns the number of registered GPUs.
func (p *Provider) GPUCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.gpuUUIDs)
}
