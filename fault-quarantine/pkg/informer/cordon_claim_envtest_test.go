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

const claimQuarantine = `[{"id":"fault-a","agent":"gpu-health-monitor","checkName":"gpu","nodeName":"test-node"}]`
const claimSource = `[{"id":"fault-a","tests":["quick"]}]`

func claimedNode(t *testing.T, name string) (*corev1.Node, cordonownership.Claim) {
	t.Helper()
	createTestNode(t.Context(), t, name, nil, nil, nil, true)
	node, err := testClient.CoreV1().Nodes().Get(t.Context(), name, metav1.GetOptions{})
	require.NoError(t, err)
	claim := cordonownership.New(node)
	_, err = cordonownership.Store(node, claim)
	require.NoError(t, err)
	return node, claim
}

func ownedClaimSession(t *testing.T, node *corev1.Node, claim cordonownership.Claim) string {
	t.Helper()
	data, err := json.Marshal([]map[string]any{{"nodeUID": node.UID, "requestUID": "request-a", "claimID": claim.ID, "cordonOwnership": "owned"}})
	require.NoError(t, err)
	return string(data)
}

func TestQuarantineClaim_InheritsOnlyActiveOwnership(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(*corev1.Node, cordonownership.Claim)
		wantOwned bool
	}{
		{"pending", func(n *corev1.Node, _ cordonownership.Claim) {
			n.Annotations[cordonownership.PendingHandoffAnnotationKey] = claimSource
		}, true},
		{"session", func(n *corev1.Node, c cordonownership.Claim) {
			n.Annotations[cordonownership.ValidationSessionAnnotationKey] = ownedClaimSession(t, n, c)
		}, true},
		{"quarantine", func(n *corev1.Node, _ cordonownership.Claim) {
			n.Annotations[cordonownership.QuarantineAnnotationKey] = claimQuarantine
			n.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey] = "True"
		}, true},
		{"detached", func(_ *corev1.Node, _ cordonownership.Claim) {}, false},
		{"empty-source", func(n *corev1.Node, _ cordonownership.Claim) {
			n.Annotations[cordonownership.PendingHandoffAnnotationKey] = "[]"
		}, false},
		{"unknown-session", func(n *corev1.Node, _ cordonownership.Claim) {
			n.Annotations[cordonownership.PendingHandoffAnnotationKey] = claimSource
			n.Annotations[cordonownership.ValidationSessionAnnotationKey] = `[{"cordonOwnership":"unknown"}]`
		}, false},
		{"preserved", func(n *corev1.Node, c cordonownership.Claim) {
			n.Annotations[cordonownership.PendingHandoffAnnotationKey] = claimSource
			c.PreserveCordon = true
			_, err := cordonownership.Store(n, c)
			require.NoError(t, err)
		}, false},
		{"different-node", func(n *corev1.Node, _ cordonownership.Claim) {
			n.Annotations[cordonownership.PendingHandoffAnnotationKey] = claimSource
			n.Annotations[cordonownership.AnnotationKey] = `{"id":"old","nodeUID":"old-node"}`
		}, false},
		{"manual", func(n *corev1.Node, _ cordonownership.Claim) {
			n.Annotations[cordonownership.QuarantineAnnotationKey] = claimQuarantine
			n.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey] = "True"
			n.Annotations[common.QuarantineHealthEventCordonPreExistingAnnotationKey] = "True"
		}, false},
		{"dry-run", func(n *corev1.Node, _ cordonownership.Claim) {
			n.Annotations[cordonownership.PendingHandoffAnnotationKey] = claimSource
			n.Annotations[common.QuarantineHealthEventDryRunAnnotationKey] = ""
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, claim := claimedNode(t, "claim-inherit-"+tc.name)
			tc.setup(node, claim)
			_, err := testClient.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
			require.NoError(t, err)
			// A fresh client represents a controller restart between A and B.
			client := &FaultQuarantineClient{Clientset: testClient}
			incoming := ownershipQuarantineAnnotations()
			incoming[common.QuarantineHealthEventAnnotationKey] = claimQuarantine
			_, err = client.QuarantineNodeAndSetAnnotations(t.Context(), node.Name, nil, true, incoming, nil)
			require.NoError(t, err)
			current, err := testClient.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.True(t, current.Spec.Unschedulable)
			inherited, ok := cordonownership.Inheritable(current)
			require.Equal(t, tc.wantOwned, ok)
			if tc.wantOwned {
				require.Equal(t, claim.ID, inherited.ID)
				require.Equal(t, common.CordonOwnershipOwned, common.CordonOwnership(current.Annotations))
			} else {
				require.NotEqual(t, common.CordonOwnershipOwned, common.CordonOwnership(current.Annotations))
			}
		})
	}
}

func TestQuarantineClaim_MetadataOnlyCordonUsesFreshOwnership(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(*corev1.Node, cordonownership.Claim)
		wantOwned bool
	}{
		{"session", func(n *corev1.Node, c cordonownership.Claim) {
			n.Annotations[cordonownership.ValidationSessionAnnotationKey] = ownedClaimSession(t, n, c)
		}, true},
		{"pending", func(n *corev1.Node, _ cordonownership.Claim) {
			n.Annotations[cordonownership.PendingHandoffAnnotationKey] = claimSource
		}, true},
		{"already-owned", func(n *corev1.Node, _ cordonownership.Claim) {
			n.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey] = "True"
		}, true},
		{"manual", func(n *corev1.Node, c cordonownership.Claim) {
			n.Annotations[cordonownership.ValidationSessionAnnotationKey] = ownedClaimSession(t, n, c)
			n.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey] = "True"
			n.Annotations[common.QuarantineHealthEventCordonPreExistingAnnotationKey] = "True"
		}, false},
		{"unknown-record", func(n *corev1.Node, c cordonownership.Claim) {
			n.Annotations[cordonownership.ValidationSessionAnnotationKey] = ownedClaimSession(t, n, c)
			n.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey] = "malformed"
		}, false},
		{"preserved", func(n *corev1.Node, c cordonownership.Claim) {
			n.Annotations[cordonownership.ValidationSessionAnnotationKey] = ownedClaimSession(t, n, c)
			c.PreserveCordon = true
			_, err := cordonownership.Store(n, c)
			require.NoError(t, err)
		}, false},
		{"dry-run", func(n *corev1.Node, c cordonownership.Claim) {
			n.Annotations[cordonownership.ValidationSessionAnnotationKey] = ownedClaimSession(t, n, c)
			n.Annotations[common.QuarantineHealthEventDryRunAnnotationKey] = ""
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, claim := claimedNode(t, "claim-record-cordon-"+tc.name)
			node.Annotations[cordonownership.QuarantineAnnotationKey] = claimQuarantine
			tc.setup(node, claim)
			_, err := testClient.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
			require.NoError(t, err)
			// Later events record a required cordon even when their cached Node is
			// already unschedulable and no physical cordon action is requested.
			incoming := map[string]string{
				common.QuarantineHealthEventIsCordonedAnnotationKey:        "True",
				common.QuarantineHealthEventCordonPreExistingAnnotationKey: "True",
			}
			client := &FaultQuarantineClient{Clientset: testClient}
			_, err = client.QuarantineNodeAndSetAnnotations(t.Context(), node.Name, nil, false, incoming, nil)
			require.NoError(t, err)
			current, err := testClient.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
			require.NoError(t, err)
			stored, valid := cordonownership.Read(current)
			require.True(t, valid)
			require.Equal(t, claim.ID, stored.ID)
			require.Equal(t, !tc.wantOwned, stored.PreserveCordon)
			require.Equal(t, tc.wantOwned, common.CordonOwnership(current.Annotations) == common.CordonOwnershipOwned)
			require.True(t, current.Spec.Unschedulable)
		})
	}
}

func TestQuarantineClaim_MetadataOnlyCordonRechecksConcurrentUncordon(t *testing.T) {
	node, claim := claimedNode(t, "claim-record-cordon-conflict")
	node.Annotations[cordonownership.QuarantineAnnotationKey] = claimQuarantine
	node.Annotations[cordonownership.ValidationSessionAnnotationKey] = ownedClaimSession(t, node, claim)
	_, err := testClient.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	cfg := rest.CopyConfig(testConfig)
	var once sync.Once
	var injectedErr error
	patches := 0
	cfg.Wrap(func(delegate http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodPatch {
				patches++
				once.Do(func() {
					fresh, e := testClient.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
					if e != nil {
						injectedErr = e
						return
					}
					fresh.Spec.Unschedulable = false
					_, injectedErr = testClient.CoreV1().Nodes().Update(t.Context(), fresh, metav1.UpdateOptions{})
				})
				if injectedErr != nil {
					return nil, injectedErr
				}
			}
			return delegate.RoundTrip(req)
		})
	})
	clientset, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	client := &FaultQuarantineClient{Clientset: clientset}
	_, err = client.QuarantineNodeAndSetAnnotations(t.Context(), node.Name, nil, false,
		map[string]string{common.QuarantineHealthEventIsCordonedAnnotationKey: "True"}, nil)
	require.ErrorContains(t, err, "recalculate the required cordon action")
	require.NoError(t, injectedErr)
	require.Equal(t, 1, patches, "after conflict, the fresh schedulable node requires a new action decision")
	current, err := testClient.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.False(t, current.Spec.Unschedulable)
	require.NotContains(t, current.Annotations, common.QuarantineHealthEventIsCordonedAnnotationKey)
	require.Equal(t, node.Annotations[cordonownership.AnnotationKey], current.Annotations[cordonownership.AnnotationKey])
}

func TestQuarantineClaim_AcquisitionWithoutNodeUIDStoresNoClaim(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "no-uid"}}
	client := &FaultQuarantineClient{}
	annotations, err := client.annotationsWithCordonOwnership(node, ownershipQuarantineAnnotations(), true, false)
	require.NoError(t, err)
	require.Equal(t, common.CordonOwnershipOwned, common.CordonOwnership(annotations))
	require.NotContains(t, node.Annotations, cordonownership.AnnotationKey)
	require.False(t, node.Spec.Unschedulable)
}

func TestQuarantineClaim_MetadataConflictRechecksAssociation(t *testing.T) {
	node, claim := claimedNode(t, "claim-metadata-conflict")
	node.Annotations[cordonownership.PendingHandoffAnnotationKey] = claimSource
	_, err := testClient.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	cfg := rest.CopyConfig(testConfig)
	var once sync.Once
	var injectedErr error
	patches := 0
	cfg.Wrap(func(delegate http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodPatch {
				patches++
				once.Do(func() {
					fresh, e := testClient.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
					if e != nil {
						injectedErr = e
						return
					}
					delete(fresh.Annotations, cordonownership.PendingHandoffAnnotationKey)
					_, injectedErr = testClient.CoreV1().Nodes().Update(t.Context(), fresh, metav1.UpdateOptions{})
				})
				if injectedErr != nil {
					return nil, injectedErr
				}
			}
			return delegate.RoundTrip(req)
		})
	})
	clientset, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	client := &FaultQuarantineClient{Clientset: clientset}
	incoming := ownershipQuarantineAnnotations()
	incoming[common.QuarantineHealthEventAnnotationKey] = claimQuarantine
	_, err = client.QuarantineNodeAndSetAnnotations(t.Context(), node.Name, nil, true, incoming, nil)
	require.NoError(t, err)
	require.NoError(t, injectedErr)
	require.Equal(t, 2, patches)
	fresh, err := testClient.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, common.CordonOwnershipPreExisting, common.CordonOwnership(fresh.Annotations))
	stored, valid := cordonownership.Read(fresh)
	require.True(t, valid)
	require.Equal(t, claim.ID, stored.ID)
	require.True(t, stored.PreserveCordon)
}

func TestQuarantineClaim_NewAcquisitionAndFinalCleanup(t *testing.T) {
	for _, keepSession := range []bool{false, true} {
		name := "claim-cleanup-final"
		if keepSession {
			name = "claim-cleanup-successor"
		}
		t.Run(name, func(t *testing.T) {
			createTestNode(t.Context(), t, name, nil, map[string]string{"nvsentinel-state": "quarantined", "test/cordon-by": "NVSentinel"}, nil, false)
			client := &FaultQuarantineClient{Clientset: testClient}
			incoming := ownershipQuarantineAnnotations()
			incoming[common.QuarantineHealthEventAnnotationKey] = claimQuarantine
			_, err := client.QuarantineNodeAndSetAnnotations(t.Context(), name, nil, true, incoming, nil)
			require.NoError(t, err)
			node, err := testClient.CoreV1().Nodes().Get(t.Context(), name, metav1.GetOptions{})
			require.NoError(t, err)
			claim, valid := cordonownership.Inheritable(node)
			require.True(t, valid)
			if keepSession {
				node.Annotations[cordonownership.ValidationSessionAnnotationKey] = ownedClaimSession(t, node, claim)
				_, err = testClient.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
				require.NoError(t, err)
			}
			err = client.UnQuarantineNodeAndRemoveAnnotations(t.Context(), name, nil, true, []string{cordonownership.QuarantineAnnotationKey, common.QuarantineHealthEventIsCordonedAnnotationKey, cordonownership.AnnotationKey}, []string{"nvsentinel-state", "test/cordon-by"}, nil)
			require.NoError(t, err)
			node, err = testClient.CoreV1().Nodes().Get(t.Context(), name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, keepSession, node.Spec.Unschedulable)
			require.NotContains(t, node.Labels, "nvsentinel-state")
			if keepSession {
				require.Equal(t, "NVSentinel", node.Labels["test/cordon-by"])
			}
			_, valid = cordonownership.Read(node)
			require.Equal(t, keepSession, valid)
		})
	}
}

func TestCompleteValidationHandoff_RetainsClaimForSuccessor(t *testing.T) {
	node, claim := claimedNode(t, "claim-handoff-successor")
	node.Annotations[cordonownership.QuarantineAnnotationKey] = claimQuarantine
	node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey] = "True"
	node.Annotations[cordonownership.PendingHandoffAnnotationKey] = claimSource
	node, err := testClient.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	client := &FaultQuarantineClient{Clientset: testClient}
	err = client.CompleteValidationHandoff(t.Context(), node, common.QuarantineAnnotationKeys, nil)
	require.NoError(t, err)
	node, err = testClient.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	got, valid := cordonownership.Inheritable(node)
	require.True(t, valid)
	require.Equal(t, claim.ID, got.ID)
	require.True(t, node.Spec.Unschedulable)
	require.NotContains(t, node.Annotations, cordonownership.QuarantineAnnotationKey)
}

func TestQuarantineClaim_RecreatedNodeDuringAcquisition(t *testing.T) {
	const name = "claim-node-recreated"
	createTestNode(t.Context(), t, name, nil, nil, nil, false)
	cfg := rest.CopyConfig(testConfig)
	var once sync.Once
	var injectedErr error
	cfg.Wrap(func(delegate http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodPatch {
				once.Do(func() {
					injectedErr = testClient.CoreV1().Nodes().Delete(t.Context(), name, metav1.DeleteOptions{})
					if injectedErr != nil {
						return
					}
					_, injectedErr = testClient.CoreV1().Nodes().Create(t.Context(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
				})
				if injectedErr != nil {
					return nil, injectedErr
				}
			}
			return delegate.RoundTrip(req)
		})
	})
	clientset, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	client := &FaultQuarantineClient{Clientset: clientset}
	incoming := ownershipQuarantineAnnotations()
	incoming[common.QuarantineHealthEventAnnotationKey] = claimQuarantine
	_, err = client.QuarantineNodeAndSetAnnotations(t.Context(), name, nil, true, incoming, nil)
	require.Error(t, err)
	require.NoError(t, injectedErr)
	node, err := testClient.CoreV1().Nodes().Get(t.Context(), name, metav1.GetOptions{})
	require.NoError(t, err)
	require.False(t, node.Spec.Unschedulable)
	require.NotContains(t, node.Annotations, cordonownership.AnnotationKey)
}

func TestQuarantineClaim_ReleaseConflictKeepsNewValidationSession(t *testing.T) {
	node, claim := claimedNode(t, "claim-release-session-conflict")
	node.Annotations[cordonownership.QuarantineAnnotationKey] = claimQuarantine
	node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey] = "True"
	_, err := testClient.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	cfg := rest.CopyConfig(testConfig)
	var once sync.Once
	var injectedErr error
	patches := 0
	cfg.Wrap(func(delegate http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodPatch {
				patches++
				once.Do(func() {
					fresh, e := testClient.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
					if e != nil {
						injectedErr = e
						return
					}
					fresh.Annotations[cordonownership.ValidationSessionAnnotationKey] = ownedClaimSession(t, fresh, claim)
					_, injectedErr = testClient.CoreV1().Nodes().Update(t.Context(), fresh, metav1.UpdateOptions{})
				})
				if injectedErr != nil {
					return nil, injectedErr
				}
			}
			return delegate.RoundTrip(req)
		})
	})
	clientset, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	client := &FaultQuarantineClient{Clientset: clientset}
	err = client.UnQuarantineNodeAndRemoveAnnotations(t.Context(), node.Name, nil, true, []string{cordonownership.QuarantineAnnotationKey, common.QuarantineHealthEventIsCordonedAnnotationKey, cordonownership.AnnotationKey}, nil, nil)
	require.NoError(t, err)
	require.NoError(t, injectedErr)
	require.Equal(t, 2, patches)
	node, err = testClient.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.True(t, node.Spec.Unschedulable)
	require.NotContains(t, node.Annotations, cordonownership.QuarantineAnnotationKey)
	inherited, valid := cordonownership.Inheritable(node)
	require.True(t, valid)
	require.Equal(t, claim.ID, inherited.ID)
}

func TestQuarantineClaim_ManualTakeoverPreservesSuccessor(t *testing.T) {
	for _, untaint := range []bool{false, true} {
		name := "claim-manual-uncordon"
		if untaint {
			name = "claim-manual-untaint"
		}
		t.Run(name, func(t *testing.T) {
			node, claim := claimedNode(t, name)
			node.Annotations[cordonownership.QuarantineAnnotationKey] = claimQuarantine
			node.Annotations[common.QuarantineHealthEventIsCordonedAnnotationKey] = "True"
			node.Annotations[cordonownership.ValidationSessionAnnotationKey] = ownedClaimSession(t, node, claim)
			if !untaint {
				node.Spec.Unschedulable = false
			}
			_, err := testClient.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
			require.NoError(t, err)
			client := &FaultQuarantineClient{Clientset: testClient}
			keys := []string{cordonownership.QuarantineAnnotationKey, common.QuarantineHealthEventIsCordonedAnnotationKey}
			if untaint {
				err = client.HandleManualUntaintCleanup(t.Context(), name, keys, nil, nil)
			} else {
				err = client.HandleManualUncordonCleanup(t.Context(), name, keys, nil, nil, nil)
			}
			require.NoError(t, err)
			node, err = testClient.CoreV1().Nodes().Get(t.Context(), name, metav1.GetOptions{})
			require.NoError(t, err)
			preserved, valid := cordonownership.Read(node)
			require.True(t, valid)
			require.Equal(t, claim.ID, preserved.ID)
			require.True(t, preserved.PreserveCordon)
			node.Spec.Unschedulable = true
			_, err = testClient.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
			require.NoError(t, err)
			incoming := ownershipQuarantineAnnotations()
			incoming[common.QuarantineHealthEventAnnotationKey] = claimQuarantine
			_, err = client.QuarantineNodeAndSetAnnotations(t.Context(), name, nil, true, incoming, nil)
			require.NoError(t, err)
			node, err = testClient.CoreV1().Nodes().Get(t.Context(), name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, common.CordonOwnershipPreExisting, common.CordonOwnership(node.Annotations))
			_, owned := cordonownership.Inheritable(node)
			require.False(t, owned)
		})
	}
}
