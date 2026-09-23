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
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
)

func annotationRule() config.HealthEventsAnalyzerRule {
	rule := recoveryRule(config.RecoveryScopeEntity)
	rule.Recovery = &config.RecoveryMapping{AnnotationKey: "nvsentinel.nvidia.com/recover-xid", Scope: config.RecoveryScopeEntity, EntityTypes: []string{"GPU_UUID"}}
	return rule
}

func TestParseAnnotationRecovery_ValidatesScopeAndTime(t *testing.T) {
	now := time.Now().UTC()
	timestamp := now.Add(-time.Second).Format(time.RFC3339Nano)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: "uid-a", CreationTimestamp: metav1.NewTime(now.Add(-time.Hour))}}
	cases := []struct {
		name, value string
		invalid     bool
		entities    int
	}{
		{name: "node-wide", value: timestamp},
		{name: "one GPU", value: `{"recoveredAt":"` + timestamp + `","entities":[{"entityType":"GPU_UUID","entityValue":"GPU-a"}]}`, entities: 1},
		{name: "missing time", value: `{}`, invalid: true},
		{name: "future", value: now.Add(time.Hour).Format(time.RFC3339Nano), invalid: true},
		{name: "old node", value: now.Add(-2 * time.Hour).Format(time.RFC3339Nano), invalid: true},
		{name: "unknown field", value: `{"recoveredAt":"` + timestamp + `","all":true}`, invalid: true},
		{name: "trailing document", value: `{"recoveredAt":"` + timestamp + `"} {}`, invalid: true},
		{name: "unknown entity", value: `{"recoveredAt":"` + timestamp + `","entities":[{"entityType":"GPU","entityValue":"0"}]}`, invalid: true},
		{name: "empty entity", value: `{"recoveredAt":"` + timestamp + `","entities":[{"entityType":"GPU_UUID","entityValue":" "}]}`, invalid: true},
		{name: "multiple GPUs", value: `{"recoveredAt":"` + timestamp + `","entities":[{"entityType":"GPU_UUID","entityValue":"a"},{"entityType":"GPU_UUID","entityValue":"b"}]}`, invalid: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source, err := parseAnnotationRecovery(node, annotationRule(), tc.value, now)
			if tc.invalid {
				require.Error(t, err)
				require.Nil(t, source)
				return
			}
			require.NoError(t, err)
			require.Len(t, source.HealthEvent.EntitiesImpacted, tc.entities)
			require.Equal(t, string(node.UID), source.HealthEvent.Metadata[annotationNodeUIDKey])
			again, err := parseAnnotationRecovery(node, annotationRule(), tc.value, now.Add(time.Second))
			require.NoError(t, err)
			require.Equal(t, source.HealthEvent.Metadata[annotationRequestKey], again.HealthEvent.Metadata[annotationRequestKey])
		})
	}
	rule := annotationRule()
	rule.Recovery.EntityTypes = []string{"GPU_UUID", "PCI"}
	request := annotationRecoveryRequest{RecoveredAt: timestamp, Entities: []annotationRecoveryEntity{{EntityType: "GPU_UUID", EntityValue: "a"}, {EntityType: "GPU_UUID", EntityValue: "a"}}}
	data, err := json.Marshal(request)
	require.NoError(t, err)
	_, err = parseAnnotationRecovery(node, rule, string(data), now)
	require.ErrorContains(t, err, "duplicate")
}

func TestNodeProcessingLocks_SerializesOneNodeAndReleasesCanceledWaiters(t *testing.T) {
	var locks nodeProcessingLocks
	unlock, err := locks.acquire(t.Context(), "node-a")
	require.NoError(t, err)
	other, err := locks.acquire(t.Context(), "node-b")
	require.NoError(t, err)
	other()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = locks.acquire(ctx, "node-a")
	require.ErrorIs(t, err, context.Canceled)
	completed := make(chan struct{})
	go func() {
		release, err := locks.acquire(t.Context(), "node-a")
		if err == nil {
			release()
			close(completed)
		}
	}()
	select {
	case <-completed:
		t.Fatal("same node entered while lock held")
	default:
	}
	unlock()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("same node remained locked")
	}
	locks.mu.Lock()
	defer locks.mu.Unlock()
	require.Empty(t, locks.nodes)
}
