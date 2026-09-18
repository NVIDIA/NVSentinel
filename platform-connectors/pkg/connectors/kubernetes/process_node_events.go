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
	"k8s.io/apimachinery/pkg/util/wait"
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
// The events must be in timestamp order: prepareHealthEventWrites sorts the whole
// batch once, for this path and the Event path alike.
func (r *K8sConnector) updateNodeConditions(ctx context.Context, healthEvents []*protos.HealthEvent) (bool, error) {
	nodeName := ""
	if len(healthEvents) > 0 && healthEvents[0] != nil {
		nodeName = healthEvents[0].NodeName
	}

	conditionEventsMap := buildConditionEventsMap(healthEvents)

	if len(conditionEventsMap) == 0 {
		return false, nil
	}

	ctx, span := tracing.StartSpan(ctx, "platform_connector.k8s.update_node_condition")
	defer span.End()

	span.SetAttributes(
		attribute.Int("platform_connector.k8s.node_condition_update_count", len(conditionEventsMap)),
	)

	skipped := false

	err := retryKubernetesAPIWrite(ctx, func(err error) bool {
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
// shows: a repeat from a monitor that reports every cycle, or a resent batch,
// then costs no update call.
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

	if !changed {
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

		// An entry naming the same fault (error codes, entities, action) is
		// that fault, whatever its text. Compaction at the length cap rewrites
		// the text, so without this a saturated message would gain the fault
		// again, move it, and count as a change on every repeat.
		if messagesMatchByIdentity(msg, newMessage[:len(newMessage)-1]) {
			return messages
		}
	}

	return append(messages, newMessage[:len(newMessage)-1])
}

// extractMessageIdentity parses a node condition message into what
// identifies its fault: the ErrorCode tokens, every entity token (GPU, PCI,
// GPU_UUID, NVSWITCH, NVLINK, ...) and the Recommended Action. Compaction
// keeps that identity prefix whole and shortens only the diagnostic text, so
// this works on full and compacted messages alike.
func extractMessageIdentity(msg string) (errorCodes []string, entities []string, recommendedAction string) {
	prefix := msg

	if raIdx := strings.LastIndex(msg, recommendedActionMarker); raIdx >= 0 {
		recommendedAction = strings.TrimRight(msg[raIdx:], " ")
		prefix = msg[:raIdx]
	}

	identityPrefix, _ := splitIdentityAndDiagnostic(strings.TrimRight(prefix, " "))

	for token := range strings.FieldsSeq(identityPrefix) {
		if strings.HasPrefix(token, "ErrorCode:") {
			errorCodes = append(errorCodes, token)
		} else {
			entities = append(entities, token)
		}
	}

	return errorCodes, entities, recommendedAction
}

// messagesMatchByIdentity reports whether two messages name the same fault:
// the same ErrorCodes, the same Recommended Action and the same entities, all
// of them. Two faults that share an entity but differ in another, two SXID
// faults on one NVSwitch that hit different GPUs and links for example, are
// different faults and both stay in the condition.
func messagesMatchByIdentity(a, b string) bool {
	aErr, aEnt, aRA := extractMessageIdentity(a)
	bErr, bEnt, bRA := extractMessageIdentity(b)

	if aRA != bRA || !slices.Equal(aErr, bErr) || len(aEnt) != len(bEnt) {
		return false
	}

	slices.Sort(aEnt)
	slices.Sort(bEnt)

	for i := range aEnt {
		if !sameEntity(aEnt[i], bEnt[i]) {
			return false
		}
	}

	return true
}

// sameEntity reports whether two entity tokens name the same entity. The
// last entry of a message that still does not fit after compaction is cut at
// the byte level, so a token ending in the truncation suffix matches the
// token it is a prefix of.
func sameEntity(a, b string) bool {
	if a == b {
		return true
	}

	if cut, ok := strings.CutSuffix(a, truncationSuffix); ok {
		return strings.HasPrefix(b, cut)
	}

	if cut, ok := strings.CutSuffix(b, truncationSuffix); ok {
		return strings.HasPrefix(a, cut)
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

// nodeEventRefreshInterval is how long repeats of a fault whose Event is
// already written are skipped. Once it has passed, the next repeat
// refreshes the Event (count and timestamp), so a fault that lasts stays in
// the Event list, which drops Events an hour after their last write. It also
// bounds how long a replica can miss a recurrence when the recovery in
// between was reported to another replica, and how long a write is
// remembered at all: the memory holds only the faults written inside the
// last interval, so it needs no size.
const nodeEventRefreshInterval = 10 * time.Minute

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

// nodeEventMemory is the memory of written Events, keyed by node and check.
// Entries expire nodeEventRefreshInterval after their last write and the
// memory has no size limit: it can hold only faults whose Event was written
// in the last interval, and every such write was an API call. It is built on
// first use so that a zero K8sConnector works.
func (r *K8sConnector) nodeEventMemory() *expirable.LRU[string, map[string]rememberedEvent] {
	if r.nodeEvents == nil {
		r.nodeEvents = expirable.NewLRU[string, map[string]rememberedEvent](0, nil, nodeEventRefreshInterval)
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
	now := time.Now()

	written, ok := r.nodeEventMemory().Get(key)
	if !ok {
		written = map[string]rememberedEvent{}
	}

	// A write is useful only inside the refresh interval; after it the next
	// repeat refreshes the Event through the API anyway. Dropping the stale
	// ones here keeps the check's memory to the faults written in the last
	// interval, however many distinct messages it produces over time.
	for message, remembered := range written {
		if now.Sub(remembered.writtenAt) >= nodeEventRefreshInterval {
			delete(written, message)
		}
	}

	written[event.Message] = rememberedEvent{entities: entities, writtenAt: now}

	// Added again so the entry's expiry follows its last write; a check that
	// stops reporting leaves the memory by itself.
	r.nodeEventMemory().Add(key, written)
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
// healthy report names no entity. Called on a healthy report, so the fault's
// next occurrence is announced again instead of skipped as a repeat.
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

// refreshNodeEvent reconciles the named fault's Event. It returns false when
// the Event disappeared and should be created again.
func (r *K8sConnector) refreshNodeEvent(
	ctx context.Context, event *corev1.Event, nodeName string,
) (bool, error) {
	_, err := r.reconcileNodeEvent(ctx, event)
	if apierrors.IsNotFound(err) {
		r.forgetNodeEvent(nodeName, event)

		return false, nil
	}

	return true, err
}

// nodeEventWrite retains one occurrence across both client and connector retries.
type nodeEventWrite struct {
	event           *corev1.Event
	entities        []string
	createAttempted bool
}

// writeNodeEvent skips recently announced faults and reconciles pending writes
// by their stable name. Recovery steps clear the memory before a recurrence.
func (r *K8sConnector) writeNodeEvent(ctx context.Context, write *nodeEventWrite, nodeName string) (bool, error) {
	ctx, span := tracing.StartSpan(ctx, "platform_connector.k8s.update_node_event")
	defer span.End()

	event := write.event
	span.SetAttributes(
		attribute.String("platform_connector.k8s.event_reason", event.Reason),
		attribute.String("platform_connector.k8s.event_type", event.Type),
	)

	remembered, known := r.rememberedNodeEvent(nodeName, event)
	if known && time.Since(remembered.writtenAt) < nodeEventRefreshInterval {
		nodeEventOperationsCounter.WithLabelValues(OperationUpdate, StatusSkipped).Inc()

		return true, nil
	}

	err := retryKubernetesAPIWrite(ctx, func(err error) bool {
		return apierrors.IsConflict(err) || isTemporaryError(err)
	}, func() error {
		if known {
			refreshed, err := r.refreshNodeEvent(ctx, event, nodeName)
			if refreshed {
				return err
			}

			known = false
		}

		_, err := r.createOrReconcileNodeEvent(ctx, write)

		return err
	})
	if err != nil {
		tracing.RecordError(span, err)

		return false, fmt.Errorf("failed to write event for node %s: %w", nodeName, err)
	}

	r.rememberNodeEvent(nodeName, event, write.entities)

	return false, nil
}

// createOrReconcileNodeEvent resolves an uncertain create before trying another POST.
// The first attempt needs no lookup; later attempts create only after a NotFound.
func (r *K8sConnector) createOrReconcileNodeEvent(ctx context.Context, write *nodeEventWrite) (*corev1.Event, error) {
	if write.createAttempted {
		existing, err := r.reconcileNodeEvent(ctx, write.event)
		if !apierrors.IsNotFound(err) {
			return existing, err
		}
	}

	write.createAttempted = true

	created, err := r.clientset.CoreV1().Events(DefaultNamespace).Create(ctx, write.event, metav1.CreateOptions{})
	if err == nil {
		nodeEventOperationsCounter.WithLabelValues(OperationCreate, StatusSuccess).Inc()

		return created, nil
	}

	if !apierrors.IsAlreadyExists(err) {
		nodeEventOperationsCounter.WithLabelValues(OperationCreate, StatusFailed).Inc()

		return nil, err
	}

	// A concurrent create can win after our lookup. Verify it rather than
	// treating every name collision as the successful delivery of this event.
	existing, lookupErr := r.reconcileNodeEvent(ctx, write.event)
	if apierrors.IsNotFound(lookupErr) {
		// The object was deleted between AlreadyExists and GET; retry the lookup.
		return nil, apierrors.NewConflict(corev1.Resource("events"), write.event.Name, lookupErr)
	}

	return existing, lookupErr
}

// reconcileNodeEvent validates the fault behind a stable name. Its last timestamp
// identifies the latest persisted occurrence: replay accepts it without increasing
// Count, while a later occurrence refreshes the same Event. FirstTimestamp belongs
// to the first occurrence and is preserved across replicas and restarts.
func (r *K8sConnector) reconcileNodeEvent(ctx context.Context, event *corev1.Event) (*corev1.Event, error) {
	existing, err := r.clientset.CoreV1().Events(DefaultNamespace).Get(ctx, event.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("look up pending event %s: %w", event.Name, err)
	}

	if !sameNodeEventFault(existing, event) {
		return nil, fmt.Errorf("existing event %s does not match the pending write", event.Name)
	}

	// Kubernetes stores metav1.Time at second precision. An already persisted
	// occurrence, or a newer one from another replica, must not be counted again.
	if existing.LastTimestamp.Unix() >= event.LastTimestamp.Unix() {
		return existing, nil
	}

	existing.Count++
	existing.LastTimestamp = event.LastTimestamp

	updated, err := r.clientset.CoreV1().Events(DefaultNamespace).Update(ctx, existing, metav1.UpdateOptions{})
	if err != nil {
		nodeEventOperationsCounter.WithLabelValues(OperationUpdate, StatusFailed).Inc()

		return nil, fmt.Errorf("refresh pending event %s: %w", event.Name, err)
	}

	nodeEventOperationsCounter.WithLabelValues(OperationUpdate, StatusSuccess).Inc()

	return updated, nil
}

// sameNodeEventFault checks immutable fault identity; timestamps and count vary
// across occurrences now that the name identifies the fault rather than a report.
func sameNodeEventFault(existing, expected *corev1.Event) bool {
	return existing.InvolvedObject == expected.InvolvedObject &&
		existing.Source == expected.Source &&
		existing.Type == expected.Type &&
		existing.Reason == expected.Reason &&
		existing.Message == expected.Message &&
		existing.ReportingController == expected.ReportingController &&
		existing.ReportingInstance == expected.ReportingInstance
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

// kubernetesWrite is one independently retryable node status update or Event write.
type kubernetesWrite struct {
	operation string
	nodeName  string
	run       func(context.Context) error
}

// processHealthEvents executes one pass for synchronous callers. They receive
// every failure and retain responsibility for retrying unacknowledged batches.
func (r *K8sConnector) processHealthEvents(ctx context.Context, events *protos.HealthEvents) error {
	ctx, span := tracing.StartSpan(ctx, "platform_connector.k8s.process_health_events")
	defer span.End()

	var failures []error

	for _, write := range r.prepareHealthEventWrites(ctx, events) {
		if err := write.run(ctx); err != nil {
			tracing.RecordError(span, err)
			failures = append(failures, fmt.Errorf("%s write for node %s: %w", write.operation, write.nodeName, err))
		}
	}

	return errors.Join(failures...)
}

// prepareHealthEventWrites fixes the work list once so retries cannot repeat successful writes.
func (r *K8sConnector) prepareHealthEventWrites(
	ctx context.Context, healthEvents *protos.HealthEvents,
) []kubernetesWrite {
	processableEvents := sortHealthEventsByTimestamp(filterProcessableEvents(ctx, healthEvents))

	var conditionEvents []*protos.HealthEvent

	for _, event := range processableEvents {
		if event.IsHealthy || event.IsFatal {
			conditionEvents = append(conditionEvents, event)
		}
	}

	eventsByNode := groupEventsByNode(conditionEvents)

	nodeNames := make([]string, 0, len(eventsByNode))
	for nodeName := range eventsByNode {
		nodeNames = append(nodeNames, nodeName)
	}

	slices.Sort(nodeNames)

	writes := make([]kubernetesWrite, 0, len(nodeNames)+len(processableEvents))
	for _, nodeName := range nodeNames {
		nodeEvents := eventsByNode[nodeName]
		writes = append(writes, kubernetesWrite{
			operation: "node_condition", nodeName: nodeName,
			run: func(ctx context.Context) error { return r.processNodeConditionUpdates(ctx, nodeEvents) },
		})
	}

	for _, healthEvent := range processableEvents {
		if healthEvent.IsHealthy {
			// Keep recovery in the same ordered work list: preparing it must not
			// erase the memory before an earlier fault has finished retrying.
			writes = append(writes, kubernetesWrite{
				operation: "node_event", nodeName: healthEvent.NodeName,
				run: func(context.Context) error {
					r.forgetNodeCheck(healthEvent.NodeName, healthEvent.CheckName, entityKeys(healthEvent))

					return nil
				},
			})

			continue
		}

		if healthEvent.IsFatal {
			continue
		}

		write := &nodeEventWrite{event: r.createK8sEvent(ctx, healthEvent), entities: entityKeys(healthEvent)}
		writes = append(writes, kubernetesWrite{
			operation: "node_event", nodeName: healthEvent.NodeName,
			run: func(ctx context.Context) error {
				start := time.Now()
				skipped, err := r.writeNodeEvent(ctx, write, healthEvent.NodeName)

				if !skipped {
					nodeEventUpdateCreateDuration.Observe(float64(time.Since(start).Milliseconds()))
				}

				return err
			},
		})
	}

	return writes
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

// retryKubernetesAPIWrite preserves client-go's short retry policy while honoring
// the batch deadline and connector shutdown during both API calls and backoff.
func retryKubernetesAPIWrite(ctx context.Context, retryable func(error) bool, run func() error) error {
	var lastErr error

	err := wait.ExponentialBackoffWithContext(ctx, retry.DefaultRetry, func(context.Context) (bool, error) {
		lastErr = run()
		if lastErr == nil {
			return true, nil
		}

		if retryable(lastErr) {
			return false, nil
		}

		return false, lastErr
	})
	if wait.Interrupted(err) && ctx.Err() == nil {
		return lastErr
	}

	return err
}
