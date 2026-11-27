// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package publisher

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/metrics"
)

// testTarget is a non-unix target so healthpub skips its socket-presence gate.
const testTarget = "passthrough:///platform-connector"

type mockClient struct {
	sent []*pb.HealthEvents
	err  error
}

func (c *mockClient) HealthEventOccurredV1(
	_ context.Context, in *pb.HealthEvents, _ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	if c.err != nil {
		return nil, c.err
	}

	c.sent = append(c.sent, in)

	return &emptypb.Empty{}, nil
}

func event(check string, fatal bool) *pb.HealthEvent {
	return &pb.HealthEvent{CheckName: check, IsFatal: fatal}
}

func TestPublishSendsOneBatchAndCountsEvents(t *testing.T) {
	client := &mockClient{}
	p := New(client, testTarget, "pub-node")

	published := func(check string) float64 {
		return testutil.ToFloat64(metrics.EventsPublished.WithLabelValues("pub-node", check, "true"))
	}
	before := published("NVHVa")

	require.NoError(t, p.Publish(context.Background(), []*pb.HealthEvent{event("NVHVa", true), event("NVHVb", true)}))

	require.Len(t, client.sent, 1)
	assert.Len(t, client.sent[0].Events, 2)
	assert.InDelta(t, before+1, published("NVHVa"), 0)
}

func TestPublishFailureIsCountedAndReturned(t *testing.T) {
	client := &mockClient{err: errors.New("down")}
	p := New(client, testTarget, "pub-fail-node", healthpub.WithRetryPolicy(0, 0, 1, 0))

	failures := testutil.ToFloat64(metrics.PublishFailures.WithLabelValues("pub-fail-node"))

	err := p.Publish(context.Background(), []*pb.HealthEvent{event("NVHVa", true)})
	require.Error(t, err)
	assert.InDelta(t, failures+1, testutil.ToFloat64(metrics.PublishFailures.WithLabelValues("pub-fail-node")), 0)
}
