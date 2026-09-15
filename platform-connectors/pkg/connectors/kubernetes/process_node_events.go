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
	"hash/fnv"
	"io"
	"log/slog"
	"net"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	DefaultNamespace        = "default"
	NoHealthFailureMsg      = "No Health Failures"
	truncationSuffix        = "..."
	recommendedActionMarker = "Recommended Action="
)

// updateNodeConditions updates node conditions for a single node.
// All healthEvents must belong to the same node; callers must partition by NodeName.
func (r *K8sConnector) updateNodeConditions(ctx context.Context, healthEvents []*protos.HealthEvent) (bool, error) {
	nodeName := ""
	if len(healthEvents) > 0 && healthEvents[0] != nil {
		nodeName = healthEvents[0].NodeName
	}

	sortedHealthEvents := sortHealthEventsByTimestamp(healthEvents)
	conditionEventsMap := buildConditionEventsMap(sortedHealthEvents)

	if len(conditionEventsMap) == 0 {
		return false, nil
	}

	ctx, span := tracing.StartSpan(ctx, "platform_connector.k8s.update_node_condition")
	defer span.End()

	span.SetAttributes(
		attribute.Int("platform_connector.k8s.node_condition_update_count", len(conditionEventsMap)),
	)

	skipped := false

	err := retry.OnError(retry.DefaultRetry, func(err error) bool {
		isRetriable := apierrors.IsConflict(err) || isTemporaryError(err)
		if isRetriable {
			span.AddEvent("platform_connector.k8s.update_node_conditions_failed",
				trace.WithAttributes(
					attribute.String("platform_connector.k8s.error.type", "update_node_conditions_failed"),
					attribute.String("platform_connector.k8s.error.message", err.Error()),
				),
			)
		}

		return isRetriable
	}, func() error {
		var err error

		skipped, err = r.readAndUpdateNode(ctx, nodeName, conditionEventsMap)

		return err
	})

	if skipped {
		nodeConditionUpdateCounter.WithLabelValues(StatusSkipped).Inc()
		slog.DebugContext(ctx, "Node conditions unchanged by batch, skipping update", "node", nodeName)

		return false, nil
	}

	if err != nil {
		conditionTypes := make([]string, 0, len(conditionEventsMap))
		for ct := range conditionEventsMap {
			conditionTypes = append(conditionTypes, string(ct))
		}

		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("platform_connector.k8s.error.type", "update_node_conditions_failed"),
			attribute.String("platform_connector.k8s.error.message", err.Error()),
		)

		slog.ErrorContext(ctx, "Failed to update node conditions",
			"node", nodeName,
			"conditionTypes", conditionTypes,
			"error", err)

		return true, fmt.Errorf("failed to update node %s conditions: %w", nodeName, err)
	}

	return true, nil
}

// readAndUpdateNode is one attempt of updateNodeConditions: read the node,
// fold the events into its conditions and write the status back. It reports
// skipped=true when the batch would leave the node showing what it already
// shows and UpdateOnlyOnChange is set: a repeat from a monitor that reports
// every cycle, or a resent batch, then costs no update call.
func (r *K8sConnector) readAndUpdateNode(
	ctx context.Context, nodeName string,
	conditionEventsMap map[corev1.NodeConditionType][]*protos.HealthEvent,
) (bool, error) {
	node, err := r.clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}

	changed := false

	for conditionType, events := range conditionEventsMap {
		if r.processNodeCondition(ctx, node, conditionType, events) {
			changed = true
		}
	}

	if r.config.UpdateOnlyOnChange && !changed {
		return true, nil
	}

	_, err = r.clientset.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})

	return false, err
}

func sortHealthEventsByTimestamp(events []*protos.HealthEvent) []*protos.HealthEvent {
	sorted := slices.Clone(events)

	// Stable, so events with equal timestamps keep their wire order and every
	// path that sorts a batch or a subset of it agrees.
	slices.SortStableFunc(sorted, func(a, b *protos.HealthEvent) int {
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

// processNodeCondition folds events into the node's condition of the given
// type, in place. It reports whether the condition's status, reason or message
// changed (a new condition counts as a change); a refreshed heartbeat alone
// does not.
func (r *K8sConnector) processNodeCondition(
	ctx context.Context, node *corev1.Node,
	conditionType corev1.NodeConditionType, events []*protos.HealthEvent,
) bool {
	if len(events) == 0 {
		return false
	}

	latestEvent := events[len(events)-1]
	latestTime := metav1.NewTime(safeTimestamp(ctx, latestEvent.GeneratedTimestamp))

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
		message := r.truncateNodeConditionMessage(messages)
		matchedCondition.Message = message

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

	if !conditionExists {
		node.Status.Conditions = append(node.Status.Conditions, matchedCondition)

		return true
	}

	previous := node.Status.Conditions[conditionIndex]
	node.Status.Conditions[conditionIndex] = matchedCondition

	return previous.Status != matchedCondition.Status ||
		previous.Reason != matchedCondition.Reason ||
		previous.Message != matchedCondition.Message
}

func safeTimestamp(ctx context.Context, ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		slog.WarnContext(ctx, "HealthEvent has nil GeneratedTimestamp, falling back to current time")

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
// Healthy events with entities forward ErrorCode to the scoped clearer; an
// empty ErrorCode preserves the legacy "clear everything on the entity" path.
func (r *K8sConnector) aggregateEventMessages(messages []string, events []*protos.HealthEvent) []string {
	for _, event := range events {
		switch {
		case !event.IsHealthy:
			messages = r.addMessageIfNotExist(messages, event)
		case len(event.EntitiesImpacted) > 0:
			messages = r.removeImpactedEntitiesMessagesScoped(messages, recoveryEntities(event), event.ErrorCode)
		default: // healthy event with no impacted entities — full recovery, clear all messages
			messages = []string{}
		}
	}

	return messages
}

// recoveryEntities drops the physical GPU UUID from recovery identity when a
// stable GPU slot identifier is available. A replacement GPU keeps its logical
// index or PCI address but receives a new UUID, so requiring the UUID would
// leave the historical node condition active after the hardware is healthy.
func recoveryEntities(event *protos.HealthEvent) []*protos.Entity {
	if !strings.EqualFold(event.ComponentClass, "GPU") {
		return event.EntitiesImpacted
	}

	hasStableGPUIdentity := false
	entities := make([]*protos.Entity, 0, len(event.EntitiesImpacted))

	for _, entity := range event.EntitiesImpacted {
		if strings.EqualFold(entity.EntityType, "GPU") || strings.EqualFold(entity.EntityType, "PCI") {
			hasStableGPUIdentity = true
		}

		if !strings.EqualFold(entity.EntityType, "GPU_UUID") {
			entities = append(entities, entity)
		}
	}

	if !hasStableGPUIdentity {
		return event.EntitiesImpacted
	}

	return entities
}

func parseMessages(message string) []string {
	var messages []string

	if message == "" {
		return nil
	}

	for msg := range strings.SplitSeq(message, ";") {
		if msg == "" || msg == truncationSuffix || isNoHealthFailureMessage(msg) {
			continue
		}

		messages = append(messages, msg)
	}

	return messages
}

// isNoHealthFailureMessage reports whether msg is the recovery sentinel in any
// form. The comparison is case-insensitive and tolerates surrounding whitespace
// so a non-canonical recovery message (different casing, a trailing separator, or
// one written by an external tool or an older build) is treated as "no fault"
// instead of being resurrected as a phantom fault that wedges the node condition
// at Status=True.
func isNoHealthFailureMessage(msg string) bool {
	return strings.EqualFold(strings.TrimSpace(msg), NoHealthFailureMsg)
}

func (r *K8sConnector) addMessageIfNotExist(messages []string, healthEvent *protos.HealthEvent) []string {
	newMessage := r.constructHealthEventMessage(healthEvent)

	for _, msg := range messages {
		if fmt.Sprintf("%s;", msg) == newMessage {
			return messages
		}

		// Updating on change only: an entry naming the same fault (error
		// codes, entity, action) is that fault, whatever its text. Compaction
		// at the length cap rewrites the text, so without this a saturated
		// message would gain the fault again, move it, and count as a change
		// on every repeat.
		if r.config.UpdateOnlyOnChange && messagesMatchByIdentity(msg, newMessage[:len(newMessage)-1]) {
			return messages
		}
	}

	return append(messages, newMessage[:len(newMessage)-1])
}

// extractMessageIdentity parses ErrorCodes, entity tokens (GPU, PCI, GPU_UUID),
// and Recommended Action from a node condition message. Works on both full and
// compacted messages.
func extractMessageIdentity(msg string) (errorCodes []string, entities []string, recommendedAction string) {
	raIdx := strings.LastIndex(msg, recommendedActionMarker)
	if raIdx >= 0 {
		recommendedAction = strings.TrimRight(msg[raIdx:], " ")
	}

	prefix := msg
	if raIdx >= 0 {
		prefix = msg[:raIdx]
	}

	for token := range strings.FieldsSeq(prefix) {
		switch {
		case strings.HasPrefix(token, "ErrorCode:"):
			errorCodes = append(errorCodes, token)
		case strings.HasPrefix(token, "GPU:") ||
			strings.HasPrefix(token, "PCI:"):
			entities = append(entities, token)
		}
	}

	return errorCodes, entities, recommendedAction
}

// messagesMatchByIdentity returns true if two messages represent the same fault:
// same ErrorCodes, same Recommended Action, and at least one shared entity
// (GPU, PCI, or GPU_UUID). Entity any-match handles the case where GPU_UUID is
// truncated by compaction but GPU or PCI identifiers still match.
func messagesMatchByIdentity(a, b string) bool {
	aErr, aEnt, aRA := extractMessageIdentity(a)
	bErr, bEnt, bRA := extractMessageIdentity(b)

	if aRA != bRA || !slices.Equal(aErr, bErr) {
		return false
	}

	for _, ae := range aEnt {
		if slices.Contains(bEnt, ae) {
			return true
		}
	}

	return false
}

// deduplicateMessagesByIdentity removes identity-duplicate messages, keeping the
// last (freshest) occurrence. This is only called when total message length
// exceeds the node condition limit, to reclaim space before compaction.
func deduplicateMessagesByIdentity(messages []string) []string {
	var result []string

	for i, msg := range messages {
		duplicate := false

		for j := i + 1; j < len(messages); j++ {
			if messagesMatchByIdentity(msg, messages[j]) {
				duplicate = true

				break
			}
		}

		if !duplicate {
			result = append(result, msg)
		}
	}

	return result
}

// entityMatchesMessage reports whether msg contains the entity token, handling
// both exact matches and truncated tokens. Prefix matching only activates when
// there is evidence of truncation (token ends with "..." or the message was
// byte-truncated before the Recommended Action marker) to prevent false
// positives against complete but shorter entity values.
func entityMatchesMessage(msg string, entity *protos.Entity) bool {
	fullToken := fmt.Sprintf("%s:%s ", entity.EntityType, entity.EntityValue)
	if strings.Contains(msg, fullToken) {
		return true
	}

	prefix := msg

	hasRecommendedAction := false
	if raIdx := strings.LastIndex(msg, recommendedActionMarker); raIdx >= 0 {
		hasRecommendedAction = true
		prefix = msg[:raIdx]
	}

	entityToken := fmt.Sprintf("%s:%s", entity.EntityType, entity.EntityValue)
	typePrefix := entity.EntityType + ":"

	tokens := strings.Fields(prefix)

	for i, tok := range tokens {
		if !strings.HasPrefix(tok, typePrefix) {
			continue
		}

		isTruncated := strings.HasSuffix(tok, truncationSuffix) || (!hasRecommendedAction && i == len(tokens)-1)

		candidate := strings.TrimSuffix(tok, truncationSuffix)

		if isTruncated && len(candidate) > len(typePrefix) && strings.HasPrefix(entityToken, candidate) {
			return true
		}
	}

	return false
}

func entitiesMatchMessage(msg string, entities []*protos.Entity) bool {
	if len(entities) == 0 {
		return false
	}

	for _, entity := range entities {
		if !entityMatchesMessage(msg, entity) {
			return false
		}
	}

	return true
}

// removeImpactedEntitiesMessagesScoped removes messages that mention all
// supplied entities. When errorCodes is non-empty, the message must also carry
// a matching ErrorCode token; this prevents an "X cancels Y" rule from
// clearing an unrelated fault Z on the same entity.
func (r *K8sConnector) removeImpactedEntitiesMessagesScoped(
	messages []string,
	entities []*protos.Entity,
	errorCodes []string,
) []string {
	var newMessages []string

	for _, msg := range messages {
		entityFound := entitiesMatchMessage(msg, entities)

		if entityFound && !messageMatchesAnyErrorCode(msg, errorCodes) {
			entityFound = false
		}

		if !entityFound {
			newMessages = append(newMessages, msg)
		}
	}

	return newMessages
}

// messageMatchesAnyErrorCode reports whether msg carries one of errorCodes.
// An empty errorCodes slice matches every message.
func messageMatchesAnyErrorCode(msg string, errorCodes []string) bool {
	if len(errorCodes) == 0 {
		return true
	}

	for _, code := range errorCodes {
		token := fmt.Sprintf("ErrorCode:%s ", code)
		if strings.Contains(msg, token) {
			return true
		}
	}

	return false
}

// maxRememberedNodeChecks bounds the Event memory of the node-local
// connector, which serves one node; the deployment platform connector sizes
// it for the fleet (K8sConnectorConfig.NodeEventMemorySize). Overflow evicts
// only the least-recently-used entry, which costs one extra Event write.
const maxRememberedNodeChecks = 1024

// nodeEventRefreshInterval is how long UpdateOnlyOnChange skips repeats of a
// fault whose Event is already written. Once it has passed, the next repeat
// refreshes the Event (count and timestamp), so a fault that lasts stays in
// the Event list, which drops Events an hour after their last write. It also
// bounds how long a replica can miss a recurrence when the recovery in
// between was reported to another replica.
const nodeEventRefreshInterval = 10 * time.Minute

// maxRememberedMessagesPerCheck bounds the Events remembered for one check
// on one node. A check reports a handful of distinct faults at a time, and
// the message is producer-controlled, so the map must not grow with it; past
// the bound the check's memory is dropped and its faults are announced again,
// which costs one extra Event write each.
const maxRememberedMessagesPerCheck = 32

// rememberedEvent is the Kubernetes Event last written for one fault, with
// the entities it named so a recovery of those entities can forget it. The
// Event's name is not kept: it is derived from the fault (nodeEventName), so
// the Event being written carries it.
type rememberedEvent struct {
	entities  []string
	writtenAt time.Time
}

// nodeCheckKey identifies one check on one node in the Event memory. Callers
// pass the Event's Type field, which carries the check name (pre-existing
// behaviour of createK8sEvent), not the Kubernetes Normal/Warning type.
func nodeCheckKey(nodeName, checkName string) string {
	return nodeName + "\x00" + checkName
}

// entityKeys names the entities a health event reports on, as type:value.
func entityKeys(healthEvent *protos.HealthEvent) []string {
	keys := make([]string, 0, len(healthEvent.EntitiesImpacted))
	for _, entity := range healthEvent.EntitiesImpacted {
		keys = append(keys, entity.EntityType+":"+entity.EntityValue)
	}

	return keys
}

// sharesEntity reports whether the two entity lists have an entity in common.
func sharesEntity(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}

	return false
}

// nodeEventMemory returns the memory, initialized lazily so struct-literal
// construction works. Callers hold nodeEventMu.
func (r *K8sConnector) nodeEventMemory() *expirable.LRU[string, map[string]rememberedEvent] {
	if r.nodeEvents == nil {
		size := r.config.NodeEventMemorySize
		if size <= 0 {
			size = maxRememberedNodeChecks
		}

		r.nodeEvents = expirable.NewLRU[string, map[string]rememberedEvent](size, nil, 0)
	}

	return r.nodeEvents
}

// rememberedNodeEvent returns the Event last written for this fault, if any.
func (r *K8sConnector) rememberedNodeEvent(nodeName string, event *corev1.Event) (rememberedEvent, bool) {
	r.nodeEventMu.Lock()
	defer r.nodeEventMu.Unlock()

	written, ok := r.nodeEventMemory().Get(nodeCheckKey(nodeName, event.Type))
	if !ok {
		return rememberedEvent{}, false
	}

	remembered, ok := written[event.Message]

	return remembered, ok
}

// rememberNodeEvent records that this fault's Event, named after the fault,
// was just written.
func (r *K8sConnector) rememberNodeEvent(nodeName string, event *corev1.Event, entities []string) {
	r.nodeEventMu.Lock()
	defer r.nodeEventMu.Unlock()

	key := nodeCheckKey(nodeName, event.Type)

	written, ok := r.nodeEventMemory().Get(key)
	if !ok {
		written = map[string]rememberedEvent{}
		r.nodeEventMemory().Add(key, written)
	}

	// A refresh of a remembered message must not cost the other messages
	// their memory; only a new message arriving at capacity starts over.
	if _, exists := written[event.Message]; !exists && len(written) >= maxRememberedMessagesPerCheck {
		clear(written)
	}

	written[event.Message] = rememberedEvent{entities: entities, writtenAt: time.Now()}
}

// forgetNodeEvent drops the memory of one fault's Event because the Event no
// longer exists.
func (r *K8sConnector) forgetNodeEvent(nodeName string, event *corev1.Event) {
	r.nodeEventMu.Lock()
	defer r.nodeEventMu.Unlock()

	key := nodeCheckKey(nodeName, event.Type)

	if written, ok := r.nodeEventMemory().Get(key); ok {
		delete(written, event.Message)

		if len(written) == 0 {
			r.nodeEventMemory().Remove(key)
		}
	}
}

// forgetNodeCheck drops the memory of the Events written for a check on a
// node: those naming one of the recovered entities, or all of them when the
// healthy report names no entity. Called on a healthy report with
// UpdateOnlyOnChange, so the fault's next occurrence is announced again
// instead of skipped as a repeat.
func (r *K8sConnector) forgetNodeCheck(nodeName, checkName string, recovered []string) {
	r.nodeEventMu.Lock()
	defer r.nodeEventMu.Unlock()

	key := nodeCheckKey(nodeName, checkName)

	if len(recovered) == 0 {
		r.nodeEventMemory().Remove(key)

		return
	}

	written, ok := r.nodeEventMemory().Get(key)
	if !ok {
		return
	}

	for message, remembered := range written {
		if sharesEntity(remembered.entities, recovered) {
			delete(written, message)
		}
	}

	if len(written) == 0 {
		r.nodeEventMemory().Remove(key)
	}
}

// refreshNodeEvent bumps the count and timestamp of the Event written for this
// fault before, which carries the same derived name as event. It returns
// (true, nil) on success, (false, nil) when the Event is gone and a fresh one
// should be created, and (true, error) for other lookup or update failures.
func (r *K8sConnector) refreshNodeEvent(
	ctx context.Context, span trace.Span, remembered rememberedEvent, event *corev1.Event, nodeName string,
) (bool, error) {
	name := event.Name

	existingEvent, getErr := r.clientset.CoreV1().Events(DefaultNamespace).Get(ctx, name, metav1.GetOptions{})

	switch {
	case getErr == nil:
		existingEvent.Count++
		existingEvent.LastTimestamp = event.LastTimestamp

		_, err := r.clientset.CoreV1().Events(DefaultNamespace).Update(ctx, existingEvent, metav1.UpdateOptions{})

		switch {
		case err == nil:
			r.rememberNodeEvent(nodeName, event, remembered.entities)
			nodeEventOperationsCounter.WithLabelValues(OperationUpdate, StatusSuccess).Inc()

			return true, nil
		case apierrors.IsNotFound(err):
			// Deleted between lookup and update: fall through and create a fresh event.
		default:
			nodeEventOperationsCounter.WithLabelValues(OperationUpdate, StatusFailed).Inc()
			span.AddEvent("platform_connector.k8s.node_event_update_failed", trace.WithAttributes(
				attribute.String("platform_connector.k8s.error.type", "node_event_update_failed"),
				attribute.String("platform_connector.k8s.error.message", err.Error()),
			))

			return true, fmt.Errorf("failed to update event for node %s: %w", nodeName, err)
		}
	case apierrors.IsNotFound(getErr):
		// The Event expired or was deleted: fall through and create a fresh event.
	default:
		return true, fmt.Errorf("failed to look up event %s for node %s: %w", name, nodeName, getErr)
	}

	// The Event is gone; forget it so the next report creates a fresh one.
	r.forgetNodeEvent(nodeName, event)

	return false, nil
}

// createOrRefreshNodeEvent creates the fault's Event. When it already exists,
// written by another replica or by an earlier life of this one, it refreshes
// that Event instead of writing a second one. An Event that disappears
// between the create and the refresh is not chased: the next report of the
// fault creates it again.
func (r *K8sConnector) createOrRefreshNodeEvent(
	ctx context.Context, span trace.Span, event *corev1.Event, nodeName string, entities []string,
) error {
	_, err := r.clientset.CoreV1().Events(DefaultNamespace).Create(ctx, event, metav1.CreateOptions{})

	switch {
	case err == nil:
		r.rememberNodeEvent(nodeName, event, entities)
		nodeEventOperationsCounter.WithLabelValues(OperationCreate, StatusSuccess).Inc()

		return nil
	case apierrors.IsAlreadyExists(err):
		found := rememberedEvent{entities: entities}

		refreshed, refreshErr := r.refreshNodeEvent(ctx, span, found, event, nodeName)
		if refreshErr == nil && !refreshed {
			// Gone between the create and the refresh: not chased, the next
			// report creates it again, but the write did not happen.
			nodeEventOperationsCounter.WithLabelValues(OperationCreate, StatusSkipped).Inc()
		}

		return refreshErr
	default:
		nodeEventOperationsCounter.WithLabelValues(OperationCreate, StatusFailed).Inc()

		return fmt.Errorf("failed to create event for node %s: %w", nodeName, err)
	}
}

// writeNodeEvent announces a non-fatal fault as a Kubernetes Event. The first
// write of a fault creates the Event; a repeat refreshes it by its derived
// name, a single-key GET, because an involvedObject LIST is an
// unindexed full-range etcd scan. The name is derived from the fault, so a
// replica with no memory of it (another replica wrote it, or this one
// restarted or evicted the entry) learns from the create's AlreadyExists
// answer and refreshes that Event instead of writing a second one. With
// UpdateOnlyOnChange a repeat inside nodeEventRefreshInterval is skipped
// without any API call.
func (r *K8sConnector) writeNodeEvent(ctx context.Context, healthEvent *protos.HealthEvent) (bool, error) {
	ctx, span := tracing.StartSpan(ctx, "platform_connector.k8s.update_node_event")
	defer span.End()

	nodeName := healthEvent.NodeName
	event := r.createK8sEvent(ctx, healthEvent)

	span.SetAttributes(
		attribute.String("platform_connector.k8s.event_reason", event.Reason),
		attribute.String("platform_connector.k8s.event_type", string(event.Type)),
	)

	remembered, known := r.rememberedNodeEvent(nodeName, event)
	if known && r.config.UpdateOnlyOnChange && time.Since(remembered.writtenAt) < nodeEventRefreshInterval {
		nodeEventOperationsCounter.WithLabelValues(OperationUpdate, StatusSkipped).Inc()

		return true, nil
	}

	entities := entityKeys(healthEvent)

	err := retry.OnError(retry.DefaultRetry, func(err error) bool {
		return apierrors.IsConflict(err) || isTemporaryError(err)
	}, func() error {
		if known {
			refreshed, err := r.refreshNodeEvent(ctx, span, remembered, event, nodeName)
			if refreshed {
				return err
			}

			// Gone: a retry goes straight to the create.
			known = false
		}

		return r.createOrRefreshNodeEvent(ctx, span, event, nodeName, entities)
	})
	if err != nil {
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("platform_connector.k8s.error.type", "write_node_event_failed"),
			attribute.String("platform_connector.k8s.error.message", err.Error()),
		)
	}

	return false, err
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
	var message strings.Builder

	for _, errorCode := range healthEvent.ErrorCode {
		fmt.Fprintf(&message, "ErrorCode:%s ", errorCode)
	}

	for _, entity := range healthEvent.EntitiesImpacted {
		fmt.Fprintf(&message, "%s:%s ", entity.EntityType, entity.EntityValue)
	}

	if healthEvent.Message != "" {
		// Replace semicolons with dots in the message to prevent delimiter collision
		sanitizedMessage := strings.ReplaceAll(healthEvent.Message, ";", ".")
		fmt.Fprintf(&message, "%s ", sanitizedMessage)
	}

	fmt.Fprintf(&message, "Recommended Action=%s;", healthEvent.RecommendedAction.String())

	return message.String()
}

// filterProcessableEvents filters out events that should not create node conditions or K8s events.
func filterProcessableEvents(ctx context.Context, healthEvents *protos.HealthEvents) []*protos.HealthEvent {
	var processableEvents []*protos.HealthEvent

	for _, healthEvent := range healthEvents.Events {
		if healthEvent.ProcessingStrategy == protos.ProcessingStrategy_STORE_ONLY ||
			healthEvent.ProcessingStrategy == protos.ProcessingStrategy_STORE_AND_ANALYSE {
			slog.InfoContext(ctx, "Skipping non-remediation health event (no node conditions / node events)",
				"node", healthEvent.NodeName,
				"checkName", healthEvent.CheckName,
				"agent", healthEvent.Agent,
				"processingStrategy", healthEvent.ProcessingStrategy.String())

			continue
		}

		processableEvents = append(processableEvents, healthEvent)
	}

	return processableEvents
}

// nodeEventName derives the Event name from the fault it announces (node,
// check, reason and message), so every replica, and every life of one, names
// the same fault the same way and finds the Event another one wrote instead of
// writing a second one.
func nodeEventName(nodeName, checkName, reason, message string) string {
	h := fnv.New64a()

	for _, part := range []string{nodeName, checkName, reason, message} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}

	return fmt.Sprintf("%s.%016x", nodeName, h.Sum64())
}

// createK8sEvent creates a Kubernetes event from a health event.
func (r *K8sConnector) createK8sEvent(ctx context.Context, healthEvent *protos.HealthEvent) *corev1.Event {
	ts := safeTimestamp(ctx, healthEvent.GeneratedTimestamp)
	reason := r.updateHealthEventReason(healthEvent.CheckName, healthEvent.IsHealthy)
	message := r.fetchHealthEventMessage(healthEvent)

	return &corev1.Event{
		Name:      nodeEventName(healthEvent.NodeName, healthEvent.CheckName, reason, message),
		Namespace: DefaultNamespace,
		InvolvedObject: corev1.ObjectReference{
			Kind: "Node",
			Name: healthEvent.NodeName,
			UID:  types.UID(healthEvent.NodeName),
		},
		Reason:              reason,
		ReportingController: healthEvent.Agent,
		ReportingInstance:   healthEvent.NodeName,
		Message:             message,
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
	ctx, span := tracing.StartSpan(ctx, "platform_connector.k8s.process_health_events")
	defer span.End()

	// One order for the whole batch: the condition and Event paths both see
	// a recovery and a later return of the same fault in that order.
	processableEvents := sortHealthEventsByTimestamp(filterProcessableEvents(ctx, healthEvents))

	span.SetAttributes(
		attribute.Int("platform_connector.k8s.processable_events", len(processableEvents)),
	)

	eventsByNode := groupEventsByNode(processableEvents)

	var firstErr error

	for _, nodeEvents := range eventsByNode {
		if err := r.processNodeConditionUpdates(ctx, nodeEvents); err != nil {
			if firstErr == nil {
				firstErr = err
			}

			span.AddEvent("platform_connector.k8s.node_condition_update_error", trace.WithAttributes(
				attribute.String("platform_connector.k8s.error.type", "node_condition_update_error"),
				attribute.String("platform_connector.k8s.error.message", err.Error()),
			))
		}
	}

	if err := r.writeNodeEvents(ctx, span, processableEvents); err != nil && firstErr == nil {
		firstErr = err
	}

	return firstErr
}

// writeNodeEvents writes the Kubernetes Events of a batch, which
// processHealthEvents hands over in timestamp order like the condition path,
// so a recovery and a later return of the same fault in one batch are seen in
// that order. It returns the first write error.
func (r *K8sConnector) writeNodeEvents(ctx context.Context, span trace.Span, events []*protos.HealthEvent) error {
	var firstErr error

	for _, healthEvent := range events {
		if healthEvent.IsHealthy {
			// The check recovered for these entities: their next fault is a
			// change again, not a repeat of an Event already written.
			if r.config.UpdateOnlyOnChange {
				r.forgetNodeCheck(healthEvent.NodeName, healthEvent.CheckName, entityKeys(healthEvent))
			}

			continue
		}

		if healthEvent.IsFatal {
			continue
		}

		start := time.Now()
		skipped, err := r.writeNodeEvent(ctx, healthEvent)

		if !skipped {
			nodeEventUpdateCreateDuration.Observe(float64(time.Since(start).Milliseconds()))
		}

		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to write node event for %s: %w", healthEvent.NodeName, err)
			}

			span.AddEvent("platform_connector.k8s.node_event_write_failed", trace.WithAttributes(
				attribute.String("platform_connector.k8s.error.type", "node_event_write_failed"),
				attribute.String("platform_connector.k8s.error.message", err.Error()),
			))
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
	if netErr, ok := errors.AsType[net.Error](err); ok {
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

// totalMessageLength returns the byte length of messages joined with ";" separators plus a trailing ";".
func totalMessageLength(messages []string) int {
	total := 0

	for i, msg := range messages {
		if i > 0 {
			total++
		}

		total += len(msg)
	}

	total++ // trailing ";"

	return total
}

// isStructuredEntityToken reports whether a whitespace-delimited token matches
// the TYPE:VALUE pattern produced by constructHealthEventMessage.
func isStructuredEntityToken(token string) bool {
	if strings.HasPrefix(token, "ErrorCode:") {
		return false
	}

	colonIdx := strings.Index(token, ":")

	return colonIdx > 0 && colonIdx < len(token)-1
}

func splitIdentityAndDiagnostic(beforeRA string) (string, string) {
	tokens := strings.Fields(beforeRA)

	var identityEnd int

	for i, tok := range tokens {
		if strings.HasPrefix(tok, "ErrorCode:") || isStructuredEntityToken(tok) {
			identityEnd = i + 1
		} else {
			break
		}
	}

	return strings.Join(tokens[:identityEnd], " "), strings.Join(tokens[identityEnd:], " ")
}

func truncateWithSuffix(text string, maxLen int) string {
	if maxLen <= len(truncationSuffix) {
		return truncationSuffix
	}

	if len(text) > maxLen-len(truncationSuffix) {
		return text[:maxLen-len(truncationSuffix)] + truncationSuffix
	}

	return text
}

func compactMessagePrefix(identityPrefix, diagnosticText string, maxLen int) string {
	if identityPrefix == "" {
		return truncateWithSuffix(diagnosticText, maxLen)
	}

	if len(identityPrefix) >= maxLen {
		return identityPrefix + " " + truncationSuffix
	}

	available := maxLen - len(identityPrefix) - 1 // -1 for space between identity and diagnostic
	if available <= 0 || diagnosticText == "" {
		return identityPrefix + " " + truncationSuffix
	}

	return identityPrefix + " " + truncateWithSuffix(diagnosticText, available)
}

// compactMessageField truncates the diagnostic free-text while preserving
// identity tokens (ErrorCode and entity tokens) and the Recommended Action
// suffix needed for recovery matching. Identity tokens are never byte-truncated;
// maxLen is treated as a target for diagnostic text compaction only.
//
// Input format:  "ErrorCode:X GPU:3 PCI:addr <diagnostic text> Recommended Action=Y"
// Output format: "ErrorCode:X GPU:3 PCI:addr <truncated>... Recommended Action=Y"
func compactMessageField(msg string, maxLen int) string {
	raIdx := strings.LastIndex(msg, recommendedActionMarker)
	if raIdx < 0 {
		return msg
	}

	beforeRA := strings.TrimRight(msg[:raIdx], " ")
	raPart := msg[raIdx:]

	if len(beforeRA) <= maxLen {
		return msg
	}

	identityPrefix, diagnosticText := splitIdentityAndDiagnostic(beforeRA)

	return compactMessagePrefix(identityPrefix, diagnosticText, maxLen) + " " + raPart
}

// truncateNodeConditionMessage builds the node condition message while respecting the max node condition
// message length. It applies two tiers of truncation:
//  1. If full messages exceed the limit, compact each message's free-text diagnostic field
//     to compactMessageFieldLen bytes, preserving entity identifiers needed for recovery.
//  2. If compacted messages still exceed the limit, truncate the last entry at the byte level
//     to fill the remaining space.
func (r *K8sConnector) truncateNodeConditionMessage(messages []string) string {
	maxLen := int(r.config.MaxNodeConditionMessageLength)

	// When messages exceed the limit, first remove identity-duplicates (same
	// ErrorCode + entity + Recommended Action) to reclaim space, then compact.
	if totalMessageLength(messages) > maxLen {
		messages = deduplicateMessagesByIdentity(messages)

		compacted := make([]string, len(messages))
		for i, msg := range messages {
			compacted[i] = compactMessageField(msg, int(r.config.CompactedHealthEventMsgLen))
		}

		messages = compacted
	}

	// Tier 2: build the result, truncating at byte level if compacted messages still don't fit.
	var result strings.Builder

	truncated := false

	for i, msg := range messages {
		separator := ""
		if i > 0 {
			separator = ";"
		}

		// +1 accounts for the trailing semicolon that is always appended after the loop
		if result.Len()+len(separator)+len(msg)+1 > maxLen {
			available := maxLen - result.Len() - len(separator) - 1 - len(truncationSuffix)
			if available > 0 {
				result.WriteString(separator)
				result.WriteString(msg[:available])
			}

			truncated = true

			break
		}

		result.WriteString(separator)
		result.WriteString(msg)
	}

	result.WriteString(";")

	if truncated {
		result.WriteString(truncationSuffix)
	}

	return result.String()
}
