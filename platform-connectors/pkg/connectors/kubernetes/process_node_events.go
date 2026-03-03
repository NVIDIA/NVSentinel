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

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

const (
	DefaultNamespace   = "default"
	NoHealthFailureMsg = "No Health Failures"
	truncationSuffix   = "..."
)

// updateNodeConditions updates node conditions for a single node.
// All healthEvents must belong to the same node; callers must partition by NodeName.
func (r *K8sConnector) updateNodeConditions(ctx context.Context, healthEvents []*protos.HealthEvent) (bool, error) {
	sortedHealthEvents := sortHealthEventsByTimestamp(healthEvents)
	conditionEventsMap := buildConditionEventsMap(sortedHealthEvents)

	if len(conditionEventsMap) == 0 {
		return false, nil
	}

	nodeName := healthEvents[0].NodeName

	err := retry.OnError(retry.DefaultRetry, func(err error) bool {
		return apierrors.IsConflict(err) || isTemporaryError(err)
	}, func() error {
		node, err := r.clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		for conditionType, events := range conditionEventsMap {
			r.processNodeCondition(node, conditionType, events)
		}

		_, err = r.clientset.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})

		return err
	})
	if err != nil {
		conditionTypes := make([]string, 0, len(conditionEventsMap))
		for ct := range conditionEventsMap {
			conditionTypes = append(conditionTypes, string(ct))
		}

		slog.Error("Failed to update node conditions",
			"node", nodeName,
			"conditionTypes", conditionTypes,
			"error", err)

		return true, fmt.Errorf("failed to update node %s conditions: %w", nodeName, err)
	}

	return true, nil
}

func sortHealthEventsByTimestamp(events []*protos.HealthEvent) []*protos.HealthEvent {
	sorted := slices.Clone(events)

	slices.SortFunc(sorted, func(a, b *protos.HealthEvent) int {
		ti := a.GeneratedTimestamp
		tj := b.GeneratedTimestamp

		if ti == nil && tj == nil {
			return 0
		}

		if ti == nil {
			return -1
		}

		if tj == nil {
			return 1
		}

		return ti.AsTime().Compare(tj.AsTime())
	})

	return sorted
}

func buildConditionEventsMap(events []*protos.HealthEvent) map[corev1.NodeConditionType][]*protos.HealthEvent {
	conditionMap := make(map[corev1.NodeConditionType][]*protos.HealthEvent)

	for _, event := range events {
		if !event.IsHealthy && !event.IsFatal {
			continue
		}

		conditionType := corev1.NodeConditionType(string(event.CheckName))
		conditionMap[conditionType] = append(conditionMap[conditionType], event)
	}

	return conditionMap
}

func (r *K8sConnector) processNodeCondition(node *corev1.Node, conditionType corev1.NodeConditionType,
	events []*protos.HealthEvent) {
	if len(events) == 0 {
		return
	}

	latestEvent := events[len(events)-1]
	latestTime := metav1.NewTime(safeTimestamp(latestEvent.GeneratedTimestamp))

	matchedCondition, conditionIndex, conditionExists := findNodeCondition(node, conditionType)

	if !conditionExists {
		matchedCondition = corev1.NodeCondition{
			Type:               conditionType,
			LastHeartbeatTime:  latestTime,
			LastTransitionTime: latestTime,
		}
	}

	messages := parseMessages(matchedCondition.Message)
	messages = r.aggregateEventMessages(messages, events)

	if len(messages) > 0 {
		matchedCondition.Message = r.truncateNodeConditionMessage(messages)
		matchedCondition.Status = corev1.ConditionTrue
		matchedCondition.Reason = r.updateHealthEventReason(latestEvent.CheckName, false)
	} else {
		matchedCondition.Message = NoHealthFailureMsg
		matchedCondition.Status = corev1.ConditionFalse
		matchedCondition.Reason = r.updateHealthEventReason(latestEvent.CheckName, true)
	}

	matchedCondition.LastHeartbeatTime = latestTime

	// node.Status.Conditions[conditionIndex].Status is the pre-update value here because
	// matchedCondition is a copy and hasn't been written back yet (write-back happens below).
	if conditionExists && matchedCondition.Status != node.Status.Conditions[conditionIndex].Status {
		matchedCondition.LastTransitionTime = latestTime
	}

	if conditionExists {
		node.Status.Conditions[conditionIndex] = matchedCondition
	} else {
		node.Status.Conditions = append(node.Status.Conditions, matchedCondition)
	}
}

func safeTimestamp(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		slog.Warn("HealthEvent has nil GeneratedTimestamp, falling back to current time")

		return time.Now()
	}

	return ts.AsTime()
}

func findNodeCondition(node *corev1.Node,
	conditionType corev1.NodeConditionType) (corev1.NodeCondition, int, bool) {
	for i, c := range node.Status.Conditions {
		if c.Type == conditionType {
			return c, i, true
		}
	}

	return corev1.NodeCondition{}, 0, false
}

// aggregateEventMessages builds the consolidated message list for a node condition.
// Events are pre-filtered by buildConditionEventsMap to IsHealthy || IsFatal,
// so !IsHealthy here implies IsFatal && !IsHealthy (a fatal fault event).
func (r *K8sConnector) aggregateEventMessages(messages []string, events []*protos.HealthEvent) []string {
	for _, event := range events {
		switch {
		case !event.IsHealthy:
			messages = r.addMessageIfNotExist(messages, event)
		case len(event.EntitiesImpacted) > 0:
			messages = r.removeImpactedEntitiesMessages(messages, event.EntitiesImpacted)
		default: // healthy event with no impacted entities — full recovery, clear all messages
			messages = []string{}
		}
	}

	return messages
}

func parseMessages(message string) []string {
	var messages []string

	if message != "" && message != NoHealthFailureMsg {
		elementMessages := strings.Split(message, ";")
		for _, msg := range elementMessages {
			if msg != "" && msg != truncationSuffix {
				messages = append(messages, msg)
			}
		}
	}

	return messages
}

func (r *K8sConnector) addMessageIfNotExist(messages []string, healthEvent *protos.HealthEvent) []string {
	newMessage := r.constructHealthEventMessage(healthEvent)

	for _, msg := range messages {
		if fmt.Sprintf("%s;", msg) == newMessage {
			return messages
		}
	}

	return append(messages, newMessage[:len(newMessage)-1])
}

func (r *K8sConnector) removeImpactedEntitiesMessages(messages []string,
	entities []*protos.Entity) []string {
	var newMessages []string

	for _, msg := range messages {
		entityFound := false

		for _, entity := range entities {
			entityPrefix := fmt.Sprintf("%s:%s ", entity.EntityType, entity.EntityValue)

			if strings.Contains(msg, entityPrefix) {
				entityFound = true
				break
			}
		}

		if !entityFound {
			newMessages = append(newMessages, msg)
		}
	}

	return newMessages
}

func (r *K8sConnector) writeNodeEvent(ctx context.Context, event *corev1.Event, nodeName string) error {
	err := retry.OnError(retry.DefaultRetry, func(err error) bool {
		return apierrors.IsConflict(err) || isTemporaryError(err)
	}, func() error {
		// Fetch all events for the node
		events, err := r.clientset.CoreV1().Events(DefaultNamespace).List(ctx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("involvedObject.name=%s", nodeName),
		})
		if err != nil {
			return fmt.Errorf("failed to list events for node %s: %w", nodeName, err)
		}

		// Check if any event matches the new event

		for _, existingEvent := range events.Items {
			if existingEvent.Type == event.Type && existingEvent.Reason == event.Reason &&
				existingEvent.Message == event.Message {
				// Matching event found, update it
				existingEvent.Count++
				existingEvent.LastTimestamp = event.LastTimestamp

				_, err = r.clientset.CoreV1().Events(DefaultNamespace).Update(ctx, &existingEvent, metav1.UpdateOptions{})
				if err != nil {
					nodeEventOperationsCounter.WithLabelValues(nodeName, OperationUpdate, StatusFailed).Inc()
					return fmt.Errorf("failed to update event for node %s: %w", nodeName, err)
				} else {
					nodeEventOperationsCounter.WithLabelValues(nodeName, OperationUpdate, StatusSuccess).Inc()
				}

				return nil
			}
		}

		// No matching event found, create a new event with count 1
		event.Count = 1

		_, err = r.clientset.CoreV1().Events(DefaultNamespace).Create(ctx, event, metav1.CreateOptions{})
		if err != nil {
			nodeEventOperationsCounter.WithLabelValues(nodeName, OperationCreate, StatusFailed).Inc()
			return fmt.Errorf("failed to create event for node %s: %w", nodeName, err)
		} else {
			nodeEventOperationsCounter.WithLabelValues(nodeName, OperationCreate, StatusSuccess).Inc()
		}

		return nil
	})

	return err
}

func (r *K8sConnector) updateHealthEventReason(checkName string, isHealthy bool) string {
	status := "IsNotHealthy"
	if isHealthy {
		status = "IsHealthy"
	}

	return fmt.Sprintf("%s%s", checkName, status)
}

func (r *K8sConnector) fetchHealthEventMessage(healthEvent *protos.HealthEvent) string {
	message := ""

	if healthEvent.IsHealthy {
		message = NoHealthFailureMsg
	} else {
		message = r.constructHealthEventMessage(healthEvent)
	}

	return message
}

func (r *K8sConnector) constructHealthEventMessage(healthEvent *protos.HealthEvent) string {
	message := ""

	for _, errorCode := range healthEvent.ErrorCode {
		message += fmt.Sprintf("ErrorCode:%s ", errorCode)
	}

	for _, entity := range healthEvent.EntitiesImpacted {
		message += fmt.Sprintf("%s:%s ", entity.EntityType, entity.EntityValue)
	}

	if healthEvent.Message != "" {
		// Replace semicolons with dots in the message to prevent delimiter collision
		sanitizedMessage := strings.ReplaceAll(healthEvent.Message, ";", ".")
		message += fmt.Sprintf("%s ", sanitizedMessage)
	}

	message += fmt.Sprintf("Recommended Action=%s;", healthEvent.RecommendedAction.String())

	return message
}

// filterProcessableEvents filters out STORE_ONLY events that should not create node conditions or K8s events.
func filterProcessableEvents(healthEvents *protos.HealthEvents) []*protos.HealthEvent {
	var processableEvents []*protos.HealthEvent

	for _, healthEvent := range healthEvents.Events {
		if healthEvent.ProcessingStrategy == protos.ProcessingStrategy_STORE_ONLY {
			slog.Info("Skipping STORE_ONLY health event (no node conditions / node events)",
				"node", healthEvent.NodeName,
				"checkName", healthEvent.CheckName,
				"agent", healthEvent.Agent)

			continue
		}

		processableEvents = append(processableEvents, healthEvent)
	}

	return processableEvents
}

// createK8sEvent creates a Kubernetes event from a health event.
func (r *K8sConnector) createK8sEvent(healthEvent *protos.HealthEvent) *corev1.Event {
	ts := safeTimestamp(healthEvent.GeneratedTimestamp)

	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s.%x", healthEvent.NodeName, metav1.Now().UnixNano()),
			Namespace: DefaultNamespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Node",
			Name: healthEvent.NodeName,
			UID:  types.UID(healthEvent.NodeName),
		},
		Reason:              r.updateHealthEventReason(healthEvent.CheckName, healthEvent.IsHealthy),
		ReportingController: healthEvent.Agent,
		ReportingInstance:   healthEvent.NodeName,
		Message:             r.fetchHealthEventMessage(healthEvent),
		Count:               1,
		Source: corev1.EventSource{
			Component: healthEvent.Agent,
			Host:      healthEvent.NodeName,
		},
		FirstTimestamp: metav1.NewTime(ts),
		LastTimestamp:  metav1.NewTime(ts),
		Type:           healthEvent.CheckName,
	}
}

func (r *K8sConnector) processHealthEvents(ctx context.Context, healthEvents *protos.HealthEvents) error {
	processableEvents := filterProcessableEvents(healthEvents)

	eventsByNode := groupEventsByNode(processableEvents)

	var firstErr error

	for nodeName, nodeEvents := range eventsByNode {
		if err := r.processNodeConditionUpdates(ctx, nodeEvents); err != nil {
			if firstErr == nil {
				firstErr = err
			} else {
				slog.Error("Failed to process node condition updates", "node", nodeName, "error", err)
			}
		}
	}

	for _, healthEvent := range processableEvents {
		if !healthEvent.IsHealthy && !healthEvent.IsFatal {
			start := time.Now()
			err := r.writeNodeEvent(ctx, r.createK8sEvent(healthEvent), healthEvent.NodeName)

			nodeEventUpdateCreateDuration.Observe(float64(time.Since(start).Milliseconds()))

			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("failed to write node event for %s: %w", healthEvent.NodeName, err)
				} else {
					slog.Error("Failed to write node event", "node", healthEvent.NodeName, "error", err)
				}
			}
		}
	}

	return firstErr
}

func groupEventsByNode(events []*protos.HealthEvent) map[string][]*protos.HealthEvent {
	grouped := make(map[string][]*protos.HealthEvent)

	for _, e := range events {
		grouped[e.NodeName] = append(grouped[e.NodeName], e)
	}

	return grouped
}

func (r *K8sConnector) processNodeConditionUpdates(ctx context.Context,
	events []*protos.HealthEvent) error {
	start := time.Now()
	conditionsProcessed, err := r.updateNodeConditions(ctx, events)

	if !conditionsProcessed {
		return err
	}

	if err != nil {
		nodeConditionUpdateCounter.WithLabelValues(StatusFailed).Inc()

		return err
	}

	nodeConditionUpdateDuration.Observe(float64(time.Since(start).Milliseconds()))
	nodeConditionUpdateCounter.WithLabelValues(StatusSuccess).Inc()

	return nil
}

// isTemporaryError checks if the error is a temporary network error that should be retried
func isTemporaryError(err error) bool {
	if err == nil {
		return false
	}

	return isContextError(err) ||
		isKubernetesAPIError(err) ||
		isNetworkError(err) ||
		isSyscallError(err) ||
		isStringBasedError(err) ||
		errors.Is(err, io.EOF) ||
		strings.Contains(err.Error(), "EOF")
}

// isContextError checks if the error is a context-related error that should be retried
func isContextError(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// isKubernetesAPIError checks if the error is a Kubernetes API error that should be retried
func isKubernetesAPIError(err error) bool {
	return apierrors.IsTimeout(err) ||
		apierrors.IsServerTimeout(err) ||
		apierrors.IsServiceUnavailable(err) ||
		apierrors.IsTooManyRequests(err) ||
		apierrors.IsInternalError(err)
}

// isNetworkError checks if the error is a network-related error that should be retried
func isNetworkError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}

	return false
}

// isSyscallError checks if the error is a syscall error that should be retried
func isSyscallError(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.ETIMEDOUT) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EPIPE)
}

// isStringBasedError checks if the error message contains retryable error patterns
func isStringBasedError(err error) bool {
	errStr := err.Error()

	return isHTTPConnectionError(errStr) ||
		isTLSError(errStr) ||
		isDNSError(errStr) ||
		isLoadBalancerError(errStr) ||
		isKubernetesStringError(errStr)
}

// isHTTPConnectionError checks for HTTP/2 and HTTP connection error patterns
func isHTTPConnectionError(errStr string) bool {
	httpErrors := []string{
		"http2: client connection lost",
		"http2: server connection lost",
		"http2: connection closed",
		"connection reset by peer",
		"broken pipe",
		"connection refused",
		"connection timed out",
		"i/o timeout",
		"network is unreachable",
		"host is unreachable",
	}

	for _, pattern := range httpErrors {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}

	return false
}

// isTLSError checks for TLS/SSL handshake error patterns
func isTLSError(errStr string) bool {
	tlsErrors := []string{
		"tls: handshake timeout",
		"tls: oversized record received",
		"remote error: tls:",
	}

	for _, pattern := range tlsErrors {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}

	return false
}

// isDNSError checks for DNS resolution error patterns
func isDNSError(errStr string) bool {
	dnsErrors := []string{
		"no such host",
		"dns: no answer",
		"temporary failure in name resolution",
	}

	for _, pattern := range dnsErrors {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}

	return false
}

// isLoadBalancerError checks for load balancer and proxy error patterns
func isLoadBalancerError(errStr string) bool {
	lbErrors := []string{
		"502 Bad Gateway",
		"503 Service Unavailable",
		"504 Gateway Timeout",
	}

	for _, pattern := range lbErrors {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}

	return false
}

// isKubernetesStringError checks for Kubernetes-specific error patterns
func isKubernetesStringError(errStr string) bool {
	k8sErrors := []string{
		"the server is currently unable to handle the request",
		"etcd cluster is unavailable",
		"unable to connect to the server",
		"server is not ready",
	}

	for _, pattern := range k8sErrors {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}

	return false
}

// truncateNodeConditionMessage builds the node condition message while respecting the max node condition
// message length. It preserves complete error entries and adds a truncation indicator if needed.
func (r *K8sConnector) truncateNodeConditionMessage(messages []string) string {
	maxLen := int(r.maxNodeConditionMessageLength) - len(truncationSuffix)

	var result strings.Builder

	truncated := false

	for i, msg := range messages {
		var newEntry string
		if i == 0 {
			newEntry = msg
		} else {
			newEntry = ";" + msg
		}

		// Check if adding this entry would exceed the limit (+1 for trailing semicolon)
		if result.Len()+len(newEntry)+1 > maxLen {
			truncated = true
			break
		}

		result.WriteString(newEntry)
	}

	result.WriteString(";")

	if truncated {
		result.WriteString(truncationSuffix)
	}

	return result.String()
}
