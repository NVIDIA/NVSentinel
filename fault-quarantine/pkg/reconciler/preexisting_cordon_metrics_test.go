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
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
)

// Both cases catch counting a skipped cordon. The conflict case also catches a
// success flag that is not reset when UpdateNode invokes its callback again.
func TestCordonAfterPreExistingCordonRemoved_SessionEnded_DoesNotCountCordon(t *testing.T) {
	for _, tc := range []struct {
		name           string
		endDuringPatch bool
	}{
		{name: "before_update"},
		{name: "after_conflict", endDuringPatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
			defer cancel()

			node := &corev1.Node{
				Name: "cordon-metrics-" + generateShortTestID(),
				Labels: map[string]string{
					"example.com/owner": "operator",
				},
				Annotations: map[string]string{
					"example.com/note": "keep",
				},
			}
			if tc.endDuringPatch {
				node.Annotations[common.QuarantineHealthEventAnnotationKey] = "{}"
				node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey] = "True"
				node.Annotations[common.QuarantineHealthEventCordonPreExistingAnnotationKey] = "True"
			}

			expected, err := e2eTestClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, e2eTestClient.CoreV1().Nodes().Delete(e2eTestContext, node.Name, metav1.DeleteOptions{}))
			})

			var patchStatuses []int

			restConfig := rest.CopyConfig(e2eTestEnv.Config)
			restConfig.Wrap(func(delegate http.RoundTripper) http.RoundTripper {
				return cordonMetricsRoundTripper(func(req *http.Request) (*http.Response, error) {
					if req.Method != http.MethodPatch || req.URL.Path != "/api/v1/nodes/"+node.Name {
						return delegate.RoundTrip(req)
					}

					if tc.endDuringPatch && len(patchStatuses) == 0 {
						// End the session after the callback has prepared its patch. The
						// real API server rejects that patch's stale resourceVersion.
						released := expected.DeepCopy()
						delete(released.Annotations, common.QuarantineHealthEventAnnotationKey)
						delete(released.Annotations, common.QuarantineHealthEventIsCordonedAnnotationKey)
						delete(released.Annotations, common.QuarantineHealthEventCordonPreExistingAnnotationKey)

						updated, err := e2eTestClient.CoreV1().Nodes().Update(req.Context(), released, metav1.UpdateOptions{})
						if err != nil {
							return nil, err
						}

						expected = updated
					}

					response, err := delegate.RoundTrip(req)
					if response != nil {
						patchStatuses = append(patchStatuses, response.StatusCode)
					}

					return response, err
				})
			})

			clientset, err := kubernetes.NewForConfig(restConfig)
			require.NoError(t, err)

			r := NewReconciler(ReconcilerConfig{}, &informer.FaultQuarantineClient{Clientset: clientset}, nil)
			r.SetLabelKeys("k8s.nvidia.com/")

			beforeCordonsApplied := getCounterValue(t, metrics.CordonsApplied)
			require.NoError(t, r.cordonAfterPreExistingCordonRemoved(ctx, node.Name))

			if tc.endDuringPatch {
				require.Equal(t, []int{http.StatusConflict}, patchStatuses, "the real patch must conflict before retrying")
			} else {
				require.Empty(t, patchStatuses, "an ended session must not send a patch")
			}

			actual, err := e2eTestClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.False(t, actual.Spec.Unschedulable, "the released node must remain schedulable")
			assert.Equal(t, expected.ResourceVersion, actual.ResourceVersion, "the skipped cordon must not write the node")
			assert.Equal(t, expected.Labels, actual.Labels)
			assert.Equal(t, expected.Annotations, actual.Annotations)
			assert.Equal(t, beforeCordonsApplied, getCounterValue(t, metrics.CordonsApplied),
				"CordonsApplied must not count a cordon skipped after the session ended")
		})
	}
}

type cordonMetricsRoundTripper func(*http.Request) (*http.Response, error)

func (f cordonMetricsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
