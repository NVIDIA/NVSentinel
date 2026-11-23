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

package akamai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_GetInstance_Running_ReturnsInstanceFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/linode/instances/11111111", r.URL.Path)

		// The API returns the instance object itself, not a data envelope.
		_, _ = w.Write([]byte(`{"id":11111111,"status":"running","lke_cluster_id":123456,"label":"lke123456-gpu"}`))
	}))
	defer srv.Close()

	instance, err := newTestClient(t, srv).GetInstance(context.Background(), 11111111)
	require.NoError(t, err)
	assert.Equal(t, 11111111, instance.ID)
	assert.Equal(t, InstanceStatusRunning, instance.Status)
	require.NotNil(t, instance.LKEClusterID)
	assert.Equal(t, 123456, *instance.LKEClusterID)
}

func TestClient_GetInstance_NonLKEInstance_HasNilClusterID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":1,"status":"offline","lke_cluster_id":null}`))
	}))
	defer srv.Close()

	instance, err := newTestClient(t, srv).GetInstance(context.Background(), 1)
	require.NoError(t, err)
	assert.Nil(t, instance.LKEClusterID)
}

func TestClient_InstanceOperations_InvalidID_ReturnErrorWithoutRequest(t *testing.T) {
	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	// A token is set and the server answers, so only the validation can make
	// these calls fail without a request.
	c := newTestClient(t, srv)

	_, err := c.GetInstance(context.Background(), 0)
	require.Error(t, err)
	require.Error(t, c.RebootInstance(context.Background(), -1))
	require.Error(t, c.DeleteInstance(context.Background(), 0))
	require.Error(t, c.RecycleLKENode(context.Background(), 0, "abc"))
	require.Error(t, c.RecycleLKENode(context.Background(), 1, ""))
	assert.Zero(t, requests.Load(), "an invalid ID must be refused before any request is sent")
}

func TestClient_RebootInstance_PostsToRebootEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/linode/instances/42/reboot", r.URL.Path)

		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	require.NoError(t, newTestClient(t, srv).RebootInstance(context.Background(), 42))
}

// TestClient_RebootInstance_LinodeBusy_ReturnsAPIReason pins that the API refusing a reboot while another
// action is queued surfaces as an error rather than a silent success.
func TestClient_RebootInstance_LinodeBusy_ReturnsAPIReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":[{"reason":"Linode busy."}]}`))
	}))
	defer srv.Close()

	err := newTestClient(t, srv).RebootInstance(context.Background(), 42)
	require.ErrorContains(t, err, "Linode busy.")
}

func TestClient_DeleteInstance_SendsDeleteRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/linode/instances/42", r.URL.Path)

		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	require.NoError(t, newTestClient(t, srv).DeleteInstance(context.Background(), 42))
}

func TestClient_FindLKENode_ReturnsMatchingNodeOrNotFoundError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/lke/clusters/123456/pools", r.URL.Path)

		_, _ = w.Write([]byte(`{"page":1,"pages":1,"results":2,"data":[
			{"id":1,"nodes":[{"id":"123456-aaa","instance_id":100,"status":"ready"}]},
			{"id":2,"nodes":[{"id":"123456-bbb","instance_id":200,"status":"ready"}]}]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)

	node, err := c.FindLKENode(context.Background(), 123456, 200)
	require.NoError(t, err)
	assert.Equal(t, "123456-bbb", node.ID)

	_, err = c.FindLKENode(context.Background(), 123456, 999)
	require.ErrorContains(t, err, "not a node of LKE cluster 123456")
}

func TestClient_RecycleLKENode_PostsToNodeRecycleEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/lke/clusters/123456/nodes/123456-bbb/recycle", r.URL.Path)

		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	require.NoError(t, newTestClient(t, srv).RecycleLKENode(context.Background(), 123456, "123456-bbb"))
}
