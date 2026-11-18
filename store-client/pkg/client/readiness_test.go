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

package client

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeReadinessChangeStreamWatcher struct {
	ChangeStreamWatcher
	ready bool
}

func (w *fakeReadinessChangeStreamWatcher) IsReady() bool {
	return w.ready
}

func TestResumeControlChangeStreamWatcher_ForwardsReadiness(t *testing.T) {
	inner := &fakeReadinessChangeStreamWatcher{}
	wrapped := NewChangeStreamWatcherWithResumeControl(inner, ResumeControlDecision{})
	provider, ok := wrapped.(interface{ IsReady() bool })
	require.True(t, ok)

	require.False(t, provider.IsReady())
	inner.ready = true
	require.True(t, provider.IsReady())
}
