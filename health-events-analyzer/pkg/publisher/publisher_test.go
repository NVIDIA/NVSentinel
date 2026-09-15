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

package publisher

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

type fakePlatformConnectorClient struct {
	events *protos.HealthEvents
}

func (f *fakePlatformConnectorClient) HealthEventOccurredV1(
	_ context.Context, events *protos.HealthEvents, _ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	f.events = events

	return &emptypb.Empty{}, nil
}

// sourceEvent is a detector event from the past, standing in for one replayed off a lagging
// change stream.
func sourceEvent(generated time.Time) *protos.HealthEvent {
	return &protos.HealthEvent{
		Agent:              "syslog-health-monitor",
		CheckName:          "SysLogsXIDError",
		ComponentClass:     "GPU",
		NodeName:           "node-1",
		ErrorCode:          []string{"31"},
		IsHealthy:          false,
		GeneratedTimestamp: timestamppb.New(generated),
	}
}

func TestPublish_LaggingSourceEvent_StampsPublishTimeAndKeepsSourceTimestamp(t *testing.T) {
	client := &fakePlatformConnectorClient{}
	pub := NewPublisher(client, protos.ProcessingStrategy_EXECUTE_REMEDIATION)

	sourceTime := time.Date(2026, 8, 21, 8, 27, 36, 0, time.UTC)
	before := time.Now()

	err := pub.Publish(context.Background(), sourceEvent(sourceTime),
		protos.RecommendedAction_RUN_DCGMEUD, "RepeatedXID31OnSameGPU", "run field diagnostics", nil)
	require.NoError(t, err)
	require.NotNil(t, client.events)
	require.Len(t, client.events.GetEvents(), 1)

	published := client.events.GetEvents()[0]

	// The derived event must be stamped at publish time, not inherit the source timestamp.
	require.NotNil(t, published.GetGeneratedTimestamp())
	require.False(t, published.GetGeneratedTimestamp().AsTime().Equal(sourceTime),
		"derived event inherited the source generated timestamp")
	require.False(t, published.GetGeneratedTimestamp().AsTime().Before(before.Add(-time.Second)),
		"derived event timestamp predates the publish call")

	// The source timestamp is preserved so provenance is not lost.
	require.Equal(t, sourceTime.Format(time.RFC3339Nano),
		published.GetMetadata()[sourceGeneratedTimestampMetadataKey])
}

// Asserts the wire key literally rather than through the constant, so a rename cannot
// silently change what consumers see while the tests still pass.
func TestPublish_DerivedEvent_UsesTheDocumentedMetadataKey(t *testing.T) {
	client := &fakePlatformConnectorClient{}
	pub := NewPublisher(client, protos.ProcessingStrategy_EXECUTE_REMEDIATION)

	sourceTime := time.Date(2026, 8, 21, 8, 27, 36, 0, time.UTC)

	err := pub.Publish(context.Background(), sourceEvent(sourceTime),
		protos.RecommendedAction_NONE, "XIDErrorSoloNoBurst", "no action", nil)
	require.NoError(t, err)

	published := client.events.GetEvents()[0]
	require.Equal(t, sourceTime.Format(time.RFC3339Nano),
		published.GetMetadata()["source_generated_timestamp"])
}

func TestPublish_SourceWithMetadata_PreservesExistingKeys(t *testing.T) {
	client := &fakePlatformConnectorClient{}
	pub := NewPublisher(client, protos.ProcessingStrategy_EXECUTE_REMEDIATION)

	sourceTime := time.Date(2026, 8, 21, 8, 27, 36, 0, time.UTC)
	src := sourceEvent(sourceTime)
	src.Metadata = map[string]string{"existing": "value"}

	err := pub.Publish(context.Background(), src,
		protos.RecommendedAction_NONE, "XIDErrorSoloNoBurst", "no action", nil)
	require.NoError(t, err)

	published := client.events.GetEvents()[0]
	require.Equal(t, "value", published.GetMetadata()["existing"])
	require.Equal(t, sourceTime.Format(time.RFC3339Nano),
		published.GetMetadata()[sourceGeneratedTimestampMetadataKey])
}

func TestPublish_SourceWithoutTimestamp_StampsWithoutSourceMetadata(t *testing.T) {
	client := &fakePlatformConnectorClient{}
	pub := NewPublisher(client, protos.ProcessingStrategy_EXECUTE_REMEDIATION)

	src := sourceEvent(time.Time{})
	src.GeneratedTimestamp = nil

	err := pub.Publish(context.Background(), src,
		protos.RecommendedAction_NONE, "XIDErrorSoloNoBurst", "no action", nil)
	require.NoError(t, err)

	published := client.events.GetEvents()[0]
	require.NotNil(t, published.GetGeneratedTimestamp())
	require.NotContains(t, published.GetMetadata(), sourceGeneratedTimestampMetadataKey)
}

func TestPublish_AnySourceEvent_DoesNotMutateCaller(t *testing.T) {
	client := &fakePlatformConnectorClient{}
	pub := NewPublisher(client, protos.ProcessingStrategy_EXECUTE_REMEDIATION)

	sourceTime := time.Date(2026, 8, 21, 8, 27, 36, 0, time.UTC)
	src := sourceEvent(sourceTime)

	err := pub.Publish(context.Background(), src,
		protos.RecommendedAction_RUN_DCGMEUD, "RepeatedXID31OnSameGPU", "run field diagnostics", nil)
	require.NoError(t, err)

	// Publish clones, so the caller's event must be untouched.
	require.True(t, src.GetGeneratedTimestamp().AsTime().Equal(sourceTime))
	require.Equal(t, "syslog-health-monitor", src.GetAgent())
	require.NotContains(t, src.GetMetadata(), sourceGeneratedTimestampMetadataKey)
}
