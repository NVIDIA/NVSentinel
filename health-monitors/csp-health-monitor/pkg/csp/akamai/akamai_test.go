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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	akamaiapi "github.com/nvidia/nvsentinel/commons/pkg/akamai"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	eventpkg "github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/event"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

type fakeAPI struct {
	events []akamaiapi.Maintenance
	err    error
}

func (f *fakeAPI) ListMaintenance(context.Context) ([]akamaiapi.Maintenance, error) {
	return f.events, f.err
}

type fakeNodes map[int]string

func (f fakeNodes) InstanceNodeNames(context.Context) (map[int]string, error) {
	return f, nil
}

type failingNodes struct{}

func (failingNodes) InstanceNodeNames(context.Context) (map[int]string, error) {
	return nil, errors.New("api down")
}

func newTestClient(api maintenanceAPI, nodes nodeResolver) *Client {
	return &Client{
		cfg:         config.AkamaiConfig{PollingIntervalSeconds: 30},
		clusterName: "cluster-a",
		api:         api,
		nodes:       nodes,
		normalizer:  &eventpkg.AkamaiNormalizer{},
	}
}

func event(id, entityID int, entityType string) akamaiapi.Maintenance {
	return akamaiapi.Maintenance{
		ID:          id,
		Entity:      akamaiapi.MaintenanceEntity{ID: entityID, Type: entityType},
		Type:        "power_off_on",
		Status:      "scheduled",
		Description: akamaiapi.MaintenanceDescriptionScheduled,
		When:        &akamaiapi.Timestamp{Time: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)},
	}
}

// TestPollEventsOnlyEmitsNodesOfThisCluster pins the filter that matters most
// here: the account lists maintenance for every instance in it, and only the
// ones backing this cluster's nodes may become events.
func TestPollEventsOnlyEmitsNodesOfThisCluster(t *testing.T) {
	c := newTestClient(
		&fakeAPI{events: []akamaiapi.Maintenance{
			event(1, 100, akamaiapi.MaintenanceEntityLinode),
			event(2, 999, akamaiapi.MaintenanceEntityLinode), // someone else's instance
			event(3, 100, "volume"),                          // not an instance
			event(4, 200, akamaiapi.MaintenanceEntityLinode),
		}},
		fakeNodes{100: "node-a", 200: "node-b"},
	)

	ch := make(chan model.MaintenanceEvent, 10)
	require.NoError(t, c.pollEvents(context.Background(), ch))
	close(ch)

	var got []model.MaintenanceEvent
	for e := range ch {
		got = append(got, e)
	}

	require.Len(t, got, 2)
	assert.Equal(t, "akamai-1", got[0].EventID)
	assert.Equal(t, "node-a", got[0].NodeName)
	assert.Equal(t, "cluster-a", got[0].ClusterName)
	assert.Equal(t, "akamai-4", got[1].EventID)
	assert.Equal(t, "node-b", got[1].NodeName)
}

// TestPollEventsNoMatchEmitsNothing pins that an account full of unrelated
// maintenance never quarantines anything.
func TestPollEventsNoMatchEmitsNothing(t *testing.T) {
	c := newTestClient(
		&fakeAPI{events: []akamaiapi.Maintenance{event(1, 999, akamaiapi.MaintenanceEntityLinode)}},
		fakeNodes{},
	)

	ch := make(chan model.MaintenanceEvent, 1)
	require.NoError(t, c.pollEvents(context.Background(), ch))
	assert.Empty(t, ch)
}

func TestPollEventsAPIError(t *testing.T) {
	c := newTestClient(&fakeAPI{err: errors.New("boom")}, fakeNodes{})

	ch := make(chan model.MaintenanceEvent, 1)
	require.ErrorContains(t, c.pollEvents(context.Background(), ch), "boom")
	assert.Empty(t, ch)
}

func TestPollEventsStopsOnCancel(t *testing.T) {
	c := newTestClient(
		&fakeAPI{events: []akamaiapi.Maintenance{event(1, 100, akamaiapi.MaintenanceEntityLinode)}},
		fakeNodes{100: "node-a"},
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// An unbuffered channel nobody reads would block forever without the ctx check.
	require.ErrorIs(t, c.pollEvents(ctx, make(chan model.MaintenanceEvent)), context.Canceled)
}

func TestGetName(t *testing.T) {
	assert.Equal(t, model.CSPAkamai, newTestClient(nil, nil).GetName())
}

func TestNewClientRequiresToken(t *testing.T) {
	t.Setenv(akamaiapi.APITokenEnvVar, "")

	_, err := NewClient(context.Background(), config.AkamaiConfig{}, "c", "")
	require.ErrorContains(t, err, akamaiapi.APITokenEnvVar)
}

func TestNewClientRefusesForeignEndpoint(t *testing.T) {
	t.Setenv(akamaiapi.APITokenEnvVar, "fake-token")

	_, err := NewClient(context.Background(), config.AkamaiConfig{APIEndpoint: "https://example.com/v4"}, "c", "")
	require.ErrorContains(t, err, "not an approved")
}

func node(name, providerID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}
}

func TestExtractInstanceID(t *testing.T) {
	tests := []struct {
		providerID string
		want       int
	}{
		{"linode://11111111", 11111111},
		{"", 0},
		{"aws:///us-east-1a/i-0abc", 0},
		{"lambda://abc", 0},
		{"linode://", 0},
		{"linode://abc", 0},
		{"linode://0", 0},
		{"linode://-3", 0},
	}

	for _, tt := range tests {
		t.Run(tt.providerID, func(t *testing.T) {
			assert.Equal(t, tt.want, extractInstanceID(node("n", tt.providerID)))
		})
	}
}

func TestNodeResolverMapsInstancesToNodes(t *testing.T) {
	k8s := fake.NewClientset(
		node("node-a", "linode://100"),
		node("node-b", "linode://200"),
		// No providerID yet, a foreign provider and a malformed ID never map.
		node("node-c", ""),
		node("node-x", "aws:///z/i-1"),
		node("node-y", "linode://abc"),
	)

	names, err := NewNodeResolver(k8s).InstanceNodeNames(context.Background())
	require.NoError(t, err)
	assert.Equal(t, map[int]string{100: "node-a", 200: "node-b"}, names)
}

// TestNodeResolverSeesProviderIDSetLater covers a node that registers with no
// providerID and gets one afterwards: each call reads the current nodes, so the
// ID is picked up by the next poll.
func TestNodeResolverSeesProviderIDSetLater(t *testing.T) {
	ctx := context.Background()
	k8s := fake.NewClientset()
	nodes := k8s.CoreV1().Nodes()
	resolver := NewNodeResolver(k8s)

	created, err := nodes.Create(ctx, node("node-a", ""), metav1.CreateOptions{})
	require.NoError(t, err)

	names, err := resolver.InstanceNodeNames(ctx)
	require.NoError(t, err)
	assert.Empty(t, names)

	created.Spec.ProviderID = "linode://100"
	_, err = nodes.Update(ctx, created, metav1.UpdateOptions{})
	require.NoError(t, err)

	names, err = resolver.InstanceNodeNames(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[int]string{100: "node-a"}, names)

	require.NoError(t, nodes.Delete(ctx, "node-a", metav1.DeleteOptions{}))

	names, err = resolver.InstanceNodeNames(ctx)
	require.NoError(t, err)
	assert.Empty(t, names)
}

// TestPollEventsSkipsNodeListWithoutEvents keeps an idle poll from listing nodes.
func TestPollEventsSkipsNodeListWithoutEvents(t *testing.T) {
	c := newTestClient(&fakeAPI{}, failingNodes{})

	require.NoError(t, c.pollEvents(context.Background(), make(chan model.MaintenanceEvent, 1)))
}

func TestPollEventsReturnsNodeListError(t *testing.T) {
	c := newTestClient(&fakeAPI{events: []akamaiapi.Maintenance{event(1, 100, akamaiapi.MaintenanceEntityLinode)}},
		failingNodes{})

	require.Error(t, c.pollEvents(context.Background(), make(chan model.MaintenanceEvent, 1)))
}
