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

package v1alpha1

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// This envtest spec is the acceptance test for the protojson marshalling fix.
// It exercises the full apiserver path that pure unit tests can't reach: the
// wrapper is created via the API server, then a status PATCH carrying a
// LastTransitionTime is applied. Without the custom MarshalJSON/UnmarshalJSON
// in external_remediation_json.go this would fail CRD validation with HTTP 422
// ("status.conditions[0].lastTransitionTime: Invalid value: 'object': must be
// of type string"). With the fix in place, the patch succeeds and round-trips
// the proto Timestamp.

var _ = Describe("ExternalRemediationRequest JSON wire format", func() {
	It("round-trips through the apiserver with a non-zero Condition.LastTransitionTime", func() {
		extrrObj := &ExternalRemediationRequest{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "nvsentinel.dgxc.nvidia.com/v1",
				Kind:       "ExternalRemediationRequest",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "envtest-extrr-1",
			},
			Spec: &protos.ExternalRemediationRequestSpec{
				HealthEvent: &protos.HealthEvent{
					Id:                "he-envtest",
					NodeName:          "node-envtest",
					IsFatal:           true,
					RecommendedAction: protos.RecommendedAction_CUSTOM,
					Message:           "synthetic envtest fault",
				},
			},
		}

		Expect(k8sClient.Create(ctx, extrrObj)).To(Succeed(),
			"creating an ExtRR through the apiserver must succeed (CRD must accept the spec shape)")

		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, extrrObj)
		})

		// Status patch with a Condition that carries a Timestamp — the path
		// that previously 422'd because encoding/json was emitting
		// {seconds, nanos} for the timestamp instead of an RFC3339 string.
		at := time.Date(2026, 6, 5, 16, 0, 0, 0, time.UTC)
		patched := extrrObj.DeepCopy()
		patched.Status = &protos.ExternalRemediationRequestStatus{
			Conditions: []*protos.Condition{
				{
					Type:               "NVSentinelOwnershipReleased",
					Status:             "True",
					Reason:             "ReleaseTaintApplied",
					Message:            "release taint and managed=false applied",
					LastTransitionTime: timestamppb.New(at),
				},
			},
		}

		Expect(k8sClient.Status().Patch(ctx, patched, client.MergeFrom(extrrObj))).To(Succeed(),
			"status patch with Condition.LastTransitionTime must not be rejected by CRD validation")

		var got ExternalRemediationRequest
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: extrrObj.Name}, &got)).To(Succeed())

		Expect(got.Status).NotTo(BeNil())
		Expect(got.Status.Conditions).To(HaveLen(1))

		gotCond := got.Status.Conditions[0]
		Expect(gotCond.Type).To(Equal("NVSentinelOwnershipReleased"))
		Expect(gotCond.Status).To(Equal("True"))
		Expect(gotCond.Reason).To(Equal("ReleaseTaintApplied"))
		Expect(gotCond.LastTransitionTime).NotTo(BeNil(),
			"Condition.LastTransitionTime must survive the apiserver round-trip")
		Expect(gotCond.LastTransitionTime.AsTime().Equal(at)).To(BeTrue(),
			"LastTransitionTime must round-trip: want=%v got=%v", at, gotCond.LastTransitionTime.AsTime())
	})
})
