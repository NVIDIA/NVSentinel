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

package devicecounts

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The node cache keeps every annotation that decides whether a label counts.
func TestEvidenceAnnotationKeys(t *testing.T) {
	require.Equal(t, ratchetPartitionAnnotation, PartitionAnnotationKey("gpu"))
	require.Equal(t, []string{ratchetPartitionAnnotation},
		newTestManager(t, testGPUSourcesConfig()).EvidenceAnnotationKeys())
	require.Equal(t, []string{ratchetPartitionAnnotation, ratchetAnnotation},
		newTestManager(t, relearnTestConfig(ratchetRelearnID)).EvidenceAnnotationKeys())

	var disabled *Manager
	require.Empty(t, disabled.EvidenceAnnotationKeys())
}
