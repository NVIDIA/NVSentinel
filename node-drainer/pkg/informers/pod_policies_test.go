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

package informers

import (
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestPodPolicyLabelsSurviveInformerTransform(t *testing.T) {
	pod := richDrainEligiblePod("workload", "worker", "node-a")
	pod.Labels = map[string]string{"drain": "immediate", "team": "training", "unrelated": "discard"}
	transform := excludedPodTransform(regexp.MustCompile(`^kube-system$`), "drain", "team", "absent")
	obj, err := transform(pod)
	require.NoError(t, err)
	cached := obj.(*v1.Pod)
	require.Equal(t, map[string]string{"drain": "immediate", "team": "training"}, cached.Labels)
	cached.Labels["drain"] = "completion"
	require.Equal(t, "immediate", pod.Labels["drain"], "cache must not alias the API object's labels")

	pod.Namespace = "kube-system"
	obj, err = transform(pod)
	require.NoError(t, err)
	require.Empty(t, obj.(*v1.Pod).Labels)
	require.Empty(t, obj.(*v1.Pod).Spec.NodeName, "excluded pods must stay out of node indexes")
}

// Exercise the API server's preconditions, including the eviction subresource.
// A fake client would not prove that Kubernetes protects a newly selected pod.
func TestStalePodObservationCannotEvictOrDelete(t *testing.T) {
	environment := &envtest.Environment{}
	cfg, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	client, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	ctx := t.Context()
	i := &Informers{clientset: client}
	for _, operation := range []string{"evict", "delete"} {
		t.Run(operation, func(t *testing.T) {
			pod := &v1.Pod{Name: operation, Namespace: "default", Labels: map[string]string{"mode": "immediate"},
				Spec: v1.PodSpec{NodeName: "node-a", Containers: []v1.Container{{Name: "workload", Image: "busybox"}}}}
			observed, createErr := client.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{})
			require.NoError(t, createErr)
			updated := observed.DeepCopy()
			updated.Labels["mode"] = "completion"
			_, updateErr := client.CoreV1().Pods("default").Update(ctx, updated, metav1.UpdateOptions{})
			require.NoError(t, updateErr)
			act := func() error {
				if operation == "evict" {
					return i.sendEvictionRequestForPod(ctx, "default", time.Second, observed)
				}
				return i.forceDeletePods(ctx, []*v1.Pod{observed})
			}
			actionErr := act()
			require.Error(t, actionErr, "stale labels must cause an API conflict")
			require.True(t, apierrors.IsConflict(actionErr), "expected an API conflict, got %v", actionErr)
			current, getErr := client.CoreV1().Pods("default").Get(ctx, pod.Name, metav1.GetOptions{})
			require.NoError(t, getErr)
			require.Nil(t, current.DeletionTimestamp)
			require.Equal(t, "completion", current.Labels["mode"])

			require.NoError(t, client.CoreV1().Pods("default").Delete(ctx, pod.Name,
				metav1.DeleteOptions{GracePeriodSeconds: new(int64(0))}))
			require.Eventually(t, func() bool {
				_, err := client.CoreV1().Pods("default").Get(ctx, pod.Name, metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}, time.Second, 10*time.Millisecond)
			replacement, createErr := client.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{})
			require.NoError(t, createErr)
			require.NotEqual(t, observed.UID, replacement.UID)
			actionErr = act()
			require.Error(t, actionErr, "an observation of the previous pod must not delete its replacement")
			require.True(t, apierrors.IsConflict(actionErr), "expected an API conflict, got %v", actionErr)
			current, getErr = client.CoreV1().Pods("default").Get(ctx, pod.Name, metav1.GetOptions{})
			require.NoError(t, getErr)
			require.Equal(t, replacement.UID, current.UID)
			require.Nil(t, current.DeletionTimestamp)
		})
	}
}
