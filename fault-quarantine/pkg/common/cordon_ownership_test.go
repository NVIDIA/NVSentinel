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

package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCordonOwnership_RequiresQuarantineEvidence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		want        string
	}{
		{"missing", nil, CordonOwnershipUnknown},
		{"audit label only", map[string]string{"k8saas.nvidia.com/cordoned-by": "fault-quarantine"}, CordonOwnershipUnknown},
		{"owned", map[string]string{QuarantineHealthEventIsCordonedAnnotationKey: "True"}, CordonOwnershipOwned},
		{"pre-existing", map[string]string{
			QuarantineHealthEventIsCordonedAnnotationKey: "True", QuarantineHealthEventCordonPreExistingAnnotationKey: "True",
		}, CordonOwnershipPreExisting},
		{"malformed cordon", map[string]string{QuarantineHealthEventIsCordonedAnnotationKey: "true"}, CordonOwnershipUnknown},
		{"malformed pre-existing", map[string]string{
			QuarantineHealthEventIsCordonedAnnotationKey: "True", QuarantineHealthEventCordonPreExistingAnnotationKey: "False",
		}, CordonOwnershipUnknown},
		{"empty pre-existing", map[string]string{
			QuarantineHealthEventIsCordonedAnnotationKey: "True", QuarantineHealthEventCordonPreExistingAnnotationKey: "",
		}, CordonOwnershipUnknown},
		{"dry-run", map[string]string{
			QuarantineHealthEventIsCordonedAnnotationKey: "True", QuarantineHealthEventDryRunAnnotationKey: "True",
		}, CordonOwnershipUnknown},
		{"empty dry-run", map[string]string{
			QuarantineHealthEventIsCordonedAnnotationKey: "True", QuarantineHealthEventDryRunAnnotationKey: "",
		}, CordonOwnershipUnknown},
		{"malformed dry-run", map[string]string{
			QuarantineHealthEventIsCordonedAnnotationKey: "True", QuarantineHealthEventDryRunAnnotationKey: "false",
		}, CordonOwnershipUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, CordonOwnership(tc.annotations))
		})
	}
}
