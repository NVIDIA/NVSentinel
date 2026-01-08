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

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

const (
	DefaultNamespace   = "default"
	NoHealthFailureMsg = "No Health Failures"
	truncationSuffix   = "..."
)

//nolint:cyclop, gocognit
func (r *K8sConnector) updateNodeConditions(ctx context.Context, healthEvents []*protos.HealthEvent) error {
	sortedHealthEvents := slices.Clone(healthEvents)

	// sort in ascending order
	slices.SortFunc(sortedHealthEvents, func(a, b *protos.HealthEvent) int {
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

		timeA := ti.AsTime()
		timeB := tj.AsTime()

		if timeA.Before(timeB) {
			return -1
		} else if timeA.After(timeB) {
			return 1
		}

		return 0
	})

	conditionToHealthEventsMap := make(map[corev1.NodeConditionType][]*protos.HealthEvent)

	for _, event := range sortedHealthEvents {
		if !event.IsHealthy && !event.IsFatal {
			continue
		}

		conditionType := corev1.NodeConditionType(string(event.CheckName))
		conditionToHealthEventsMap[conditionType] = append(conditionToHealthEventsMap[conditionType], event)
	}

	err := retry.OnError(retry.DefaultRetry, func(err error) bool {
		return apierrors.IsConflict(err) || isTemporaryError(err)
	}, func() error {
		node, err := r.clientset.CoreV1().Nodes().Get(ctx, healthEvents[0].NodeName, metav1.GetOptions{})
		if err != nil {
			slog.Error("Error getting node", "error", err)
			return fmt.Errorf("failed to get node %s: %w", healthEvents[0].NodeName, err)
		}

		for conditionType, events := range conditionToHealthEventsMap {
			var matchedCondition *corev1.NodeCondition

			var conditionIndex int

			conditionExists := false

			// search for existing condition
			for i, c := range node.Status.Conditions {
				if c.Type == conditionType {
					matchedCondition = &c
					conditionIndex = i
					conditionExists = true

					break
				}
			}

			// Initialize condition if it doesn't exist
			if !conditionExists {
				matchedCondition = &corev1.NodeCondition{
					Type:               conditionType,
					LastHeartbeatTime:  metav1.NewTime(events[len(events)-1].GeneratedTimestamp.AsTime()),
					LastTransitionTime: metav1.NewTime(events[len(events)-1].GeneratedTimestamp.AsTime()),
				}
			}

			// split messages by ";" in condition
			messages := r.parseMessages(matchedCondition.Message)

			// aggregate messages from all health events for the associated condition
			for _, event := range events {
				if !event.IsHealthy {
					// add the new message if it doesn't exist
					messages = r.addMessageIfNotExist(messages, event)
				} else {
					// remove messages that include any of the entities in entitiesImpacted, else if
					// empty then clear all the messages for all entities
					if len(event.EntitiesImpacted) > 0 {
						messages = r.removeImpactedEntitiesMessages(messages, event.EntitiesImpacted)
					} else {
						messages = []string{}
					}
				}
			}

			if len(messages) > 0 {
				matchedCondition.Message = r.truncateNodeConditionMessage(messages)
				matchedCondition.Status = corev1.ConditionTrue
				matchedCondition.Reason = r.updateHealthEventReason(events[len(events)-1].CheckName, false)
			} else {
				matchedCondition.Message = NoHealthFailureMsg
				matchedCondition.Status = corev1.ConditionFalse
				matchedCondition.Reason = r.updateHealthEventReason(events[len(events)-1].CheckName, true)
			}

			matchedCondition.LastHeartbeatTime = metav1.NewTime(events[len(events)-1].GeneratedTimestamp.AsTime())

			// update transition time if status has changed
			if conditionExists && matchedCondition.Status != node.Status.Conditions[conditionIndex].Status {
				matchedCondition.LastTransitionTime = matchedCondition.LastHeartbeatTime
			}

			// updates to the node conditions
			if conditionExists {
				node.Status.Conditions[conditionIndex] = *matchedCondition
			} else {
				node.Status.Conditions = append(node.Status.Conditions, *matchedCondition)
			}
		}

		_, err = r.clientset.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
		if err != nil {
			for conditionType := range conditionToHealthEventsMap {
				slog.Info("Node condition update failed", "conditionType", conditionType, "error", err)
			}

			return fmt.Errorf("failed to update node %s status: %w", node.Name, err)
		}

		return nil
	})

	return err
}

func (r *K8sConnector) parseMessages(message string) []string {
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

func (r *K8sConnector) processHealthEvents(ctx context.Context, healthEvents *protos.HealthEvents) error {
	var nodeConditions []corev1.NodeCondition

	for _, healthEvent := range healthEvents.Events {
		conditionType := corev1.NodeConditionType(string(healthEvent.CheckName))
		message := r.fetchHealthEventMessage(healthEvent)

		if healthEvent.IsHealthy || healthEvent.IsFatal {
			newCondition := corev1.NodeCondition{
				Type:               conditionType,
				LastHeartbeatTime:  metav1.NewTime(healthEvent.GeneratedTimestamp.AsTime()),
				LastTransitionTime: metav1.NewTime(healthEvent.GeneratedTimestamp.AsTime()),
				Message:            message,
			}

			nodeConditions = append(nodeConditions, newCondition)
		}
	}

	if len(nodeConditions) > 0 {
		start := time.Now()
		err := r.updateNodeConditions(ctx, healthEvents.Events)

		duration := float64(time.Since(start).Milliseconds())
		nodeConditionUpdateDuration.Observe(duration)

		if err != nil {
			nodeConditionUpdateCounter.WithLabelValues(StatusFailed).Inc()
			return fmt.Errorf("failed to update node conditions: %w", err)
		}

		nodeConditionUpdateCounter.WithLabelValues(StatusSuccess).Inc()
	}

	for _, healthEvent := range healthEvents.Events {
		if !healthEvent.IsHealthy && !healthEvent.IsFatal {
			event := &corev1.Event{
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
				FirstTimestamp: metav1.NewTime(healthEvent.GeneratedTimestamp.AsTime()),
				LastTimestamp:  metav1.NewTime(healthEvent.GeneratedTimestamp.AsTime()),
				Type:           healthEvent.CheckName,
			}
			start := time.Now()

			err := r.writeNodeEvent(ctx, event, healthEvent.NodeName)
			duration := float64(time.Since(start).Milliseconds())
			nodeEventUpdateCreateDuration.Observe(duration)

			if err != nil {
				return fmt.Errorf("failed to write node event for %s: %w", healthEvent.NodeName, err)
			}
		}
	}

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
