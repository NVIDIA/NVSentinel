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
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/publisher"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

func TestAnnotationRecoveryWithRealProvider(t *testing.T) {
	if os.Getenv(recoveryIntegrationEnv) != "1" {
		t.Skipf("set %s=1 with a real provider configuration", recoveryIntegrationEnv)
	}
	server := &envtest.Environment{}
	restConfig, err := server.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Stop()) })
	kube, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dsConfig, err := datastore.LoadDatastoreConfig()
	require.NoError(t, err)
	ds, err := datastore.NewDataStore(ctx, *dsConfig)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ds.Close(context.Background())) })
	adapter, ok := ds.(interface{ GetDatabaseClient() client.DatabaseClient })
	require.True(t, ok)
	database := adapter.GetDatabaseClient()
	nodeName := fmt.Sprintf("annotation-provider-%d", time.Now().UnixNano())
	_, err = kube.CoreV1().Nodes().Create(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	rule := annotationRule()
	rule.Stage = []string{
		`{"$match":{"healthevent.checkname":"SysLogsXIDError","healthevent.ishealthy":false}}`,
		`{"$count":"count"}`,
		`{"$match":{"count":{"$gte":2}}}`,
	}
	eventAt := func(at time.Time) datamodels.HealthEventWithStatus {
		return storedEvent(time.Now().UTC(), &protos.HealthEvent{
			Agent: "syslog-health-monitor", CheckName: "SysLogsXIDError",
			NodeName: nodeName, ComponentClass: "GPU", Version: 1,
			EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU_UUID", EntityValue: "GPU-a"}},
			GeneratedTimestamp: timestamppb.New(at),
			ProcessingStrategy: protos.ProcessingStrategy_EXECUTE_REMEDIATION,
		})
	}
	insertHealthEvents(t, ctx, database, eventAt(time.Now().Add(-time.Minute)), eventAt(time.Now().Add(-30*time.Second)))
	insertHealthEvents(t, ctx, database, activeAnnotationFault(nodeName, "GPU-a", time.Now().Add(-time.Second)))
	sink := &integrationStoreSink{database: database, results: make(chan error, 8), drop: map[int32]bool{1: true}}
	newReconciler := func() *Reconciler {
		return &Reconciler{
			config: HealthEventsAnalyzerReconcilerConfig{
				HealthEventsAnalyzerRules: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{rule}},
				Publisher:                 publisher.NewPublisher(sink, protos.ProcessingStrategy_EXECUTE_REMEDIATION),
			},
			databaseClient: database, provider: ds.Provider(),
			recoveryPoll: 2 * time.Millisecond, recoveryRepublish: 100 * time.Millisecond,
		}
	}
	reconciler := newReconciler()
	stop := startAnnotationTestController(t, kube, reconciler)
	verifiedAt := time.Now().UTC()
	request := verifiedAt.Format(time.RFC3339Nano)
	setRecoveryAnnotation(t, kube, nodeName, request)
	requireAnnotationRemoved(t, kube, nodeName)
	require.NoError(t, <-sink.results)
	require.EqualValues(t, 2, sink.calls.Load(), "accepted but lost clear must be republished")
	requireStoredDerivedState(t, ctx, reconciler, rule, nodeName, "GPU-a", true)
	stop()

	restarted := newReconciler()
	startAnnotationTestController(t, kube, restarted)
	next := eventAt(time.Now().UTC())
	boundary, err := restarted.recoveryBoundaryForEvent(ctx, rule, next.HealthEvent)
	require.NoError(t, err)
	require.NotNil(t, boundary)
	require.True(t, verifiedAt.Equal(boundary.generated.AsTime()), "the boundary must survive annotation removal and restart")
	// A delayed pre-verification record is stored after recovery. Neither it nor
	// the original history may contribute to the next threshold evaluation.
	insertHealthEvents(t, ctx, database, eventAt(verifiedAt.Add(-time.Second)), next)
	published, err := restarted.handleEvent(ctx, &next)
	require.NoError(t, err)
	require.False(t, published, "one new error must not combine with pre-recovery history")
	requireStoredDerivedState(t, ctx, restarted, rule, nodeName, "GPU-a", true)
	next = eventAt(time.Now().UTC())
	insertHealthEvents(t, ctx, database, next)
	published, err = restarted.handleEvent(ctx, &next)
	require.NoError(t, err)
	require.True(t, published, "two new errors must reactivate the condition")
	require.NoError(t, <-sink.results)
	requireStoredDerivedState(t, ctx, restarted, rule, nodeName, "GPU-a", false)
	setRecoveryAnnotation(t, kube, nodeName, request)
	requireAnnotationRemoved(t, kube, nodeName)
	require.EqualValues(t, 3, sink.calls.Load(), "stale verification must not clear the new fault")
}
