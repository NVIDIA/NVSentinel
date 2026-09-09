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
	"github.com/nvidia/nvsentinel/store-client/pkg/lagstate"
)

// lagStateWatcher is a watcher that reports lag state, standing in for the MongoDB watcher at
// the bottom of the chain.
type lagStateWatcher struct {
	client.ChangeStreamWatcher

	observed time.Time
}

func (w *lagStateWatcher) LagState() (lastEmptyBatch, lastEventRead time.Time) {
	return w.observed, time.Time{}
}

// This service serves only controller-runtime's registry, so store-client's change stream
// metrics have to be registered there. A test against the default registry would pass while
// /metrics stayed empty.
//
// The watcher is wrapped the way the factory wraps it in production, because an earlier version
// of this test passed a bare stub and therefore passed while the real MongoDB chain registered
// nothing: the resume-control wrapper had no LagState, so the assertion inside
// RegisterChangeStreamLag answered for the wrapper. Registering the unwrapped watcher tests a
// path production never takes.
func TestRegisterChangeStreamLag_ProductionChain_ExportsOnControllerRuntimeRegistry(t *testing.T) {
	inner := &lagStateWatcher{observed: time.Now()}
	wrapped := client.NewChangeStreamWatcherWithResumeControl(inner, client.ResumeControlDecision{})

	require.Implements(t, (*lagstate.Provider)(nil), wrapped,
		"the resume-control wrapper must pass LagState through, or nothing registers")

	client.RegisterChangeStreamLag(crmetrics.Registry, t.Name(), wrapped)

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
