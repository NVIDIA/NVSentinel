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

package initializer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

type stubLagProvider struct{}

func (stubLagProvider) LagState() (lastEmptyBatch, lastEventRead time.Time) {
	return time.Now(), time.Time{}
}

// This service serves only controller-runtime's registry, so store-client's change stream
// metrics have to be registered there. A test against the default registry would pass while
// /metrics stayed empty, which is the failure this covers.
func TestChangeStreamLagMetricsReachControllerRuntimeRegistry(t *testing.T) {
	client.RegisterChangeStreamLag(crmetrics.Registry, t.Name(), stubLagProvider{})

	families, err := crmetrics.Registry.Gather()
	require.NoError(t, err)

	var found []string

	for _, family := range families {
		switch family.GetName() {
		case "change_stream_lag_seconds", "change_stream_lag_known":
			found = append(found, family.GetName())
		}
	}

	assert.ElementsMatch(t, []string{"change_stream_lag_seconds", "change_stream_lag_known"}, found)
}
