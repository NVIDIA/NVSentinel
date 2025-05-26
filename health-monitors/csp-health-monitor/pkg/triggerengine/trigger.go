// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package trigger

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/csp-health-monitor/pkg/datastore"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/csp-health-monitor/pkg/metrics"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/util/wait"
	klog "k8s.io/klog/v2"
)

const (
	udsMaxRetries = 5
	udsRetryDelay = 5 * time.Second
	// Standard messages for health events
	maintenanceScheduledMessage = "CSP maintenance scheduled"
	maintenanceCompletedMessage = "CSP maintenance completed"
	quarantineTriggerType       = "quarantine"
	healthyTriggerType          = "healthy"
	queryTypeQuarantine         = "quarantine"
	queryTypeHealthy            = "healthy"
	failureReasonMapping        = "mapping"
	failureReasonUDS            = "uds"
	failureReasonDBUpdate       = "db_update"
)

// Engine polls the datastore for maintenance events and forwards the
// corresponding health signals to NVSentinel through the UDS connector.
type Engine struct {
	store        datastore.Store
	udsClient    pb.PlatformConnectorClient
	config       *config.Config
	pollInterval time.Duration
}

// NewEngine constructs a ready-to-run Engine instance.
func NewEngine(cfg *config.Config, store datastore.Store, udsClient pb.PlatformConnectorClient) *Engine {
	return &Engine{
		config:       cfg,
		store:        store,
		udsClient:    udsClient,
		pollInterval: time.Duration(cfg.MaintenanceEventPollIntervalSeconds) * time.Second,
	}
}

// Start begins the polling loop and blocks until ctx is cancelled.
func (e *Engine) Start(ctx context.Context) {
	klog.Infof("Starting Quarantine Trigger Engine polling every %v", e.pollInterval)

	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.Infof("Quarantine Trigger Engine stopping due to context cancellation.")
			return
		case <-ticker.C:
			metrics.TriggerPollCycles.Inc() // Increment poll cycle counter
			klog.V(1).InfoS("Quarantine Trigger Engine polling datastore...")

			startCycle := time.Now()

			if err := e.checkAndTriggerEvents(ctx); err != nil {
				metrics.TriggerPollErrors.Inc() // Increment poll error counter
				klog.Errorf("Error during trigger engine poll cycle: %v", err)
			}

			klog.V(2).Infof("Trigger engine poll cycle finished in %v", time.Since(startCycle))
		}
	}
}

// checkAndTriggerEvents queries the datastore and triggers necessary UDS events.
func (e *Engine) checkAndTriggerEvents(ctx context.Context) error {
	triggerLimit := time.Duration(e.config.TriggerQuarantineWorkflowTimeLimitMinutes) * time.Minute
	healthyDelay := time.Duration(e.config.PostMaintenanceHealthyDelayMinutes) * time.Minute

	// --- Check for quarantine triggers ---
	startQuery := time.Now()
	quarantineEvents, err := e.store.FindEventsToTriggerQuarantine(ctx, triggerLimit)
	queryDuration := time.Since(startQuery).Seconds()
	metrics.TriggerDatastoreQueryDuration.WithLabelValues(queryTypeQuarantine).Observe(queryDuration)

	if err != nil {
		metrics.TriggerDatastoreQueryErrors.WithLabelValues(queryTypeQuarantine).Inc()
		klog.Errorf("Failed to query for quarantine triggers: %v", err)

		return fmt.Errorf(
			"failed to query for quarantine triggers: %w",
			err,
		) // Return error to increment poll error metric
	}

	metrics.TriggerEventsFound.WithLabelValues(quarantineTriggerType).Add(float64(len(quarantineEvents)))
	klog.V(1).Infof("Found %d events potentially needing quarantine trigger.", len(quarantineEvents))

	for _, event := range quarantineEvents {
		if errTrig := e.triggerQuarantine(ctx, event); errTrig != nil {
			// Metrics incremented within triggerQuarantine
			klog.Errorf(
				"Error triggering quarantine for event %s (Node: %s): %v",
				event.EventID,
				event.NodeName,
				errTrig,
			)
		}
	}

	// --- Check for healthy triggers ---
	startQuery = time.Now()
	healthyEvents, err := e.store.FindEventsToTriggerHealthy(ctx, healthyDelay)
	queryDuration = time.Since(startQuery).Seconds()
	metrics.TriggerDatastoreQueryDuration.WithLabelValues(queryTypeHealthy).Observe(queryDuration)

	if err != nil {
		metrics.TriggerDatastoreQueryErrors.WithLabelValues(queryTypeHealthy).Inc()
		klog.Errorf("Failed to query for healthy triggers: %v", err)

		return fmt.Errorf("failed to query for healthy triggers: %w", err) // Return error
	}

	metrics.TriggerEventsFound.WithLabelValues(healthyTriggerType).Add(float64(len(healthyEvents)))
	klog.V(1).Infof("Found %d events potentially needing healthy trigger.", len(healthyEvents))

	for _, event := range healthyEvents {
		if errTrig := e.triggerHealthy(ctx, event); errTrig != nil {
			// Metrics incremented within triggerHealthy
			klog.Errorf("Error triggering healthy for event %s (Node: %s): %v", event.EventID, event.NodeName, errTrig)
		}
	}

	return nil // Poll cycle completed (though individual triggers might have failed)
}

// processAndSendTrigger is a helper to handle the common logic for sending quarantine or healthy triggers.
func (e *Engine) processAndSendTrigger(
	ctx context.Context,
	event model.MaintenanceEvent,
	triggerType string,
	isHealthy, isFatal bool,
	message string,
	targetDBStatus model.InternalStatus,
) error {
	metrics.TriggerAttempts.WithLabelValues(triggerType).Inc()

	if event.NodeName == "" {
		klog.Warningf(
			"Cannot trigger %s event for event %s: NodeName is missing. Skipping.",
			triggerType,
			event.EventID,
		)
		metrics.TriggerFailures.WithLabelValues(triggerType, failureReasonMapping).Inc()

		return fmt.Errorf("missing NodeName for %s trigger (EventID: %s)", triggerType, event.EventID)
	}

	klog.Infof(
		"Attempting to trigger %s event for node %s (EventID: %s)",
		strings.ToUpper(triggerType),
		event.NodeName,
		event.EventID,
	)

	healthEvent, mapErr := e.mapMaintenanceEventToHealthEvent(event, isHealthy, isFatal, message)
	if mapErr != nil {
		klog.Errorf(
			"Error mapping maintenance event to health event for %s trigger (EventID: %s): %v",
			triggerType,
			event.EventID,
			mapErr,
		)
		metrics.TriggerFailures.WithLabelValues(triggerType, failureReasonMapping).Inc()

		return fmt.Errorf("error mapping event %s for %s: %w", event.EventID, triggerType, mapErr)
	}

	udsErr := e.sendHealthEventWithRetry(ctx, healthEvent)
	if udsErr != nil {
		metrics.TriggerFailures.WithLabelValues(triggerType, failureReasonUDS).Inc()

		return fmt.Errorf(
			"failed to send %s health event via UDS for event %s after retries: %w",
			triggerType,
			event.EventID,
			udsErr,
		)
	}

	dbErr := e.store.UpdateEventStatus(ctx, event.EventID, targetDBStatus)
	if dbErr != nil {
		metrics.TriggerDatastoreUpdateErrors.WithLabelValues(triggerType).Inc()
		metrics.TriggerFailures.WithLabelValues(triggerType, failureReasonDBUpdate).Inc()
		klog.Errorf(
			"CRITICAL: Failed to update status to %s for"+
				" event %s after successfully sending UDS message: %v. Potential for duplicate triggers.",
			targetDBStatus,
			event.EventID,
			dbErr,
		)

		return fmt.Errorf(
			"failed to update event status post-%s-trigger for event %s: %w",
			triggerType,
			event.EventID,
			dbErr,
		)
	}

	metrics.TriggerSuccess.WithLabelValues(triggerType).Inc()
	klog.Infof(
		"Successfully triggered %s event and updated status for node %s (EventID: %s)",
		strings.ToUpper(triggerType),
		event.NodeName,
		event.EventID,
	)

	return nil
}

// triggerQuarantine constructs and sends an unhealthy (fatal=true) event via UDS.
func (e *Engine) triggerQuarantine(ctx context.Context, event model.MaintenanceEvent) error {
	return e.processAndSendTrigger(
		ctx,
		event,
		quarantineTriggerType,
		false,
		true,
		maintenanceScheduledMessage,
		model.StatusQuarantineTriggered,
	)
}

// triggerHealthy constructs and sends a healthy (isHealthy=true) event via UDS.
func (e *Engine) triggerHealthy(ctx context.Context, event model.MaintenanceEvent) error {
	return e.processAndSendTrigger(
		ctx,
		event,
		healthyTriggerType,
		true,
		false,
		maintenanceCompletedMessage,
		model.StatusHealthyTriggered,
	)
}

// mapMaintenanceEventToHealthEvent converts our internal MaintenanceEvent into
// the protobuf HealthEvent expected by the Platform Connector.
func (e *Engine) mapMaintenanceEventToHealthEvent(
	event model.MaintenanceEvent,
	isHealthy, isFatal bool,
	message string,
) (*pb.HealthEvent, error) {
	// Basic validation (redundant if nodeName check done by caller, but safe)
	if event.ResourceType == "" || event.ResourceID == "" || event.NodeName == "" {
		return nil, fmt.Errorf(
			"missing required fields (ResourceType, ResourceID, NodeName) for event %s",
			event.EventID,
		)
	}

	actionEnum, ok := pb.RecommenedAction_value[event.RecommendedAction]
	if !ok {
		klog.Warningf(
			"Unknown recommended action '%s' for event %s. Defaulting to NONE.",
			event.RecommendedAction,
			event.EventID,
		)

		actionEnum = int32(pb.RecommenedAction_NONE)
	}

	healthEvent := &pb.HealthEvent{
		Agent:             "csp-health-monitor", // Consistent agent name
		ComponentClass:    event.ResourceType,   // e.g., "EC2", "gce_instance"
		CheckName:         "CSPMaintenance",     // Consistent check name
		IsFatal:           isFatal,
		IsHealthy:         isHealthy,
		Message:           message,
		RecommendedAction: pb.RecommenedAction(actionEnum),
		EntitiesImpacted: []*pb.Entity{
			{
				EntityType:  event.ResourceType,
				EntityValue: event.ResourceID, // CSP's ID (e.g., instance-id, full gcp resource name)
			},
		},
		Metadata:           event.Metadata, // Pass along metadata
		NodeName:           event.NodeName, // K8s node name
		GeneratedTimestamp: timestamppb.New(time.Now()),
	}

	return healthEvent, nil
}

// isRetryableGRPCError checks if a gRPC error code indicates a potentially transient issue.
func isRetryableGRPCError(err error) bool {
	if err == nil {
		return false
	}

	st, ok := status.FromError(err)
	if !ok {
		// If it's not a gRPC status error, assume it's not retryable for simplicity
		return false
	}
	// Only retry on Unavailable, typically indicating temporary network or server issues
	return st.Code() == codes.Unavailable
}

// sendHealthEventWithRetry attempts to send a HealthEvent via UDS, with retries and metrics.
func (e *Engine) sendHealthEventWithRetry(ctx context.Context, healthEvent *pb.HealthEvent) error {
	backoff := wait.Backoff{
		Steps:    udsMaxRetries,
		Duration: udsRetryDelay,
		Factor:   1.5,
		Jitter:   0.1,
	}

	var lastErr error

	sendStart := time.Now() // Start timer before backoff loop

	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		healthEvents := &pb.HealthEvents{
			Events: []*pb.HealthEvent{healthEvent},
		}

		klog.V(2).Infof("Attempting to send health event via UDS (Node: %s, Check: %s, Fatal: %v, Healthy: %v)",
			healthEvent.NodeName, healthEvent.CheckName, healthEvent.IsFatal, healthEvent.IsHealthy)

		_, attemptErr := e.udsClient.HealthEventOccuredV1(ctx, healthEvents)
		lastErr = attemptErr // Store the error from this attempt

		if attemptErr == nil {
			klog.V(1).
				Infof("Successfully sent health event via UDS: (Node: %s, Check: %s)", healthEvent.NodeName, healthEvent.CheckName)
			return true, nil // Success
		}

		// Increment UDS error metric on each failed attempt
		metrics.TriggerUDSSendErrors.Inc()

		if isRetryableGRPCError(attemptErr) {
			klog.Warningf(
				"Retryable error sending health event via UDS (Node: %s): %v. Retrying...",
				healthEvent.NodeName,
				attemptErr,
			)

			return false, nil // Retryable error, continue loop
		}

		klog.Errorf("Non-retryable error sending health event via UDS (Node: %s): %v", healthEvent.NodeName, attemptErr)

		return false, attemptErr // Non-retryable error, stop loop and return this error
	})

	// Observe duration after the entire backoff process completes (success or failure)
	duration := time.Since(sendStart).Seconds()
	metrics.TriggerUDSSendDuration.Observe(duration)

	if wait.Interrupted(err) {
		// The loop timed out after all retries
		klog.Errorf(
			"Failed to send health event via UDS (Node: %s) after %d attempts due to timeout. Last error: %v",
			healthEvent.NodeName,
			udsMaxRetries,
			lastErr,
		)

		return fmt.Errorf("failed to send health event after %d retries (timeout): %w", udsMaxRetries, lastErr)
	}

	if err != nil {
		// This is the non-retryable error returned from the callback
		klog.Errorf(
			"Failed to send health event via UDS (Node: %s) due to non-retryable error: %v",
			healthEvent.NodeName,
			err,
		)

		return fmt.Errorf("failed to send health event (Node: %s): %w", healthEvent.NodeName, err)
	}

	return nil // Success
}
