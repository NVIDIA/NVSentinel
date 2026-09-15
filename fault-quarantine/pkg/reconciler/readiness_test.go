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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/commons/pkg/server"
)

type fakeLagWatcher struct {
	lastEmptyBatch time.Time
	lastEventRead  time.Time
}

func (f fakeLagWatcher) LagState() (lastEmptyBatch, lastEventRead time.Time) {
	return f.lastEmptyBatch, f.lastEventRead
}

func TestReconciler_ReadinessChecker(t *testing.T) {
	r := &Reconciler{}
	checker := server.NewDatastoreReadinessChecker(prometheus.NewRegistry())
	r.SetReadinessChecker(checker)

	ctx := context.Background()

	// Initial: returns error
	err := checker.Ready(ctx)
	require.Error(t, err)

	// Simulating setupChangeStreamWatcher completing with a watcher that hasn't polled
	r.readinessChecker.SetWatcher(fakeLagWatcher{})

	err = checker.Ready(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "awaiting first poll")

	// Simulating watcher having polled an empty batch
	r.readinessChecker.SetWatcher(fakeLagWatcher{lastEmptyBatch: time.Now()})

	err = checker.Ready(ctx)
	require.NoError(t, err)
}
