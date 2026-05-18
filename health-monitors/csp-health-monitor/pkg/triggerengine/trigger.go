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

package trigger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/datastore"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

const (
	agentName     = "csp-health-monitor"
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
	defaultMonitorInterval      = 5 * time.Minute
)

// Engine polls the datastore for maintenance events and forwards the
// corresponding health signals to NVSentinel through the UDS connector.
type Engine struct {
	store              datastore.Store
	udsClient          pb.PlatformConnectorClient
	pub                *healthpub.Publisher
	config             *config.Config
	pollInterval       time.Duration
	k8sClient          kubernetes.Interface
	monitoredNodes     sync.Map // Track which nodes are currently being monitored
	monitorInterval    time.Duration
	processingStrategy pb.ProcessingStrategy
}

// NewEngine constructs a ready-to-run Engine instance. udsTarget must
// match the gRPC target string used to dial udsClient (typically
// "unix:/var/run/nvsentinel.sock"); pass "" in tests to disable the
// healthpub socket-existence gate.
func NewEngine(
	cfg *config.Config,
	store datastore.Store,
	udsClient pb.PlatformConnectorClient,
	udsTarget string,
	k8sClient kubernetes.Interface,
	processingStrategy pb.ProcessingStrategy,
) *Engine {
	return &Engine{
		config:    cfg,
		store:     store,
		udsClient: udsClient,
		pub: healthpub.New(udsClient, udsTarget, agentName,
			healthpub.WithRetryPolicy(udsMaxRetries, udsRetryDelay, 1.5, 0.1)),
		pollInterval:       time.Duration(cfg.MaintenanceEventPollIntervalSeconds) * time.Second,
		k8sClient:          k8sClient,
		monitorInterval:    defaultMonitorInterval,
		processingStrategy: processingStrategy,
	}
}

// Start begins the polling loop and blocks until ctx is cancelled.
func (e *Engine) Start(ctx context.Context) {
	slog.Info("Starting Quarantine Trigger Engine",
		"pollInterval", e.pollInterval)

	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("Quarantine Trigger Engine stopping due to context cancellation")
			return
		case <-ticker.C:
			metrics.TriggerPollCycles.Inc() // Increment poll cycle counter
			slog.Debug("Quarantine Trigger Engine polling datastore")

			startCycle := time.Now()

			if err := e.checkAndTriggerEvents(ctx); err != nil {
				metrics.TriggerPollErrors.Inc() // Increment poll error counter
				slog.Error("Error during trigger engine poll cycle", "error", err)
			}

			slog.Debug("Trigger engine poll cycle finished", "duration", time.Since(startCycle))
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
		slog.Error("Failed to query for quarantine triggers", "error", err)

		return fmt.Errorf(
			"failed to query for quarantine triggers: %w",
			err,
		) // Return error to increment poll error metric
	}

	metrics.TriggerEventsFound.WithLabelValues(quarantineTriggerType).Add(float64(len(quarantineEvents)))
	slog.Debug("Found events potentially needing quarantine trigger",
		"count", len(quarantineEvents))

	for _, event := range quarantineEvents {
		if errTrig := e.triggerQuarantine(ctx, event); errTrig != nil {
			// Metrics incremented within triggerQuarantine
			slog.Error("Error triggering quarantine for event",
				"eventID", event.EventID,
				"node", event.NodeName,
				"error", errTrig)
		}
	}

	// --- Check for healthy triggers ---
	startQuery = time.Now()
	healthyEvents, err := e.store.FindEventsToTriggerHealthy(ctx, healthyDelay)
	queryDuration = time.Since(startQuery).Seconds()
	metrics.TriggerDatastoreQueryDuration.WithLabelValues(queryTypeHealthy).Observe(queryDuration)

	if err != nil {
		metrics.TriggerDatastoreQueryErrors.WithLabelValues(queryTypeHealthy).Inc()
		slog.Error("Failed to query for healthy triggers", "error", err)

		return fmt.Errorf("failed to query for healthy triggers: %w", err) // Return error
	}

	metrics.TriggerEventsFound.WithLabelValues(healthyTriggerType).Add(float64(len(healthyEvents)))
	slog.Debug("Found events potentially needing healthy trigger",
		"count", len(healthyEvents))

	for _, event := range healthyEvents {
		ready, err := e.isNodeReady(ctx, event.NodeName)
		if err != nil {
			slog.Error("Failed to confirm node readiness, will check next polling interval",
				"eventID", event.EventID,
				"node", event.NodeName,
				"error", err)

			continue
		}

		if ready {
			// Node is ready, proceed with triggering healthy event
			if errTrig := e.triggerHealthy(ctx, event); errTrig != nil {
				// Metrics incremented within triggerHealthy
				slog.Error("Error triggering healthy for event",
					"eventID", event.EventID,
					"node", event.NodeName,
					"error", errTrig)
			}
		} else {
			// Node is not ready, start background monitoring if not already monitoring
			_, alreadyMonitoring := e.monitoredNodes.LoadOrStore(event.NodeName, true)
			if !alreadyMonitoring {
				slog.Debug(
					"Node %s is not Ready yet. Starting background monitoring for event %s.",
					event.NodeName,
					event.EventID,
				)

				// Increment monitoring started metric
				metrics.NodeReadinessMonitoringStarted.WithLabelValues(event.NodeName).Inc()

				// Start background monitoring in a goroutine
				go e.monitorNodeReadiness(context.Background(), event.NodeName, event.EventID, event)
			} else {
				slog.Debug(
					"Node %s is already being monitored. Deferring healthy trigger for event %s.",
					event.NodeName,
					event.EventID,
				)
			}
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
		slog.Warn(
			"Skipping the event; NodeName is missing.",
			"triggerType", triggerType,
			"eventID", event.EventID,
		)
		metrics.TriggerFailures.WithLabelValues(triggerType, failureReasonMapping).Inc()

		return fmt.Errorf("missing NodeName for %s trigger (EventID: %s)", triggerType, event.EventID)
	}

	slog.Info("Attempting to trigger event",
		"type", strings.ToUpper(triggerType),
		"node", event.NodeName,
		"eventID", event.EventID)

	healthEvent, mapErr := e.mapMaintenanceEventToHealthEvent(event, isHealthy, isFatal, message)
	if mapErr != nil {
		slog.Error("Error mapping maintenance event to health event",
			"triggerType", triggerType,
			"eventID", event.EventID,
			"error", mapErr)
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
		slog.Error("CRITICAL: Failed to update status after sending UDS message",
			"eventID", event.EventID,
			"targetStatus", targetDBStatus,
			"error", dbErr,
			"warning", "Potential for duplicate triggers")

		return fmt.Errorf(
			"failed to update event status post-%s-trigger for event %s: %w",
			triggerType,
			event.EventID,
			dbErr,
		)
	}

	metrics.TriggerSuccess.WithLabelValues(triggerType).Inc()
	slog.Info("Successfully triggered event and updated status",
		"type", strings.ToUpper(triggerType),
		"node", event.NodeName,
		"eventID", event.EventID)

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

	actionEnum, ok := pb.RecommendedAction_value[event.RecommendedAction]
	if !ok {
		slog.Warn(
			"Unknown recommended action; defaulting to NONE.",
			"recommendedAction",
			event.RecommendedAction,
			"eventID",
			event.EventID,
		)

		actionEnum = int32(pb.RecommendedAction_NONE)
	}

	healthEvent := &pb.HealthEvent{
		Agent:              "csp-health-monitor", // Consistent agent name
		ComponentClass:     event.ResourceType,   // e.g., "EC2", "gce_instance"
		CheckName:          "CSPMaintenance",     // Consistent check name
		IsFatal:            isFatal,
		IsHealthy:          isHealthy,
		ProcessingStrategy: e.processingStrategy,
		Message:            message,
		RecommendedAction:  pb.RecommendedAction(actionEnum),
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

// sendHealthEventWithRetry forwards a HealthEvent to platform-connector
// via the shared healthpub publisher.
//
// TriggerUDSSendDuration and TriggerUDSSendErrors are preserved for csp
// dashboards. TriggerUDSSendErrors now increments once per terminal
// failure (not once per retry attempt as before).
func (e *Engine) sendHealthEventWithRetry(ctx context.Context, healthEvent *pb.HealthEvent) error {
	healthEvents := &pb.HealthEvents{
		Events: []*pb.HealthEvent{healthEvent},
	}

	slog.Debug("Attempting to send health event via UDS",
		"node", healthEvent.NodeName,
		"check", healthEvent.CheckName,
		"fatal", healthEvent.IsFatal,
		"healthy", healthEvent.IsHealthy)

	sendStart := time.Now()

	err := e.pub.Publish(ctx, healthEvents)

	metrics.TriggerUDSSendDuration.Observe(time.Since(sendStart).Seconds())

	if err == nil {
		slog.Debug("Successfully sent health event via UDS",
			"node", healthEvent.NodeName,
			"check", healthEvent.CheckName)

		return nil
	}

	// Connector-unavailable skips are not UDS failures — the gRPC call
	// never happened. Don't increment TriggerUDSSendErrors for them.
	if errors.Is(err, healthpub.ErrPlatformConnectorUnavailable) {
		slog.Warn("Skipped health event send: platform-connector unavailable. "+
			"Next poll will re-evaluate and re-stamp.",
			"node", healthEvent.NodeName,
			"check", healthEvent.CheckName)

		return fmt.Errorf("failed to send health event (Node: %s): %w", healthEvent.NodeName, err)
	}

	metrics.TriggerUDSSendErrors.Inc()
	slog.Error("Failed to send health event via UDS",
		"node", healthEvent.NodeName,
		"error", err)

	return fmt.Errorf("failed to send health event (Node: %s): %w", healthEvent.NodeName, err)
}

func (e *Engine) isNodeReady(ctx context.Context, nodeName string) (bool, error) {
	if nodeName == "" {
		return false, fmt.Errorf("node name is empty")
	}

	node, err := e.k8sClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue, nil
		}
	}

	return false, nil
}

// monitorNodeReadiness monitors a node's readiness status with exponential backoff
// and creates an alert if the node is not ready after 1 hour
func (e *Engine) monitorNodeReadiness(ctx context.Context, nodeName, eventID string, event model.MaintenanceEvent) {
	defer func() {
		// Clean up the monitoring flag when done
		slog.Info("Deleting monitoring flag for node",
			"node", nodeName,
			"eventID", eventID)
		e.monitoredNodes.Delete(nodeName)
	}()

	slog.Info("Starting background node readiness monitoring",
		"node", nodeName,
		"eventID", eventID)

	// Create a context with configurable timeout
	nodeReadinessTimeout := time.Duration(e.config.NodeReadinessTimeoutMinutes) * time.Minute

	monitorCtx, cancel := context.WithTimeout(ctx, nodeReadinessTimeout)

	defer cancel()

	// Start periodic monitoring with fixed interval (node was confirmed NOT ready before calling this function)

	ticker := time.NewTicker(e.monitorInterval)
	defer ticker.Stop()

	startTime := time.Now()

	var err error

	for {
		select {
		case <-monitorCtx.Done():
			// Context timeout or cancellation - monitoring period ended
			duration := time.Since(startTime)
			err = monitorCtx.Err()

			if err == context.DeadlineExceeded {
				slog.Error("ALERT: Node readiness timeout exceeded",
					"node", nodeName,
					"duration", duration,
					"eventID", eventID,
					"message", "Node has been not Ready for extended period")

				metrics.NodeNotReadyTimeout.WithLabelValues(nodeName).Inc()

				if err := e.store.UpdateEventStatus(ctx, eventID, model.StatusNodeReadinessTimeout); err != nil {
					slog.Error("Failed to update event status to NODE_READINESS_TIMEOUT",
						"eventID", eventID,
						"error", err)
				}
			} else if err != nil {
				slog.Error("Background node readiness monitoring failed",
					"node", nodeName,
					"eventID", eventID,
					"error", err)
			}

			return
		case <-ticker.C:
			ready, err := e.isNodeReady(monitorCtx, nodeName)
			if err != nil {
				slog.Debug("Error checking node readiness during background monitoring",
					"node", nodeName,
					"error", err,
					"message", "Will retry in next interval")

				continue
			}

			if ready {
				elapsed := time.Since(startTime)
				slog.Info("Node became Ready, triggering healthy event",
					"node", nodeName,
					"monitoringDuration", elapsed)

				if errTrig := e.triggerHealthy(monitorCtx, event); errTrig != nil {
					slog.Error("Error triggering healthy for event",
						"eventID", event.EventID,
						"node", event.NodeName,
						"error", errTrig)
				}

				return
			}

			elapsed := time.Since(startTime)
			slog.Debug("Node still not Ready, will check again",
				"node", nodeName,
				"elapsed", elapsed,
				"nextCheck", e.monitorInterval)
		}
	}
}
