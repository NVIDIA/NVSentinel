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
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	akamaiapi "github.com/nvidia/nvsentinel/commons/pkg/akamai"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

// testToken is a fake credential. Nothing here talks to the real Akamai API.
const testToken = "test-token-not-a-secret"

const (
	clusterNodeInstance = 11111111
	otherInstance       = 99999999
)

// The bodies below use the shape the real account maintenance API returns,
// including its zone-less UTC timestamps.
const scheduledMaintenanceBody = `{"page":1,"pages":1,"results":2,"data":[
	{
		"id": 7,
		"entity": {"id": 11111111, "label": "lke123456-gpu", "type": "linode", "url": "/v4/linode/instances/11111111"},
		"type": "power_off_on",
		"status": "scheduled",
		"reason": "Host upgrade",
		"description": "scheduled",
		"source": "platform",
		"maintenance_policy_set": "linode/power_off_on",
		"when": "2026-10-20T12:00:00",
		"not_before": "2026-10-20T12:00:00Z",
		"start_time": null,
		"complete_time": null
	},
	{
		"id": 8,
		"entity": {"id": 99999999, "label": "someone-elses-linode", "type": "linode", "url": "/v4/linode/instances/99999999"},
		"type": "reboot",
		"status": "scheduled",
		"reason": "Host upgrade",
		"description": "scheduled",
		"source": "platform",
		"maintenance_policy_set": "linode/reboot",
		"when": "2026-10-21T12:00:00",
		"not_before": "2026-10-21T12:00:00Z",
		"start_time": null,
		"complete_time": null
	}
]}`

const emergencyMaintenanceBody = `{"page":1,"pages":1,"results":1,"data":[
	{
		"id": 9,
		"entity": {"id": 11111111, "label": "lke123456-gpu", "type": "linode", "url": "/v4/linode/instances/11111111"},
		"type": "reboot",
		"status": "scheduled",
		"reason": "Hardware failure",
		"description": "emergency",
		"source": "platform",
		"maintenance_policy_set": "linode/reboot",
		"when": null,
		"not_before": null,
		"start_time": null,
		"complete_time": null
	}
]}`

// newMaintenanceAPI serves body as the account maintenance list from a local
// server, moves the pinned API host to that server for the rest of the test,
// and returns the API client that newAPI builds. newAPI is the path NewClient
// takes, so the token and endpoint checks run for real.
func newMaintenanceAPI(t *testing.T, body string) *akamaiapi.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v4/account/maintenance", r.URL.Path)
		assert.Equal(t, "Bearer "+testToken, r.Header.Get("Authorization"))

		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	host, _, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)

	akamaiapi.AllowLoopbackHostForTest(t, host)
	t.Setenv(akamaiapi.APITokenEnvVar, testToken)

	api, endpoint, err := newAPI(config.AkamaiConfig{APIEndpoint: srv.URL + "/v4"})
	require.NoError(t, err)
	require.Equal(t, srv.URL+"/v4", endpoint)

	return api
}

// poll runs one poll against a client wired to api and returns what it emitted.
func poll(t *testing.T, api maintenanceAPI) []model.MaintenanceEvent {
	t.Helper()

	c := newTestClient(api, fakeNodes{clusterNodeInstance: "node-a"})
	ch := make(chan model.MaintenanceEvent, 4)

	require.NoError(t, c.pollEvents(context.Background(), ch))
	close(ch)

	var got []model.MaintenanceEvent
	for e := range ch {
		got = append(got, e)
	}

	return got
}

// TestPollEvents_ScheduledMaintenanceOverHTTP_EmitsDetectedEventForClusterNode
// follows one maintenance event the whole way: the API's JSON, through the real
// client, to the normalized event. The account also lists an instance that is
// not a node of this cluster, and that one must not become an event.
func TestPollEvents_ScheduledMaintenanceOverHTTP_EmitsDetectedEventForClusterNode(t *testing.T) {
	got := poll(t, newMaintenanceAPI(t, scheduledMaintenanceBody))

	require.Len(t, got, 1, "only the node of this cluster may produce an event")

	e := got[0]
	assert.Equal(t, "akamai-7", e.EventID)
	assert.Equal(t, model.CSPAkamai, e.CSP)
	assert.Equal(t, "cluster-a", e.ClusterName)
	assert.Equal(t, "node-a", e.NodeName)
	assert.Equal(t, "11111111", e.ResourceID)
	assert.Equal(t, model.TypeScheduled, e.MaintenanceType)
	assert.Equal(t, model.StatusDetected, e.Status)
	assert.Equal(t, model.ProviderStatus("scheduled"), e.CSPStatus)

	// The zone-less "when" lands on the same instant, in UTC.
	require.NotNil(t, e.ScheduledStartTime)
	assert.True(t, e.ScheduledStartTime.Equal(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)))
	assert.Nil(t, e.ActualStartTime)
	assert.Nil(t, e.ActualEndTime)

	assert.Equal(t, "power_off_on", e.Metadata["maintenanceType"])
	assert.Equal(t, "linode/power_off_on", e.Metadata["policy"])
	assert.Equal(t, "Host upgrade", e.Metadata["reason"])
	assert.NotEmpty(t, e.Metadata[model.ProviderLastUpdatedKey], "the change fingerprint must be set")
}

// TestPollEvents_EmergencyMaintenanceOverHTTP_EmitsUnscheduledEventWithoutStartTime
// pins the emergency path: no scheduled start, so the trigger fires on the next
// poll, and the urgency is the shared constant the trigger query matches.
func TestPollEvents_EmergencyMaintenanceOverHTTP_EmitsUnscheduledEventWithoutStartTime(t *testing.T) {
	got := poll(t, newMaintenanceAPI(t, emergencyMaintenanceBody))

	require.Len(t, got, 1)

	e := got[0]
	assert.Equal(t, "akamai-9", e.EventID)
	assert.Equal(t, "node-a", e.NodeName)
	assert.Equal(t, model.TypeUnscheduled, e.MaintenanceType)
	assert.Equal(t, model.StatusDetected, e.Status)
	assert.Nil(t, e.ScheduledStartTime, "an emergency event has no scheduled start")
	assert.Equal(t, model.MetadataUrgencyEmergency, e.Metadata["urgency"])
}

// TestNewAPI_LoopbackEndpointWithoutMovedPin_IsRefused pins that a local server
// is reachable only while a test has moved the pin. Without that, the
// production check refuses it, so the token cannot be sent to a local address.
func TestNewAPI_LoopbackEndpointWithoutMovedPin_IsRefused(t *testing.T) {
	t.Setenv(akamaiapi.APITokenEnvVar, testToken)

	_, _, err := newAPI(config.AkamaiConfig{APIEndpoint: "http://127.0.0.1:43210/v4"})

	require.ErrorContains(t, err, "failed to resolve Akamai API endpoint")
	assert.ErrorContains(t, err, "cleartext")
}
