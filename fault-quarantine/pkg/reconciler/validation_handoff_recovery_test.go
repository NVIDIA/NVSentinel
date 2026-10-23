// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
)

func TestValidationHandoffRecovery_RetriesWithoutNodeChangesUntilConsumed(t *testing.T) {
	clock := clocktesting.NewFakeClock(time.Now())
	queue := workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.DefaultTypedControllerRateLimiter[string](),
		workqueue.TypedRateLimitingQueueConfig[string]{Clock: clock},
	)
	defer queue.ShutDown()
	calls := 0
	recovery := &validationHandoffRecovery{queue: queue, reconcile: func(context.Context, string) (bool, error) {
		calls++
		switch calls {
		case 1:
			return true, errors.New("temporary API outage")
		case 2:
			return true, nil // Request exists, but can still be deleted before registration.
		default:
			return false, nil // Consumer registered and consumed the source.
		}
	}}
	recovery.enqueue(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "handoff-node", Annotations: map[string]string{
		common.QuarantineValidationHealthEventAnnotationKey: "source",
	}}})
	require.True(t, recovery.processNext(t.Context()))
	require.Equal(t, 1, calls)
	require.Equal(t, 1, queue.NumRequeues("handoff-node"))
	for i := 0; i < 2; i++ {
		// The delaying queue holds its heartbeat plus the scheduled retry timer.
		require.Eventually(t, func() bool { return clock.Waiters() >= 2 }, time.Second, time.Millisecond)
		clock.Step(time.Minute)
		require.Eventually(t, func() bool { return queue.Len() == 1 }, time.Second, time.Millisecond)
		require.True(t, recovery.processNext(t.Context()))
	}
	require.Equal(t, 3, calls)
	require.Zero(t, queue.NumRequeues("handoff-node"))
	clock.Step(time.Hour)
	require.Never(t, func() bool { return queue.Len() > 0 }, 25*time.Millisecond, time.Millisecond)
}

func TestValidationHandoffRecovery_OnlyQueuesCompletedProducerHandoffs(t *testing.T) {
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
	defer queue.ShutDown()
	recovery := &validationHandoffRecovery{queue: queue}
	recovery.enqueue(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "healthy"}})
	recovery.enqueue(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "quarantined", Annotations: map[string]string{
		common.QuarantineValidationHealthEventAnnotationKey: "source",
		common.QuarantineHealthEventAnnotationKey:           "fault",
	}}})
	require.Zero(t, queue.Len())
	recovery.enqueue(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "handoff", Annotations: map[string]string{
		common.QuarantineValidationHealthEventAnnotationKey: "source",
	}}})
	require.Equal(t, 1, queue.Len())
}
