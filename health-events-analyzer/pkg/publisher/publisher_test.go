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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	storeclient "github.com/nvidia/nvsentinel/store-client/pkg/client"
)

type capturePlatformConnector struct {
	events *protos.HealthEvents
}

type unavailablePlatformConnector struct{}

func (*unavailablePlatformConnector) HealthEventOccurredV1(
	context.Context,
	*protos.HealthEvents,
	...grpc.CallOption,
) (*emptypb.Empty, error) {
	return nil, status.Error(codes.Unavailable, "connector unavailable")
}

func (c *capturePlatformConnector) HealthEventOccurredV1(
	_ context.Context,
	events *protos.HealthEvents,
	_ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	c.events = proto.Clone(events).(*protos.HealthEvents)
	return &emptypb.Empty{}, nil
}

func TestPublishRecovery(t *testing.T) {
	client := &capturePlatformConnector{}
	pub := NewPublisher(client, protos.ProcessingStrategy_EXECUTE_REMEDIATION)
	sourceGeneratedTime := time.Date(2026, 8, 21, 8, 27, 36, 0, time.UTC)
	source := &protos.HealthEvent{
		Version:                 1,
		Agent:                   "syslog-health-monitor",
		ComponentClass:          "GPU",
		CheckName:               "SysLogsXIDError",
		IsFatal:                 true,
		IsHealthy:               true,
		RecommendedAction:       protos.RecommendedAction_RESTART_BM,
		CustomRecommendedAction: "old-custom-action",
		ErrorCode:               []string{"94"},
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "0"},
			{EntityType: "GPU_UUID", EntityValue: "GPU-123"},
		},
		Metadata:            map[string]string{"source": "reboot-check"},
		GeneratedTimestamp:  timestamppb.New(sourceGeneratedTime),
		NodeName:            "node-a",
		QuarantineOverrides: &protos.BehaviourOverrides{Force: true},
		DrainOverrides:      &protos.BehaviourOverrides{Skip: true},
		ProcessingStrategy:  protos.ProcessingStrategy_STORE_AND_ANALYSE,
	}
	original := proto.Clone(source).(*protos.HealthEvent)
	rule := &config.HealthEventsAnalyzerRule{
		Name:               "RepeatedXID94OnSameGPU",
		ProcessingStrategy: "EXECUTE_REMEDIATION",
	}
	before := time.Now()

	err := pub.PublishRecovery(context.Background(), source, rule.Name,
		[]*protos.Entity{{EntityType: "GPU_UUID", EntityValue: "GPU-123"}}, rule)
	require.NoError(t, err)
	require.True(t, proto.Equal(original, source), "publishing must not mutate the source event")
	require.NotNil(t, client.events)
	require.Len(t, client.events.Events, 1)

	recovery := client.events.Events[0]
	require.Equal(t, "health-events-analyzer", recovery.Agent)
	require.Equal(t, rule.Name, recovery.CheckName)
	require.True(t, recovery.IsHealthy)
	require.False(t, recovery.IsFatal)
	require.Equal(t, protos.RecommendedAction_NONE, recovery.RecommendedAction)
	require.Empty(t, recovery.CustomRecommendedAction)
	require.Empty(t, recovery.ErrorCode)
	require.Equal(t, []*protos.Entity{{EntityType: "GPU_UUID", EntityValue: "GPU-123"}}, recovery.EntitiesImpacted)
	require.Nil(t, recovery.QuarantineOverrides)
	require.Nil(t, recovery.DrainOverrides)
	require.Equal(t, protos.ProcessingStrategy_EXECUTE_REMEDIATION, recovery.ProcessingStrategy)
	require.Equal(t, "reboot-check", recovery.Metadata["source"])
	require.Equal(t, sourceGeneratedTime.Format(time.RFC3339Nano),
		recovery.Metadata[SourceGeneratedTimestampMetadataKey])
	require.NotNil(t, recovery.GeneratedTimestamp)
	require.NotEqual(t, sourceGeneratedTime, recovery.GeneratedTimestamp.AsTime())
	require.False(t, recovery.GeneratedTimestamp.AsTime().Before(before.Add(-time.Second)))
	require.Equal(t, source.NodeName, recovery.NodeName)
}

func TestPublishRecoveryNodeScopeHasNoEntities(t *testing.T) {
	client := &capturePlatformConnector{}
	pub := NewPublisher(client, protos.ProcessingStrategy_EXECUTE_REMEDIATION)

	err := pub.PublishRecovery(context.Background(), &protos.HealthEvent{
		NodeName: "node-a",
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU_UUID", EntityValue: "GPU-123"},
		},
	}, "NodeDerivedCondition", nil, nil)
	require.NoError(t, err)
	require.Empty(t, client.events.Events[0].EntitiesImpacted)
}

func TestPublishRecoveryRejectsInvalidProcessingStrategy(t *testing.T) {
	client := &capturePlatformConnector{}
	pub := NewPublisher(client, protos.ProcessingStrategy_EXECUTE_REMEDIATION)
	rule := &config.HealthEventsAnalyzerRule{ProcessingStrategy: "NOT_A_STRATEGY"}

	err := pub.PublishRecovery(context.Background(), &protos.HealthEvent{NodeName: "node-a"},
		"DerivedCondition", nil, rule)
	require.ErrorContains(t, err, "unexpected processingStrategy value")
	require.True(t, storeclient.IsPermanentError(err))
	require.Nil(t, client.events)
}

func TestPublishPreservesUnhealthyEventSemantics(t *testing.T) {
	client := &capturePlatformConnector{}
	pub := NewPublisher(client, protos.ProcessingStrategy_EXECUTE_REMEDIATION)
	source := &protos.HealthEvent{
		Agent:            "source-monitor",
		CheckName:        "SourceCheck",
		IsHealthy:        false,
		IsFatal:          false,
		EntitiesImpacted: []*protos.Entity{nil, {EntityType: "GPU_UUID", EntityValue: "GPU-1"}},
	}

	err := pub.Publish(context.Background(), source, protos.RecommendedAction_NONE,
		"DerivedCondition", "derived", nil)
	require.NoError(t, err)
	require.NotNil(t, client.events)
	derived := client.events.Events[0]
	require.Equal(t, "health-events-analyzer", derived.Agent)
	require.Equal(t, "DerivedCondition", derived.CheckName)
	require.False(t, derived.IsHealthy)
	require.False(t, derived.IsFatal)

	clones := cloneEntities(source.EntitiesImpacted)
	require.Len(t, clones, 1)
	require.True(t, proto.Equal(
		&protos.Entity{EntityType: "GPU_UUID", EntityValue: "GPU-1"}, clones[0],
	))
}

func TestPublishRetryHonorsContextDeadline(t *testing.T) {
	pub := NewPublisher(&unavailablePlatformConnector{}, protos.ProcessingStrategy_EXECUTE_REMEDIATION)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := pub.Publish(ctx, &protos.HealthEvent{}, protos.RecommendedAction_NONE,
		"DerivedCondition", "derived", nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.DeadlineExceeded), err)
}

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
		published.GetMetadata()[SourceGeneratedTimestampMetadataKey])
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
		published.GetMetadata()[SourceGeneratedTimestampMetadataKey])
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
	require.NotContains(t, published.GetMetadata(), SourceGeneratedTimestampMetadataKey)
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
	require.NotContains(t, src.GetMetadata(), SourceGeneratedTimestampMetadataKey)
}
