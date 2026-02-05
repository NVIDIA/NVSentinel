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

// Package crdpublisher creates HealthEvent CRDs from GPU status changes.
package crdpublisher

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devicev1alpha1 "github.com/nvidia/nvsentinel/api/device/v1alpha1"
	nvsentinelv1alpha1 "github.com/nvidia/nvsentinel/api/nvsentinel/v1alpha1"
)

// Config configures the CRD publisher.
type Config struct {
	// NodeName is the name of the node this publisher runs on.
	NodeName string

	// Enabled controls whether CRD publishing is active.
	Enabled bool

	// DebounceInterval is the minimum time between CRD updates for the same GPU.
	DebounceInterval time.Duration

	// Source identifies this publisher (e.g., "nvml-health-monitor").
	Source string
}

// Publisher creates HealthEvent CRDs when GPU status changes.
type Publisher struct {
	config Config
	client client.Client
	logger klog.Logger

	// Debouncing
	mu           sync.Mutex
	lastPublish  map[string]time.Time // GPU UUID -> last publish time
	pendingGPUs  map[string]*devicev1alpha1.GPU
	debounceStop chan struct{}
}

// New creates a new CRD publisher.
func New(config Config, logger klog.Logger) (*Publisher, error) {
	if !config.Enabled {
		return &Publisher{config: config, logger: logger}, nil
	}

	// Create in-cluster client
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	// Register nvsentinel scheme
	if err := nvsentinelv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return nil, fmt.Errorf("failed to register nvsentinel scheme: %w", err)
	}

	k8sClient, err := client.New(restConfig, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s client: %w", err)
	}

	if config.DebounceInterval == 0 {
		config.DebounceInterval = 5 * time.Second
	}
	if config.Source == "" {
		config.Source = "device-api-server"
	}

	return &Publisher{
		config:       config,
		client:       k8sClient,
		logger:       logger.WithName("crd-publisher"),
		lastPublish:  make(map[string]time.Time),
		pendingGPUs:  make(map[string]*devicev1alpha1.GPU),
		debounceStop: make(chan struct{}),
	}, nil
}

// Start starts the debounce processor.
func (p *Publisher) Start(ctx context.Context) {
	if !p.config.Enabled {
		return
	}

	go p.runDebounceProcessor(ctx)
}

// Stop stops the publisher.
func (p *Publisher) Stop() {
	if p.debounceStop != nil {
		close(p.debounceStop)
	}
}

// OnGPUUnhealthy is called when a GPU becomes unhealthy.
// It creates or updates a HealthEvent CRD.
func (p *Publisher) OnGPUUnhealthy(ctx context.Context, gpu *devicev1alpha1.GPU) {
	if !p.config.Enabled || p.client == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	uuid := gpu.Spec.UUID
	if uuid == "" {
		p.logger.V(1).Info("GPU has no UUID, skipping CRD publish")
		return
	}

	// Check debounce
	if lastTime, ok := p.lastPublish[uuid]; ok {
		if time.Since(lastTime) < p.config.DebounceInterval {
			// Queue for later
			p.pendingGPUs[uuid] = gpu
			p.logger.V(2).Info("Debouncing GPU update", "uuid", uuid)
			return
		}
	}

	// Publish immediately
	p.lastPublish[uuid] = time.Now()
	go p.publishHealthEvent(ctx, gpu)
}

// OnGPUHealthy is called when a GPU becomes healthy again.
// It can update an existing HealthEvent CRD to resolved status.
func (p *Publisher) OnGPUHealthy(ctx context.Context, gpu *devicev1alpha1.GPU) {
	if !p.config.Enabled || p.client == nil {
		return
	}

	// For now, we don't auto-resolve events
	// The resolution should come from the remediation workflow
	p.logger.V(1).Info("GPU healthy, consider manual resolution",
		"uuid", gpu.Spec.UUID,
		"nodeName", p.config.NodeName,
	)
}

// runDebounceProcessor processes pending GPU updates.
func (p *Publisher) runDebounceProcessor(ctx context.Context) {
	ticker := time.NewTicker(p.config.DebounceInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.debounceStop:
			return
		case <-ticker.C:
			p.processPendingGPUs(ctx)
		}
	}
}

// processPendingGPUs publishes any pending GPU updates.
func (p *Publisher) processPendingGPUs(ctx context.Context) {
	p.mu.Lock()
	pending := make(map[string]*devicev1alpha1.GPU)
	for uuid, gpu := range p.pendingGPUs {
		pending[uuid] = gpu
	}
	p.pendingGPUs = make(map[string]*devicev1alpha1.GPU)
	p.mu.Unlock()

	for uuid, gpu := range pending {
		p.mu.Lock()
		p.lastPublish[uuid] = time.Now()
		p.mu.Unlock()

		p.publishHealthEvent(ctx, gpu)
	}
}

// publishHealthEvent creates or updates a HealthEvent CRD.
func (p *Publisher) publishHealthEvent(ctx context.Context, gpu *devicev1alpha1.GPU) {
	log := p.logger.WithValues("uuid", gpu.Spec.UUID)

	// Extract error info from GPU status
	errorCodes, message, isFatal := p.extractErrorInfo(gpu)

	// Generate event name: nodename-gpuindex-errorcode-timestamp
	eventName := p.generateEventName(gpu)

	healthEvent := &nvsentinelv1alpha1.HealthEvent{
		ObjectMeta: metav1.ObjectMeta{
			Name: eventName,
			Labels: map[string]string{
				"nvsentinel.nvidia.com/node":            p.config.NodeName,
				"nvsentinel.nvidia.com/component-class": "gpu",
				"nvsentinel.nvidia.com/is-fatal":        fmt.Sprintf("%t", isFatal),
				"nvsentinel.nvidia.com/source":          p.config.Source,
			},
		},
		Spec: nvsentinelv1alpha1.HealthEventSpec{
			Source:         p.config.Source,
			NodeName:       p.config.NodeName,
			ComponentClass: "GPU",
			CheckName:      p.determineCheckName(gpu),
			IsFatal:        isFatal,
			IsHealthy:      false,
			Message:        message,
			ErrorCodes:     errorCodes,
			DetectedAt:     metav1.Now(),
			EntitiesImpacted: []nvsentinelv1alpha1.Entity{
				{Type: "GPU", Value: gpu.Spec.UUID},
			},
			Metadata: map[string]string{
				"uuid": gpu.Spec.UUID,
			},
		},
		Status: nvsentinelv1alpha1.HealthEventStatus{
			Phase: nvsentinelv1alpha1.PhaseNew,
		},
	}

	// Try to create the HealthEvent
	err := p.client.Create(ctx, healthEvent)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			log.V(1).Info("HealthEvent already exists", "name", eventName)
			return
		}
		log.Error(err, "Failed to create HealthEvent", "name", eventName)
		return
	}

	log.Info("Created HealthEvent CRD",
		"name", eventName,
		"isFatal", isFatal,
		"errorCodes", errorCodes,
	)

	// Record metric
	healthEventsCreatedTotal.WithLabelValues(p.config.NodeName, p.config.Source).Inc()
}

// extractErrorInfo extracts error information from GPU status conditions.
func (p *Publisher) extractErrorInfo(gpu *devicev1alpha1.GPU) (errorCodes []string, message string, isFatal bool) {
	for _, cond := range gpu.Status.Conditions {
		if cond.Type == "Healthy" && cond.Status == metav1.ConditionFalse {
			message = cond.Message
			if cond.Reason != "" {
				// Extract XID from reason if present (e.g., "XID79")
				if strings.HasPrefix(cond.Reason, "XID") {
					xid := strings.TrimPrefix(cond.Reason, "XID")
					errorCodes = append(errorCodes, xid)
				}
			}
		}
	}

	// Determine if fatal based on recommended action
	isFatal = gpu.Status.RecommendedAction == "RESTART_BM" ||
		gpu.Status.RecommendedAction == "RESTART_VM" ||
		gpu.Status.RecommendedAction == "REPLACE_VM"

	if message == "" {
		message = "GPU reported unhealthy"
	}

	return errorCodes, message, isFatal
}

// generateEventName creates a unique name for the HealthEvent.
func (p *Publisher) generateEventName(gpu *devicev1alpha1.GPU) string {
	// Use UUID short form + timestamp
	uuid := gpu.Spec.UUID
	if len(uuid) > 12 {
		uuid = uuid[len(uuid)-12:]
	}
	uuid = strings.ReplaceAll(uuid, "-", "")
	uuid = strings.ToLower(uuid)

	timestamp := time.Now().Unix()

	return fmt.Sprintf("%s-%s-%d", p.config.NodeName, uuid, timestamp)
}

// determineCheckName determines the check name based on GPU conditions.
func (p *Publisher) determineCheckName(gpu *devicev1alpha1.GPU) string {
	for _, cond := range gpu.Status.Conditions {
		if cond.Type == "Healthy" && cond.Status == metav1.ConditionFalse {
			if strings.Contains(cond.Reason, "XID") {
				return "xid-error-check"
			}
			if strings.Contains(cond.Reason, "ECC") {
				return "memory-ecc-check"
			}
			if strings.Contains(cond.Reason, "Thermal") || strings.Contains(cond.Reason, "Temperature") {
				return "thermal-check"
			}
		}
	}
	return "health-check"
}
