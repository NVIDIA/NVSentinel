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

package reconciler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/coldstart"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/evaluator"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/eventwatcher"
	healthEventsAnnotation "github.com/nvidia/nvsentinel/fault-quarantine/pkg/healthEventsAnnotation"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

type recoveryHistoryStoreStub struct {
	datastore.HealthEventStore
	events []datastore.HealthEventWithStatus
}

type phaseAwareRecoveryDatabase struct {
	client.DatabaseClient
	completed    map[string]struct{}
	statusWrites int
}

func (d *phaseAwareRecoveryDatabase) UpdateManyDocuments(
	_ context.Context,
	filter any,
	_ any,
) (*client.UpdateResult, error) {
	builder, ok := filter.(datastore.QueryBuilder)
	if !ok {
		return nil, fmt.Errorf("unexpected completion filter %T", filter)
	}
	idFilter, ok := builder.ToMongo()["_id"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("completion filter has no _id predicate")
	}
	ids, ok := idFilter["$in"].([]any)
	if !ok {
		return nil, fmt.Errorf("completion filter has no $in IDs")
	}
	if d.completed == nil {
		d.completed = make(map[string]struct{})
	}
	for _, id := range ids {
		d.completed[fmt.Sprint(id)] = struct{}{}
	}

	return &client.UpdateResult{}, nil
}

func (d *phaseAwareRecoveryDatabase) UpdateDocumentStatusFields(
	context.Context,
	string,
	map[string]any,
) error {
	d.statusWrites++

	return nil
}

type phaseAwareRecoveryStore struct {
	datastore.HealthEventStore
	db     *phaseAwareRecoveryDatabase
	events []datastore.HealthEventWithStatus
}

func (s *phaseAwareRecoveryStore) FindHealthEventsByQueryBatched(
	_ context.Context,
	builder datastore.QueryBuilder,
	_ int,
	visit func([]datastore.HealthEventWithStatus) error,
) error {
	filterTerminal := strings.Contains(
		fmt.Sprint(builder.ToMongo()), coldstart.RecoveryCompletionStatusPath)
	visible := make([]datastore.HealthEventWithStatus, 0, len(s.events))
	for i := range s.events {
		id := fmt.Sprint(s.events[i].RawEvent["id"])
		if _, completed := s.db.completed[id]; filterTerminal && completed {
			continue
		}
		visible = append(visible, s.events[i])
	}

	return visit(visible)
}

func (s *recoveryHistoryStoreStub) FindHealthEventsByQueryBatched(
	_ context.Context,
	_ datastore.QueryBuilder,
	_ int,
	visit func([]datastore.HealthEventWithStatus) error,
) error {
	return visit(s.events)
}

type reconcilerRecoveryProcessor struct {
	reconciler *Reconciler
	evaluators []evaluator.RuleSetEvaluatorIface
	rulesets   rulesetsConfig
}

type recordingErrorCodeEvaluator struct {
	name string
	code string
	seen [][]string
}

type projectedEventRecorder struct {
	name string
	seen []*protos.HealthEvent
}

func (e *projectedEventRecorder) Evaluate(
	_ context.Context,
	event *protos.HealthEvent,
) (common.RuleEvaluationResult, error) {
	e.seen = append(e.seen, event)

	return common.RuleEvaluationSuccess, nil
}

func (e *projectedEventRecorder) GetName() string  { return e.name }
func (*projectedEventRecorder) GetVersion() string { return "1" }
func (*projectedEventRecorder) GetPriority() int   { return 0 }

func (e *recordingErrorCodeEvaluator) Evaluate(
	_ context.Context,
	event *protos.HealthEvent,
) (common.RuleEvaluationResult, error) {
	codes := append([]string(nil), event.GetErrorCode()...)
	e.seen = append(e.seen, codes)
	for _, code := range codes {
		if code == e.code {
			return common.RuleEvaluationSuccess, nil
		}
	}

	return common.RuleEvaluationFailed, nil
}

func (e *recordingErrorCodeEvaluator) GetName() string  { return e.name }
func (*recordingErrorCodeEvaluator) GetVersion() string { return "1" }
func (*recordingErrorCodeEvaluator) GetPriority() int   { return 0 }

type allErrorCodesEvaluator struct {
	name     string
	required map[string]struct{}
	seen     [][]string
}

func (e *allErrorCodesEvaluator) Evaluate(
	_ context.Context,
	event *protos.HealthEvent,
) (common.RuleEvaluationResult, error) {
	codes := append([]string(nil), event.GetErrorCode()...)
	e.seen = append(e.seen, codes)
	remaining := make(map[string]struct{}, len(e.required))
	for code := range e.required {
		remaining[code] = struct{}{}
	}
	for _, code := range codes {
		delete(remaining, code)
	}
	if len(remaining) == 0 {
		return common.RuleEvaluationSuccess, nil
	}

	return common.RuleEvaluationFailed, nil
}

func (e *allErrorCodesEvaluator) GetName() string  { return e.name }
func (*allErrorCodesEvaluator) GetVersion() string { return "1" }
func (*allErrorCodesEvaluator) GetPriority() int   { return 0 }

type flakyProjectedEvaluator struct {
	name     string
	failCode string
	err      error
}

func (e *flakyProjectedEvaluator) Evaluate(
	_ context.Context,
	event *protos.HealthEvent,
) (common.RuleEvaluationResult, error) {
	for _, code := range event.GetErrorCode() {
		if code == e.failCode && e.err != nil {
			return common.RuleEvaluationFailed, e.err
		}
	}

	return common.RuleEvaluationSuccess, nil
}

func (e *flakyProjectedEvaluator) GetName() string  { return e.name }
func (*flakyProjectedEvaluator) GetVersion() string { return "1" }
func (*flakyProjectedEvaluator) GetPriority() int   { return 0 }

func (p *reconcilerRecoveryProcessor) ProcessStoredEvent(
	ctx context.Context,
	event model.HealthEventWithStatus,
	_ string,
) (coldstart.ProcessResult, error) {
	status, err := p.reconciler.ProcessEvent(
		coldstart.WithRecoveryContext(ctx), &event, p.evaluators, p.rulesets)
	if err != nil {
		return coldstart.ProcessResultFailed, err
	}
	if status == nil {
		return coldstart.ProcessResultSkipped, nil
	}

	return coldstart.ProcessResultProcessed, nil
}

func (*reconcilerRecoveryProcessor) CompleteStoredEvents(
	context.Context,
	[]coldstart.StoredEventCompletion,
) error {
	return nil
}

func recoveryStoreRecord(
	t testing.TB,
	id string,
	createdAt time.Time,
	event *protos.HealthEvent,
) datastore.HealthEventWithStatus {
	t.Helper()
	encoded, err := json.Marshal(event)
	require.NoError(t, err)
	var eventMap map[string]any
	require.NoError(t, json.Unmarshal(encoded, &eventMap))

	return datastore.HealthEventWithStatus{
		CreatedAt: createdAt,
		RawEvent: datastore.Event{
			"id": id, "healthevent": eventMap, "healtheventstatus": map[string]any{},
		},
	}
}

func recoverHistoryWithInitialAnnotation(
	t testing.TB,
	initial []*protos.HealthEvent,
	oldHealthy *protos.HealthEvent,
	newerFailure *protos.HealthEvent,
) (*corev1.Node, *healthEventsAnnotation.HealthEventsAnnotationMap, bool) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	annotation := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	for _, event := range initial {
		require.True(t, annotation.AddOrUpdateEvent(event))
	}
	annotationJSON, err := json.Marshal(annotation)
	require.NoError(t, err)

	clientset := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldHealthy.GetNodeName(),
			Annotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey:           string(annotationJSON),
				common.QuarantineHealthEventIsCordonedAnnotationKey: common.QuarantineHealthEventIsCordonedAnnotationValueTrue,
			},
		},
		Spec: corev1.NodeSpec{Unschedulable: true},
	})
	uncordonObserved := false
	clientset.PrependReactor("patch", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patchAction, ok := action.(k8stesting.PatchAction)
		if ok && bytes.Contains(patchAction.GetPatch(), []byte(`"unschedulable":false`)) {
			uncordonObserved = true
		}

		return false, nil, nil
	})
	r := NewReconciler(ReconcilerConfig{}, &informer.FaultQuarantineClient{Clientset: clientset}, nil)
	r.SetLabelKeys("nvsentinel.nvidia.com/")
	matchAll := &stubRuleSetEvaluator{name: "all", result: common.RuleEvaluationSuccess}
	processor := &reconcilerRecoveryProcessor{
		reconciler: r,
		evaluators: []evaluator.RuleSetEvaluatorIface{matchAll},
		rulesets: rulesetsConfig{
			CordonConfigMap: map[string]bool{"all": true},
		},
	}
	store := &recoveryHistoryStoreStub{events: []datastore.HealthEventWithStatus{
		recoveryStoreRecord(t, "old-healthy", base, oldHealthy),
		recoveryStoreRecord(t, "newer-failure", base.Add(time.Minute), newerFailure),
	}}

	require.NoError(t, coldstart.Handle(ctx, coldstart.Dependencies{
		HealthEventStore: store, EventProcessor: processor,
		ColdStartAfterTime: base.Add(-time.Minute), ColdStartUntilTime: base.Add(time.Hour),
	}))
	node, err := clientset.CoreV1().Nodes().Get(ctx, oldHealthy.GetNodeName(), metav1.GetOptions{})
	require.NoError(t, err)
	require.True(t, node.Spec.Unschedulable)
	finalAnnotation := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	require.NoError(t, json.Unmarshal(
		[]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), finalAnnotation))

	return node, finalAnnotation, uncordonObserved
}

func TestHasExistingQuarantineReturnsRecoveryNodeLookupFailure(t *testing.T) {
	lookupErr := errors.New("API server unavailable")
	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, lookupErr
	})
	r := &Reconciler{k8sClient: &informer.FaultQuarantineClient{Clientset: clientset}}

	annotations, quarantined, err := r.hasExistingQuarantine(
		coldstart.WithRecoveryContext(context.Background()), "node-a")

	require.ErrorIs(t, err, lookupErr)
	assert.Nil(t, annotations)
	assert.False(t, quarantined)
}

func TestRemoveFinalRecoveredEventRetainsAnnotationUntilUnquarantineSucceeds(t *testing.T) {
	ctx := coldstart.WithRecoveryContext(context.Background())
	event := &protos.HealthEvent{
		Agent: "gpu-health-monitor", ComponentClass: "GPU", CheckName: "GpuNvlinkWatch",
		NodeName: "node-a", Version: 1, IsHealthy: true,
		EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
	}
	storedEvent := proto.Clone(event).(*protos.HealthEvent)
	storedEvent.IsHealthy = false
	annotationMap := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	require.True(t, annotationMap.AddOrUpdateEvent(storedEvent))
	annotationJSON, err := json.Marshal(annotationMap)
	require.NoError(t, err)

	clientset := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-a",
			Annotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey: string(annotationJSON),
			},
		},
	})
	r := &Reconciler{k8sClient: &informer.FaultQuarantineClient{Clientset: clientset}}

	updated, err := r.removeEventFromAnnotation(ctx, event)
	require.NoError(t, err)
	require.True(t, updated.IsEmpty())

	node, err := clientset.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, string(annotationJSON), node.Annotations[common.QuarantineHealthEventAnnotationKey],
		"a failed subsequent unquarantine must leave enough state for recovery to retry")

	stillQuarantined, err := r.performUncordon(ctx, event, map[string]string{
		common.QuarantineHealthEventAnnotationKey: string(annotationJSON),
	})
	require.NoError(t, err)
	assert.False(t, stillQuarantined)
	node, err = clientset.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, node.Annotations, common.QuarantineHealthEventAnnotationKey,
		"the successful atomic unquarantine must consume the retained retry marker")
}

func TestRecoveryTransientRuleEvaluationAppliesNoPartialActionsBeforeRetry(t *testing.T) {
	transientErr := errors.New("temporary evaluator failure")
	clientset := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
	})
	fqClient := &informer.FaultQuarantineClient{Clientset: clientset}
	r := NewReconciler(ReconcilerConfig{}, fqClient, nil)
	r.SetLabelKeys("nvsentinel.nvidia.com/")

	event := &model.HealthEventWithStatus{HealthEvent: &protos.HealthEvent{
		Agent: "gpu-health-monitor", ComponentClass: "GPU", CheckName: "GpuNvlinkWatch",
		NodeName: "node-a", Version: 1,
	}}
	working := &stubRuleSetEvaluator{name: "taint", result: common.RuleEvaluationSuccess}
	flaky := &stubRuleSetEvaluator{name: "cordon", result: common.RuleEvaluationFailed, err: transientErr}
	evaluators := []evaluator.RuleSetEvaluatorIface{working, flaky}
	rulesets := rulesetsConfig{
		TaintConfigMap: map[string]*config.Taint{
			"taint": {Key: "nvidia.com/gpu-fault", Value: "true", Effect: string(corev1.TaintEffectNoSchedule)},
		},
		CordonConfigMap:    map[string]bool{"cordon": true},
		RuleSetPriorityMap: map[string]int{"taint": 1, "cordon": 2},
		RuleSetOrderMap:    map[string]int{"taint": 0, "cordon": 1},
	}

	status, err := r.handleEvent(coldstart.WithRecoveryContext(context.Background()), event, evaluators, rulesets)
	require.ErrorIs(t, err, transientErr)
	assert.Nil(t, status)
	node, err := clientset.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	assert.False(t, node.Spec.Unschedulable)
	assert.Empty(t, node.Spec.Taints)
	assert.Empty(t, node.Annotations[common.QuarantineHealthEventAnnotationKey])

	flaky.err = nil
	flaky.result = common.RuleEvaluationSuccess
	status, err = r.handleEvent(coldstart.WithRecoveryContext(context.Background()), event, evaluators, rulesets)
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.Equal(t, model.Quarantined, *status)
	node, err = clientset.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, node.Spec.Unschedulable, "the retry must apply the recovered cordon action")
	require.Len(t, node.Spec.Taints, 1)
	assert.Equal(t, "nvidia.com/gpu-fault", node.Spec.Taints[0].Key)
	assert.NotEmpty(t, node.Annotations[common.QuarantineHealthEventAnnotationKey])
}

func TestFinalRecoveryRetriesAfterAtomicUnquarantineFailure(t *testing.T) {
	ctx := context.Background()
	transientErr := errors.New("temporary node patch failure")
	healthy := &protos.HealthEvent{
		Agent: "gpu-health-monitor", ComponentClass: "GPU", CheckName: "GpuNvlinkWatch",
		NodeName: "node-a", Version: 1, IsHealthy: true,
		EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
	}
	failure := proto.Clone(healthy).(*protos.HealthEvent)
	failure.IsHealthy = false
	annotationMap := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	require.True(t, annotationMap.AddOrUpdateEvent(failure))
	annotationJSON, err := json.Marshal(annotationMap)
	require.NoError(t, err)

	clientset := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-a",
			Annotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey:           string(annotationJSON),
				common.QuarantineHealthEventIsCordonedAnnotationKey: common.QuarantineHealthEventIsCordonedAnnotationValueTrue,
			},
		},
		Spec: corev1.NodeSpec{Unschedulable: true},
	})
	patchCalls := 0
	clientset.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		patchCalls++
		if patchCalls == 1 {
			return true, nil, transientErr
		}

		return false, nil, nil
	})
	fqClient := &informer.FaultQuarantineClient{Clientset: clientset}
	r := NewReconciler(ReconcilerConfig{}, fqClient, nil)
	r.SetLabelKeys("nvsentinel.nvidia.com/")

	stillQuarantined, err := r.handleQuarantinedNode(
		coldstart.WithRecoveryContext(ctx), healthy, nil, rulesetsConfig{})
	require.ErrorIs(t, err, transientErr)
	assert.True(t, stillQuarantined)
	node, err := clientset.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, node.Spec.Unschedulable)
	assert.Equal(t, string(annotationJSON), node.Annotations[common.QuarantineHealthEventAnnotationKey])

	stillQuarantined, err = r.handleQuarantinedNode(
		coldstart.WithRecoveryContext(ctx), healthy, nil, rulesetsConfig{})
	require.NoError(t, err)
	assert.False(t, stillQuarantined)
	node, err = clientset.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	assert.False(t, node.Spec.Unschedulable)
	assert.NotContains(t, node.Annotations, common.QuarantineHealthEventAnnotationKey)
	assert.Equal(t, 2, patchCalls)
}

func TestColdStartHealthyEventNeverUncordonsBeforeNewerFailedEntity(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	identity := func(healthy bool, entities []*protos.Entity) *protos.HealthEvent {
		return &protos.HealthEvent{
			Version: 1, Agent: "agent", ComponentClass: "GPU", CheckName: "check", NodeName: "node-a",
			IsHealthy: healthy, EntitiesImpacted: entities,
		}
	}
	failureA := identity(false, []*protos.Entity{{EntityType: "GPU", EntityValue: "A"}})
	failureB := identity(false, []*protos.Entity{{EntityType: "GPU", EntityValue: "B"}})
	annotationMap := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	require.True(t, annotationMap.AddOrUpdateEvent(failureB))
	annotationJSON, err := json.Marshal(annotationMap)
	require.NoError(t, err)

	clientset := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-a",
			Annotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey:           string(annotationJSON),
				common.QuarantineHealthEventIsCordonedAnnotationKey: common.QuarantineHealthEventIsCordonedAnnotationValueTrue,
			},
		},
		Spec: corev1.NodeSpec{Unschedulable: true},
	})
	uncordonObserved := false
	clientset.PrependReactor("patch", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patchAction, ok := action.(k8stesting.PatchAction)
		if ok && bytes.Contains(patchAction.GetPatch(), []byte(`"unschedulable":false`)) {
			uncordonObserved = true
		}

		return false, nil, nil
	})
	fqClient := &informer.FaultQuarantineClient{Clientset: clientset}
	r := NewReconciler(ReconcilerConfig{}, fqClient, nil)
	r.SetLabelKeys("nvsentinel.nvidia.com/")
	processor := &reconcilerRecoveryProcessor{
		reconciler: r,
		evaluators: []evaluator.RuleSetEvaluatorIface{
			&stubRuleSetEvaluator{name: "match", result: common.RuleEvaluationSuccess},
		},
		rulesets: rulesetsConfig{
			CordonConfigMap: map[string]bool{"match": true},
		},
	}

	storedRecord := func(id string, createdAt time.Time, event *protos.HealthEvent) datastore.HealthEventWithStatus {
		encoded, marshalErr := json.Marshal(event)
		require.NoError(t, marshalErr)
		var eventMap map[string]any
		require.NoError(t, json.Unmarshal(encoded, &eventMap))

		return datastore.HealthEventWithStatus{
			CreatedAt: createdAt,
			RawEvent: datastore.Event{
				"id": id, "healthevent": eventMap, "healtheventstatus": map[string]any{},
			},
		}
	}
	oldCheckWideHealthy := storedRecord("old-healthy", base, identity(true, nil))
	newerFailureA := storedRecord("newer-failure", base.Add(time.Minute), failureA)
	store := &recoveryHistoryStoreStub{events: []datastore.HealthEventWithStatus{
		oldCheckWideHealthy, newerFailureA,
	}}

	require.NoError(t, coldstart.Handle(ctx, coldstart.Dependencies{
		HealthEventStore: store, EventProcessor: processor,
		ColdStartAfterTime: base.Add(-time.Minute), ColdStartUntilTime: base.Add(time.Hour),
	}))
	assert.False(t, uncordonObserved,
		"a partially outdated recovery must never expose a schedulable node with a newer active fault")
	node, err := clientset.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, node.Spec.Unschedulable)
	remaining := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	require.NoError(t, json.Unmarshal(
		[]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), remaining))
	require.Equal(t, 1, remaining.Count())
	for key := range remaining.Events {
		assert.Equal(t, "A", key.EntityValue)
	}
}

func TestColdStartHealthyEventNeverUncordonsBeforeNewerDifferentCode(t *testing.T) {
	baseEvent := &protos.HealthEvent{
		Version: 1, Agent: "agent", ComponentClass: "GPU", CheckName: "check", NodeName: "node-a",
		EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "A"}},
	}
	oldFailure := proto.Clone(baseEvent).(*protos.HealthEvent)
	oldFailure.ErrorCode = []string{"48"}
	oldHealthy := proto.Clone(oldFailure).(*protos.HealthEvent)
	oldHealthy.IsHealthy = true
	newerFailure := proto.Clone(baseEvent).(*protos.HealthEvent)
	newerFailure.ErrorCode = []string{"79"}

	_, annotation, uncordonObserved := recoverHistoryWithInitialAnnotation(
		t, []*protos.HealthEvent{oldFailure}, oldHealthy, newerFailure)
	assert.False(t, uncordonObserved,
		"a recovery for an older code must not uncordon before a newer code is restored")
	require.Equal(t, 1, annotation.Count())
	for key := range annotation.Events {
		assert.Equal(t, "A", key.EntityValue)
		assert.Equal(t, "79", key.ErrorCode)
	}
}

func TestColdStartHealthyWildcardLeavesOnlyNewerUncodedFault(t *testing.T) {
	baseEvent := &protos.HealthEvent{
		Version: 1, Agent: "agent", ComponentClass: "GPU", CheckName: "check", NodeName: "node-a",
		EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "A"}},
	}
	oldFailure := proto.Clone(baseEvent).(*protos.HealthEvent)
	oldFailure.ErrorCode = []string{"48", "79"}
	oldHealthy := proto.Clone(baseEvent).(*protos.HealthEvent)
	oldHealthy.IsHealthy = true
	newerFailure := proto.Clone(baseEvent).(*protos.HealthEvent)

	_, annotation, uncordonObserved := recoverHistoryWithInitialAnnotation(
		t, []*protos.HealthEvent{oldFailure}, oldHealthy, newerFailure)
	assert.False(t, uncordonObserved)
	require.Equal(t, 1, annotation.Count())
	for key := range annotation.Events {
		assert.Equal(t, "A", key.EntityValue)
		assert.Empty(t, key.ErrorCode,
			"the old wildcard recovery must clear coded faults but preserve the newer uncoded fault")
	}
}

func TestColdStartCheckWideRecoveryLeavesOnlyNewerCheckWideFault(t *testing.T) {
	oldFailure := &protos.HealthEvent{
		Version: 1, Agent: "agent", ComponentClass: "GPU", CheckName: "check", NodeName: "node-a",
		ErrorCode: []string{"79"},
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "A"},
			{EntityType: "GPU", EntityValue: "B"},
		},
	}
	oldHealthy := proto.Clone(oldFailure).(*protos.HealthEvent)
	oldHealthy.IsHealthy = true
	oldHealthy.EntitiesImpacted = nil
	newerFailure := proto.Clone(oldHealthy).(*protos.HealthEvent)
	newerFailure.IsHealthy = false

	_, annotation, uncordonObserved := recoverHistoryWithInitialAnnotation(
		t, []*protos.HealthEvent{oldFailure}, oldHealthy, newerFailure)
	assert.False(t, uncordonObserved)
	require.Equal(t, 1, annotation.Count())
	for key := range annotation.Events {
		assert.Empty(t, key.EntityType)
		assert.Empty(t, key.EntityValue)
		assert.Equal(t, "79", key.ErrorCode,
			"the old check-wide recovery must clear entity faults but preserve the newer check-wide fault")
	}
}

func TestColdStartHealthyPhaseUsesPersistedFaultPhaseOutcomes(t *testing.T) {
	tests := []struct {
		name           string
		matchCode      string
		malformed      bool
		wantCordoned   bool
		wantCompletion bool
	}{
		{
			name: "newer fault intentionally skipped", matchCode: "different",
			wantCordoned: true, wantCompletion: true,
		},
		{
			name: "newer fault malformed", matchCode: "48", malformed: true,
			wantCordoned: true, wantCompletion: true,
		},
		{
			name: "newer fault applied", matchCode: "48",
			wantCordoned: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
			oldFailure := &protos.HealthEvent{
				Version: 1, Agent: "agent", ComponentClass: "GPU", CheckName: "check", NodeName: "node-a",
				ErrorCode:        []string{"48"},
				EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "A"}},
			}
			oldHealthy := proto.Clone(oldFailure).(*protos.HealthEvent)
			oldHealthy.IsHealthy = true
			newerFailure := proto.Clone(oldFailure).(*protos.HealthEvent)
			annotation := healthEventsAnnotation.NewHealthEventsAnnotationMap()
			require.True(t, annotation.AddOrUpdateEvent(oldFailure))
			annotationJSON, err := json.Marshal(annotation)
			require.NoError(t, err)

			clientset := fake.NewSimpleClientset(&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node-a",
					Annotations: map[string]string{
						common.QuarantineHealthEventAnnotationKey:           string(annotationJSON),
						common.QuarantineHealthEventIsCordonedAnnotationKey: common.QuarantineHealthEventIsCordonedAnnotationValueTrue,
					},
				},
				Spec: corev1.NodeSpec{Unschedulable: true},
			})
			uncordonObserved := false
			clientset.PrependReactor("patch", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
				patchAction, ok := action.(k8stesting.PatchAction)
				if ok && bytes.Contains(patchAction.GetPatch(), []byte(`"unschedulable":false`)) {
					uncordonObserved = true
				}

				return false, nil, nil
			})
			r := NewReconciler(
				ReconcilerConfig{}, &informer.FaultQuarantineClient{Clientset: clientset}, nil)
			r.SetLabelKeys("nvsentinel.nvidia.com/")
			rule := &recordingErrorCodeEvaluator{name: "rule", code: test.matchCode}
			rulesets := rulesetsConfig{CordonConfigMap: map[string]bool{"rule": true}}
			db := &phaseAwareRecoveryDatabase{}
			watcher := eventwatcher.NewEventWatcher(nil, db, time.Minute, nil)
			watcher.SetProcessEventCallback(func(
				ctx context.Context,
				event *model.HealthEventWithStatus,
			) (*model.Status, error) {
				return r.ProcessEvent(ctx, event, []evaluator.RuleSetEvaluatorIface{rule}, rulesets)
			})
			oldRecord := recoveryStoreRecord(t, "old-healthy", base, oldHealthy)
			newerRecord := recoveryStoreRecord(t, "newer-failure", base.Add(time.Minute), newerFailure)
			if test.malformed {
				newerRecord.RawEvent["healthevent"].(map[string]any)["isHealthy"] = "not-a-bool"
			}
			store := &phaseAwareRecoveryStore{db: db, events: []datastore.HealthEventWithStatus{
				oldRecord, newerRecord,
			}}

			require.NoError(t, coldstart.Handle(ctx, coldstart.Dependencies{
				HealthEventStore: store, EventProcessor: watcher,
				ColdStartAfterTime: base.Add(-time.Minute), ColdStartUntilTime: base.Add(time.Hour),
			}))
			node, err := clientset.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, test.wantCordoned, node.Spec.Unschedulable)
			assert.False(t, uncordonObserved,
				"an older recovery must never uncordon past a newer failed health state")
			_, completed := db.completed["newer-failure"]
			assert.Equal(t, test.wantCompletion, completed)
			if test.wantCordoned {
				finalAnnotation := healthEventsAnnotation.NewHealthEventsAnnotationMap()
				require.NoError(t, json.Unmarshal(
					[]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), finalAnnotation))
				require.Equal(t, 1, finalAnnotation.Count())
			} else {
				assert.NotContains(t, node.Annotations, common.QuarantineHealthEventAnnotationKey)
			}
		})
	}
}

func TestColdStartCompletedNewerHealthyStateStillSupersedesOlderFault(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	oldFailure := &protos.HealthEvent{
		Version: 1, Agent: "agent", ComponentClass: "GPU", CheckName: "check", NodeName: "node-a",
		ErrorCode:        []string{"48"},
		EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "A"}},
	}
	newerHealthy := proto.Clone(oldFailure).(*protos.HealthEvent)
	newerHealthy.IsHealthy = true
	clientset := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
	r := NewReconciler(
		ReconcilerConfig{}, &informer.FaultQuarantineClient{Clientset: clientset}, nil)
	r.SetLabelKeys("nvsentinel.nvidia.com/")
	rule := &recordingErrorCodeEvaluator{name: "rule", code: "48"}
	rulesets := rulesetsConfig{CordonConfigMap: map[string]bool{"rule": true}}
	db := &phaseAwareRecoveryDatabase{completed: map[string]struct{}{"newer-healthy": {}}}
	watcher := eventwatcher.NewEventWatcher(nil, db, time.Minute, nil)
	watcher.SetProcessEventCallback(func(
		ctx context.Context,
		event *model.HealthEventWithStatus,
	) (*model.Status, error) {
		return r.ProcessEvent(ctx, event, []evaluator.RuleSetEvaluatorIface{rule}, rulesets)
	})
	store := &phaseAwareRecoveryStore{db: db, events: []datastore.HealthEventWithStatus{
		recoveryStoreRecord(t, "old-failure", base, oldFailure),
		recoveryStoreRecord(t, "newer-healthy", base.Add(time.Minute), newerHealthy),
	}}

	require.NoError(t, coldstart.Handle(ctx, coldstart.Dependencies{
		HealthEventStore: store, EventProcessor: watcher,
		ColdStartAfterTime: base.Add(-time.Minute), ColdStartUntilTime: base.Add(time.Hour),
	}))
	node, err := clientset.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	assert.False(t, node.Spec.Unschedulable)
	assert.Empty(t, node.Annotations[common.QuarantineHealthEventAnnotationKey])
	assert.Empty(t, rule.seen, "the completed newer healthy state must suppress stale fault evaluation")
	_, oldCompleted := db.completed["old-failure"]
	assert.True(t, oldCompleted)
}

func TestColdStartComplexProjectionUsesBoundedCoverAndProgresses(t *testing.T) {
	const dimension = 9
	ctx := context.Background()
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	oldFailure := &protos.HealthEvent{
		Version: 1, Agent: "agent", ComponentClass: "GPU", CheckName: "check", NodeName: "node-a",
	}
	for index := range dimension {
		oldFailure.EntitiesImpacted = append(oldFailure.EntitiesImpacted, &protos.Entity{
			EntityType: "GPU", EntityValue: fmt.Sprintf("entity-%d", index),
		})
		oldFailure.ErrorCode = append(oldFailure.ErrorCode, fmt.Sprintf("code-%d", index))
	}
	records := []datastore.HealthEventWithStatus{
		recoveryStoreRecord(t, "old-failure", base, oldFailure),
	}
	for index := range dimension {
		recovery := &protos.HealthEvent{
			Version: 1, Agent: "agent", ComponentClass: "GPU", CheckName: "check", NodeName: "node-a",
			IsHealthy: true, ErrorCode: []string{fmt.Sprintf("code-%d", index)},
			EntitiesImpacted: []*protos.Entity{{
				EntityType: "GPU", EntityValue: fmt.Sprintf("entity-%d", index),
			}},
		}
		records = append(records, recoveryStoreRecord(
			t, fmt.Sprintf("recovery-%d", index), base.Add(time.Duration(index+1)*time.Minute), recovery))
	}
	clientset := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
	r := NewReconciler(
		ReconcilerConfig{}, &informer.FaultQuarantineClient{Clientset: clientset}, nil)
	r.SetLabelKeys("nvsentinel.nvidia.com/")
	matchAll := &projectedEventRecorder{name: "all"}
	rulesets := rulesetsConfig{CordonConfigMap: map[string]bool{"all": true}}
	db := &phaseAwareRecoveryDatabase{}
	watcher := eventwatcher.NewEventWatcher(nil, db, time.Minute, nil)
	watcher.SetProcessEventCallback(func(
		ctx context.Context,
		event *model.HealthEventWithStatus,
	) (*model.Status, error) {
		return r.ProcessEvent(ctx, event, []evaluator.RuleSetEvaluatorIface{matchAll}, rulesets)
	})
	store := &phaseAwareRecoveryStore{db: db, events: records}

	err := coldstart.Handle(ctx, coldstart.Dependencies{
		HealthEventStore: store, EventProcessor: watcher,
		ColdStartAfterTime: base.Add(-time.Minute), ColdStartUntilTime: base.Add(time.Hour),
	})
	require.NoError(t, err)
	_, completed := db.completed["old-failure"]
	assert.False(t, completed, "an applied fault records node status rather than a skip completion")
	assert.Equal(t, 1, db.statusWrites, "the source event must receive one durable quarantine status")
	assert.Len(t, matchAll.seen, 1, "overflow must evaluate the original event conservatively")
	node, getErr := clientset.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.True(t, node.Spec.Unschedulable)
	annotation := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	require.NoError(t, json.Unmarshal(
		[]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), annotation))
	assert.Equal(t, dimension*(dimension-1), annotation.Count(),
		"the annotation must retain only exact non-superseded effects")
	for index := range dimension {
		_, obsolete := annotation.Events[healthEventsAnnotation.HealthEventKey{
			Agent: "agent", ComponentClass: "GPU", CheckName: "check", NodeName: "node-a", Version: 1,
			EntityType: "GPU", EntityValue: fmt.Sprintf("entity-%d", index),
			ErrorCode: fmt.Sprintf("code-%d", index),
		}]
		assert.False(t, obsolete, "a superseded diagonal effect must not be persisted")
	}
}

func TestColdStartResidualProjectionReachesRuleEvaluation(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	clientset := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
	})
	fqClient := &informer.FaultQuarantineClient{Clientset: clientset}
	r := NewReconciler(ReconcilerConfig{}, fqClient, nil)
	r.SetLabelKeys("nvsentinel.nvidia.com/")
	code79 := &recordingErrorCodeEvaluator{name: "code-79", code: "79"}
	processor := &reconcilerRecoveryProcessor{
		reconciler: r,
		evaluators: []evaluator.RuleSetEvaluatorIface{code79},
		rulesets: rulesetsConfig{
			CordonConfigMap: map[string]bool{"code-79": true},
		},
	}

	record := func(id string, createdAt time.Time, healthy bool, codes []string) datastore.HealthEventWithStatus {
		event := &protos.HealthEvent{
			Version: 1, Agent: "agent", ComponentClass: "GPU", CheckName: "check", NodeName: "node-a",
			IsHealthy: healthy, ErrorCode: codes,
			EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "A"}},
		}
		encoded, err := json.Marshal(event)
		require.NoError(t, err)
		var eventMap map[string]any
		require.NoError(t, json.Unmarshal(encoded, &eventMap))

		return datastore.HealthEventWithStatus{
			CreatedAt: createdAt,
			RawEvent: datastore.Event{
				"id": id, "healthevent": eventMap, "healtheventstatus": map[string]any{},
			},
		}
	}
	oldFailure := record("old-failure", base, false, []string{"48", "79"})
	newerRecovery := record("newer-recovery", base.Add(time.Minute), true, []string{"79"})
	store := &recoveryHistoryStoreStub{events: []datastore.HealthEventWithStatus{
		oldFailure, newerRecovery,
	}}

	require.NoError(t, coldstart.Handle(ctx, coldstart.Dependencies{
		HealthEventStore: store, EventProcessor: processor,
		ColdStartAfterTime: base.Add(-time.Minute), ColdStartUntilTime: base.Add(time.Hour),
	}))
	assert.Equal(t, [][]string{{"48"}}, code79.seen,
		"an outdated 79 effect must not reach a CEL rule after only 48 remains")
	node, err := clientset.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	assert.False(t, node.Spec.Unschedulable)
	assert.Empty(t, node.Annotations[common.QuarantineHealthEventAnnotationKey])
}

func TestColdStartWithoutSupersessionPreservesCompoundRuleSemantics(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	clientset := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
	})
	fqClient := &informer.FaultQuarantineClient{Clientset: clientset}
	r := NewReconciler(ReconcilerConfig{}, fqClient, nil)
	r.SetLabelKeys("nvsentinel.nvidia.com/")
	bothCodes := &allErrorCodesEvaluator{
		name: "both-codes",
		required: map[string]struct{}{
			"48": {},
			"79": {},
		},
	}
	processor := &reconcilerRecoveryProcessor{
		reconciler: r,
		evaluators: []evaluator.RuleSetEvaluatorIface{bothCodes},
		rulesets: rulesetsConfig{
			CordonConfigMap: map[string]bool{"both-codes": true},
		},
	}
	event := &protos.HealthEvent{
		Version: 1, Agent: "agent", ComponentClass: "GPU", CheckName: "check", NodeName: "node-a",
		ErrorCode: []string{"48", "79"},
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "A"},
			{EntityType: "GPU", EntityValue: "B"},
		},
	}
	encoded, err := json.Marshal(event)
	require.NoError(t, err)
	var eventMap map[string]any
	require.NoError(t, json.Unmarshal(encoded, &eventMap))
	record := datastore.HealthEventWithStatus{
		CreatedAt: base,
		RawEvent: datastore.Event{
			"id": "compound", "healthevent": eventMap, "healtheventstatus": map[string]any{},
		},
	}
	store := &recoveryHistoryStoreStub{events: []datastore.HealthEventWithStatus{record}}

	require.NoError(t, coldstart.Handle(ctx, coldstart.Dependencies{
		HealthEventStore: store, EventProcessor: processor,
		ColdStartAfterTime: base.Add(-time.Minute), ColdStartUntilTime: base.Add(time.Hour),
	}))
	assert.Equal(t, [][]string{{"48", "79"}}, bothCodes.seen,
		"an event with no outdated effects must reach CEL unchanged")
	node, err := clientset.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, node.Spec.Unschedulable)
	assert.NotEmpty(t, node.Annotations[common.QuarantineHealthEventAnnotationKey])
}

func TestColdStartPartialSupersessionPreservesResidualCompoundRuleSemantics(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	clientset := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
	})
	fqClient := &informer.FaultQuarantineClient{Clientset: clientset}
	r := NewReconciler(ReconcilerConfig{}, fqClient, nil)
	r.SetLabelKeys("nvsentinel.nvidia.com/")
	bothCodes := &allErrorCodesEvaluator{
		name: "both-codes",
		required: map[string]struct{}{
			"48": {},
			"79": {},
		},
	}
	processor := &reconcilerRecoveryProcessor{
		reconciler: r,
		evaluators: []evaluator.RuleSetEvaluatorIface{bothCodes},
		rulesets: rulesetsConfig{
			CordonConfigMap: map[string]bool{"both-codes": true},
		},
	}
	record := func(
		id string,
		createdAt time.Time,
		healthy bool,
		entities []*protos.Entity,
	) datastore.HealthEventWithStatus {
		event := &protos.HealthEvent{
			Version: 1, Agent: "agent", ComponentClass: "GPU", CheckName: "check", NodeName: "node-a",
			IsHealthy: healthy, ErrorCode: []string{"48", "79"}, EntitiesImpacted: entities,
		}
		encoded, err := json.Marshal(event)
		require.NoError(t, err)
		var eventMap map[string]any
		require.NoError(t, json.Unmarshal(encoded, &eventMap))

		return datastore.HealthEventWithStatus{
			CreatedAt: createdAt,
			RawEvent: datastore.Event{
				"id": id, "healthevent": eventMap, "healtheventstatus": map[string]any{},
			},
		}
	}
	entitiesAB := []*protos.Entity{
		{EntityType: "GPU", EntityValue: "A"},
		{EntityType: "GPU", EntityValue: "B"},
	}
	oldFailure := record("old-failure", base, false, entitiesAB)
	newerRecovery := record("newer-recovery", base.Add(time.Minute), true,
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "A"}})
	// Only A/79 is newer. Keep A/48 plus both B effects.
	newerRecovery.RawEvent["healthevent"].(map[string]any)["errorCode"] = []any{"79"}
	store := &recoveryHistoryStoreStub{events: []datastore.HealthEventWithStatus{
		oldFailure, newerRecovery,
	}}

	require.NoError(t, coldstart.Handle(ctx, coldstart.Dependencies{
		HealthEventStore: store, EventProcessor: processor,
		ColdStartAfterTime: base.Add(-time.Minute), ColdStartUntilTime: base.Add(time.Hour),
	}))
	assert.Contains(t, bothCodes.seen, []string{"48", "79"},
		"B retains both codes and must reach a compound CEL rule as one coherent residual event")
	node, err := clientset.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, node.Spec.Unschedulable)
	assert.NotEmpty(t, node.Annotations[common.QuarantineHealthEventAnnotationKey])
}

func TestColdStartProjectionTransientFailureAppliesNoPartialActions(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	transientErr := errors.New("temporary projected evaluation failure")
	clientset := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
	})
	fqClient := &informer.FaultQuarantineClient{Clientset: clientset}
	r := NewReconciler(ReconcilerConfig{}, fqClient, nil)
	r.SetLabelKeys("nvsentinel.nvidia.com/")
	flaky := &flakyProjectedEvaluator{name: "projected", failCode: "79", err: transientErr}
	processor := &reconcilerRecoveryProcessor{
		reconciler: r,
		evaluators: []evaluator.RuleSetEvaluatorIface{flaky},
		rulesets: rulesetsConfig{
			CordonConfigMap: map[string]bool{"projected": true},
		},
	}
	record := func(
		id string,
		createdAt time.Time,
		healthy bool,
		codes []string,
		entities []*protos.Entity,
	) datastore.HealthEventWithStatus {
		event := &protos.HealthEvent{
			Version: 1, Agent: "agent", ComponentClass: "GPU", CheckName: "check", NodeName: "node-a",
			IsHealthy: healthy, ErrorCode: codes, EntitiesImpacted: entities,
		}
		encoded, err := json.Marshal(event)
		require.NoError(t, err)
		var eventMap map[string]any
		require.NoError(t, json.Unmarshal(encoded, &eventMap))

		return datastore.HealthEventWithStatus{
			CreatedAt: createdAt,
			RawEvent: datastore.Event{
				"id": id, "healthevent": eventMap, "healtheventstatus": map[string]any{},
			},
		}
	}
	entitiesAB := []*protos.Entity{
		{EntityType: "GPU", EntityValue: "A"},
		{EntityType: "GPU", EntityValue: "B"},
	}
	oldFailure := record("old-failure", base, false, []string{"48", "79"}, entitiesAB)
	newerRecovery := record("newer-recovery", base.Add(time.Minute), true, []string{"79"},
		[]*protos.Entity{{EntityType: "GPU", EntityValue: "A"}})
	store := &recoveryHistoryStoreStub{events: []datastore.HealthEventWithStatus{
		oldFailure, newerRecovery,
	}}
	deps := coldstart.Dependencies{
		HealthEventStore: store, EventProcessor: processor,
		ColdStartAfterTime: base.Add(-time.Minute), ColdStartUntilTime: base.Add(time.Hour),
	}

	err := coldstart.Handle(ctx, deps)
	require.ErrorIs(t, err, transientErr)
	node, err := clientset.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	assert.False(t, node.Spec.Unschedulable)
	assert.Empty(t, node.Annotations[common.QuarantineHealthEventAnnotationKey],
		"a successful projection must not mutate the node before every projection evaluates")

	flaky.err = nil
	require.NoError(t, coldstart.Handle(ctx, deps))
	node, err = clientset.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, node.Spec.Unschedulable)
	annotation := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	require.NoError(t, json.Unmarshal(
		[]byte(node.Annotations[common.QuarantineHealthEventAnnotationKey]), annotation))
	require.Equal(t, 3, annotation.Count())
	_, obsoleteExists := annotation.Events[healthEventsAnnotation.HealthEventKey{
		Agent: "agent", ComponentClass: "GPU", CheckName: "check", NodeName: "node-a",
		Version: 1, EntityType: "GPU", EntityValue: "A", ErrorCode: "79",
	}]
	assert.False(t, obsoleteExists)
}
