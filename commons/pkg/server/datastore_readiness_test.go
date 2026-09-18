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

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeLagProvider struct {
	lastEmptyBatch time.Time
	lastEventRead  time.Time
}

func (f fakeLagProvider) LagState() (lastEmptyBatch, lastEventRead time.Time) {
	return f.lastEmptyBatch, f.lastEventRead
}

func TestDatastoreReadinessChecker_Lifecycle(t *testing.T) {
	reg := prometheus.NewRegistry()
	checker := NewDatastoreReadinessChecker(reg)
	ctx := context.Background()

	// 1. Before provider is set: returns error, metric is 0
	err := checker.Ready(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "datastore watcher initializing")
	assert.Equal(t, float64(0), testutil.ToFloat64(checker))

	checkErr := checker.Check(nil)
	require.Error(t, checkErr)
	assert.Contains(t, checkErr.Error(), "datastore watcher initializing")

	// 2. Provider set (watcher connected): ready, metric is 1
	checker.SetLagProvider(fakeLagProvider{})

	err = checker.Ready(ctx)
	require.NoError(t, err)
	assert.Equal(t, float64(1), testutil.ToFloat64(checker))

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	assert.NoError(t, checker.Check(req))
}

func TestDatastoreReadinessChecker_SetWatcher(t *testing.T) {
	reg := prometheus.NewRegistry()
	checker := NewDatastoreReadinessChecker(reg)
	ctx := context.Background()

	// Set nil
	checker.SetWatcher(nil)
	assert.Error(t, checker.Ready(ctx))

	// Set non-provider object (does not set provider)
	checker.SetWatcher("not-a-provider")
	assert.Error(t, checker.Ready(ctx))

	// Set valid watcher satisfying LagStateProvider
	checker.SetWatcher(fakeLagProvider{})
	assert.NoError(t, checker.Ready(ctx))
	assert.Equal(t, float64(1), testutil.ToFloat64(checker))
}

func TestDatastoreReadinessChecker_ServerIntegration(t *testing.T) {
	reg := prometheus.NewRegistry()
	checker := NewDatastoreReadinessChecker(reg)

	srv := NewServer(
		WithPort(0),
		WithSimpleHealth(),
		WithReadinessCheck(checker),
	)

	concreteSrv, ok := srv.(*server)
	require.True(t, ok)

	// Before watcher connects: /healthz is 200, /readyz is 503
	recHealthz := httptest.NewRecorder()
	reqHealthz := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	concreteSrv.mux.ServeHTTP(recHealthz, reqHealthz)
	assert.Equal(t, http.StatusOK, recHealthz.Code)

	recReadyz := httptest.NewRecorder()
	reqReadyz := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	concreteSrv.mux.ServeHTTP(recReadyz, reqReadyz)
	assert.Equal(t, http.StatusServiceUnavailable, recReadyz.Code)

	// After watcher connects: /readyz flips to 200
	checker.SetLagProvider(fakeLagProvider{})

	recReadyz2 := httptest.NewRecorder()
	concreteSrv.mux.ServeHTTP(recReadyz2, reqReadyz)
	assert.Equal(t, http.StatusOK, recReadyz2.Code)
}

func TestDatastoreReadinessChecker_DuplicateRegistration(t *testing.T) {
	reg := prometheus.NewRegistry()
	checker1 := NewDatastoreReadinessChecker(reg)
	require.NotNil(t, checker1)

	// Creating a second checker on the same registry returns the existing collector
	checker2 := NewDatastoreReadinessChecker(reg)
	require.NotNil(t, checker2)
	assert.Same(t, checker1, checker2)

	// Updating checker2 updates the registered collector
	checker2.SetLagProvider(fakeLagProvider{lastEmptyBatch: time.Now()})
	assert.Equal(t, float64(1), testutil.ToFloat64(checker1))
}
