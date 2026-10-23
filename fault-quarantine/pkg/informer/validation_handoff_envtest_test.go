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

package informer

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nvidia/nvsentinel/commons/pkg/cordonownership"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
)

func TestCompleteValidationHandoff_ConflictsRecheckQuarantine(t *testing.T) {
	for _, tc := range []struct {
		name      string
		changeKey string
	}{
		{"unrelated-label", ""},
		{"new-source", common.QuarantineValidationHealthEventAnnotationKey},
		{"new-claim", cordonownership.AnnotationKey},
		{"preserved-claim", cordonownership.AnnotationKey},
		{"new-quarantine", common.QuarantineHealthEventAnnotationKey},
		{"new-cordon-owner", common.QuarantineHealthEventCordonPreExistingAnnotationKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			name := "handoff-conflict-" + tc.name
			annotations := ownershipQuarantineAnnotations()
			annotations[common.QuarantineHealthEventAnnotationKey] = `{"events":{"recovered-fault":{}}}`
			annotations[common.QuarantineValidationHealthEventAnnotationKey] = "original-source"
			createTestNode(ctx, t, name, annotations, nil, nil, true)
			expected, err := testClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
			require.NoError(t, err)
			if tc.name == "preserved-claim" {
				_, err = cordonownership.Store(expected, cordonownership.New(expected))
				require.NoError(t, err)
				expected, err = testClient.CoreV1().Nodes().Update(ctx, expected, metav1.UpdateOptions{})
				require.NoError(t, err)
			}

			config := rest.CopyConfig(testConfig)
			var once sync.Once
			var injectedErr error
			patches := 0
			config.Wrap(func(delegate http.RoundTripper) http.RoundTripper {
				return roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					if request.Method == http.MethodPatch {
						patches++
						once.Do(func() {
							node, err := testClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
							if err != nil {
								injectedErr = err
								return
							}
							if tc.changeKey == "" {
								node.Labels = map[string]string{"example.com/concurrent": "preserved"}
							} else if tc.name == "preserved-claim" {
								claim, _ := cordonownership.Read(node)
								claim.PreserveCordon = true
								if _, err := cordonownership.Store(node, claim); err != nil {
									injectedErr = err
									return
								}
							} else {
								node.Annotations[tc.changeKey] = "new-value"
							}
							_, injectedErr = testClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
						})
						if injectedErr != nil {
							return nil, injectedErr
						}
					}
					return delegate.RoundTrip(request)
				})
			})
			clientset, err := kubernetes.NewForConfig(config)
			require.NoError(t, err)
			client := &FaultQuarantineClient{Clientset: clientset}
			err = client.CompleteValidationHandoff(ctx, expected, []string{
				common.QuarantineHealthEventAnnotationKey, common.QuarantineHealthEventIsCordonedAnnotationKey,
				common.QuarantineValidationHealthEventAnnotationKey,
			}, nil)
			// A consumer may mark the claim preserved while it registers the request;
			// that does not change the claim identity, so the handoff still completes.
			tolerated := tc.changeKey == "" || tc.name == "preserved-claim"
			if tolerated {
				require.NoError(t, err)
				require.Equal(t, 2, patches)
			} else {
				require.Error(t, err)
				require.Equal(t, 1, patches, "changed quarantine must be rejected before a second patch")
			}
			node, err := testClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
			require.NoError(t, err)
			require.True(t, node.Spec.Unschedulable, "handoff must not uncordon")
			require.Contains(t, node.Annotations, common.QuarantineValidationHealthEventAnnotationKey)
			if tc.changeKey == "" {
				require.NotContains(t, node.Annotations, common.QuarantineHealthEventAnnotationKey)
				require.Equal(t, "preserved", node.Labels["example.com/concurrent"])
			} else if tc.name == "preserved-claim" {
				claim, valid := cordonownership.Read(node)
				require.True(t, valid)
				require.True(t, claim.PreserveCordon)
				require.NotContains(t, node.Annotations, common.QuarantineHealthEventAnnotationKey)
			} else {
				require.Equal(t, "new-value", node.Annotations[tc.changeKey])
				require.Contains(t, node.Annotations, common.QuarantineHealthEventAnnotationKey)
			}
		})
	}
}

func TestCompleteValidationHandoff_PreservesConcurrentConsumerSession(t *testing.T) {
	for _, tc := range []struct {
		name        string
		method      string
		wantPatches int
	}{
		{"before-get", http.MethodGet, 1},
		{"before-patch", http.MethodPatch, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			name := "handoff-consumer-session-" + tc.name
			node, claim := claimedNode(t, name)
			node.Annotations[common.QuarantineHealthEventAnnotationKey] = claimQuarantine
			node.Annotations[common.QuarantineValidationHealthEventAnnotationKey] = claimSource
			expected, err := testClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
			require.NoError(t, err)
			session, err := json.Marshal([]map[string]any{{
				"name": "validation-b", "tests": []string{"quick"}, "nodeUID": node.UID,
				"requestUID": "request-b", "claimID": claim.ID, "cordonOwnership": "owned",
			}})
			require.NoError(t, err)

			config := rest.CopyConfig(testConfig)
			var once sync.Once
			var injectedErr error
			patches := 0
			config.Wrap(func(delegate http.RoundTripper) http.RoundTripper {
				return roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					if request.Method == http.MethodPatch {
						patches++
					}
					if request.Method == tc.method {
						once.Do(func() {
							consumer := expected.DeepCopy()
							consumer.Annotations[cordonownership.ValidationSessionAnnotationKey] = string(session)
							_, injectedErr = testClient.CoreV1().Nodes().Update(ctx, consumer, metav1.UpdateOptions{})
						})
						if injectedErr != nil {
							return nil, injectedErr
						}
					}
					return delegate.RoundTrip(request)
				})
			})
			clientset, err := kubernetes.NewForConfig(config)
			require.NoError(t, err)
			client := &FaultQuarantineClient{Clientset: clientset}
			require.NoError(t, client.CompleteValidationHandoff(ctx, expected, common.QuarantineAnnotationKeys, nil))
			require.Equal(t, tc.wantPatches, patches)

			actual, err := testClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
			require.NoError(t, err)
			require.NotContains(t, actual.Annotations, common.QuarantineHealthEventAnnotationKey)
			require.Equal(t, string(session), actual.Annotations[cordonownership.ValidationSessionAnnotationKey])
			require.Equal(t, expected.Annotations[cordonownership.AnnotationKey], actual.Annotations[cordonownership.AnnotationKey])
			require.Equal(t, claimSource, actual.Annotations[common.QuarantineValidationHealthEventAnnotationKey])
			require.True(t, actual.Spec.Unschedulable)
		})
	}
}

func TestCompleteValidationHandoff_RejectsRecreatedNode(t *testing.T) {
	ctx := t.Context()
	const name = "handoff-recreated-node"
	annotations := ownershipQuarantineAnnotations()
	annotations[common.QuarantineValidationHealthEventAnnotationKey] = "source"
	createTestNode(ctx, t, name, annotations, nil, nil, true)
	expected, err := testClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, testClient.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{}))
	replacement, err := testClient.CoreV1().Nodes().Create(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations},
		Spec:       corev1.NodeSpec{Unschedulable: true},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NotEqual(t, expected.UID, replacement.UID)
	client := &FaultQuarantineClient{Clientset: testClient}
	require.Error(t, client.CompleteValidationHandoff(ctx, expected, []string{common.QuarantineHealthEventAnnotationKey}, nil))
	node, err := testClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, replacement.Annotations, node.Annotations)
	require.True(t, node.Spec.Unschedulable)
}

func TestCompleteValidationHandoff_RejectsUnresolvedSnapshot(t *testing.T) {
	ctx := t.Context()
	const name = "handoff-unresolved-node"
	annotations := ownershipQuarantineAnnotations()
	annotations[common.QuarantineHealthEventAnnotationKey] = `{"events":{"new-fault":{}}}`
	annotations[common.QuarantineValidationHealthEventAnnotationKey] = "unchanged-source"
	createTestNode(ctx, t, name, annotations, nil, nil, true)
	live, err := testClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	// A snapshot that still shows the recovered quarantine cannot hand off the new one.
	expected := live.DeepCopy()
	expected.Annotations[common.QuarantineHealthEventAnnotationKey] = `{"events":{"recovered-fault":{}}}`
	client := &FaultQuarantineClient{Clientset: testClient}
	require.Error(t, client.CompleteValidationHandoff(ctx, expected, []string{common.QuarantineHealthEventAnnotationKey}, nil))
	node, err := testClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, live.Annotations, node.Annotations)
	require.True(t, node.Spec.Unschedulable)
}
