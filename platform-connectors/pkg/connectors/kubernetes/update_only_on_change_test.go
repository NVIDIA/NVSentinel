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

package kubernetes

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// These tests run against the package's envtest API server (see TestMain), so
// the evidence is what the API server recorded, not what a fake counted: a
// node status write moves the node's resourceVersion and a skipped one does
// not; a Kubernetes Event write shows up as an Event object, and a refresh
// bumps its count.

var nonNodeNameChars = regexp.MustCompile(`[^a-zA-Z0-9.-]+`)

// envtestConnector returns a connector over the shared API server and a node
// of this test's own; the node and its Events are removed when the test ends.
func envtestConnector(t *testing.T, onlyOnChange bool, cfg ...K8sConnectorConfig) (*K8sConnector, *kubernetes.Clientset, string) {
	t.Helper()

	cli := envtestClient(t)
	ctx := context.Background()
	// Node names are DNS subdomains: lower case letters, digits, "-" and ".".
	nodeName := "node-" + strings.ToLower(nonNodeNameChars.ReplaceAllString(t.Name(), "-"))

	_, err := cli.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = cli.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
		_ = cli.CoreV1().Events(DefaultNamespace).DeleteCollection(ctx, metav1.DeleteOptions{},
			metav1.ListOptions{FieldSelector: "involvedObject.name=" + nodeName})
	})

	config := K8sConnectorConfig{
		MaxNodeConditionMessageLength: 1024,
		CompactedHealthEventMsgLen:    72,
		UpdateOnlyOnChange:            onlyOnChange,
	}
	if len(cfg) > 0 {
		config = cfg[0]
		config.UpdateOnlyOnChange = onlyOnChange
	}

	return NewK8sConnector(cli, nil, nil, ctx, config), cli, nodeName
}

// nodeVersion is the node's resourceVersion: it moves on every status write
// the API server accepted and stays put when the connector skipped the write.
func nodeVersion(t *testing.T, cli *kubernetes.Clientset, nodeName string) string {
	t.Helper()

	node, err := cli.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	require.NoError(t, err)

	return node.ResourceVersion
}

// nodeEvents lists the Events written for the node, oldest name first.
func nodeEvents(t *testing.T, cli *kubernetes.Clientset, nodeName string) []corev1.Event {
	t.Helper()

	list, err := cli.CoreV1().Events(DefaultNamespace).List(context.Background(),
		metav1.ListOptions{FieldSelector: "involvedObject.name=" + nodeName})
	require.NoError(t, err)

	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })

	return list.Items
}

// eventCount returns the count of the one Event carrying the message.
func eventCount(t *testing.T, cli *kubernetes.Clientset, nodeName, message string) int32 {
	t.Helper()

	for _, event := range nodeEvents(t, cli, nodeName) {
		if event.Message == message {
			return event.Count
		}
	}

	require.Failf(t, "Event not found", "no Event on %s with message %q", nodeName, message)

	return 0
}

func xidEvent(nodeName string, at time.Time, healthy bool) *protos.HealthEvent {
	return &protos.HealthEvent{
		CheckName:          "GpuXidError",
		IsHealthy:          healthy,
		IsFatal:            !healthy,
		EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		ErrorCode:          []string{"79"},
		GeneratedTimestamp: timestamppb.New(at),
		ComponentClass:     "GPU",
		RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
		Message:            "XID 79 on GPU 0",
		NodeName:           nodeName,
	}
}

func batch(events ...*protos.HealthEvent) *protos.HealthEvents {
	return &protos.HealthEvents{Version: 1, Events: events}
}

// TestUpdateOnlyOnChange_SkipsRepeats: the first fault is a transition and
// updates the node; the same fault again (a repeat, or a resent batch) changes
// nothing the node shows and must not cost a status write; a recovery is a
// transition again.
func TestUpdateOnlyOnChange_SkipsRepeats(t *testing.T) {
	connector, cli, node := envtestConnector(t, true)
	ctx := context.Background()
	now := time.Now()
	created := nodeVersion(t, cli, node)

	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(node, now, false))))
	afterFault := nodeVersion(t, cli, node)
	require.NotEqual(t, created, afterFault, "the first fault is a transition")

	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(node, now, false))))
	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(node, now.Add(time.Minute), false))))
	require.Equal(t, afterFault, nodeVersion(t, cli, node), "a repeat of the same fault changes nothing and is skipped")

	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(node, now.Add(2*time.Minute), true))))
	afterRecovery := nodeVersion(t, cli, node)
	require.NotEqual(t, afterFault, afterRecovery, "the recovery is a transition")

	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(node, now.Add(3*time.Minute), true))))
	require.Equal(t, afterRecovery, nodeVersion(t, cli, node), "healthy again is a repeat")
}

// TestUpdateOnlyOnChange_SaturatedMessageIsStillARepeat: when a node's
// condition message would exceed its length cap the stored entries are
// compacted, so their text never equals a repeat's full text again; the repeat
// must still count as no change, or exactly the busiest nodes would pay a
// status write on every repeat.
func TestUpdateOnlyOnChange_SaturatedMessageIsStillARepeat(t *testing.T) {
	connector, cli, node := envtestConnector(t, true, K8sConnectorConfig{
		// Tight enough that six full messages do not fit and are compacted,
		// wide enough that the six compacted ones do.
		MaxNodeConditionMessageLength: 700,
		CompactedHealthEventMsgLen:    40,
	})
	ctx := context.Background()
	now := time.Now()

	faults := make([]*protos.HealthEvent, 0, 6)
	for gpu := range 6 {
		fault := xidEvent(node, now.Add(time.Duration(gpu)*time.Second), false)
		fault.EntitiesImpacted = []*protos.Entity{{EntityType: "GPU", EntityValue: fmt.Sprint(gpu)}}
		fault.Message = fmt.Sprintf("XID 79 on GPU %d: %s", gpu, strings.Repeat("diagnostic detail ", 8))
		faults = append(faults, fault)
	}

	require.NoError(t, connector.ProcessBatch(ctx, batch(faults...)))
	afterFirst := nodeVersion(t, cli, node)

	// The same faults again, as one batch and one by one: nothing changed.
	require.NoError(t, connector.ProcessBatch(ctx, batch(faults...)))

	for _, fault := range faults {
		require.NoError(t, connector.ProcessBatch(ctx, batch(fault)))
	}

	require.Equal(t, afterFirst, nodeVersion(t, cli, node), "repeats of compacted faults are no change")
}

// TestUpdateOnlyOnChange_NewFaultJoiningIsAChange: a second fault on the same
// check adds a message, which the node does not show yet.
func TestUpdateOnlyOnChange_NewFaultJoiningIsAChange(t *testing.T) {
	connector, cli, node := envtestConnector(t, true)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(node, now, false))))
	afterFirst := nodeVersion(t, cli, node)

	second := xidEvent(node, now.Add(time.Second), false)
	second.EntitiesImpacted = []*protos.Entity{{EntityType: "GPU", EntityValue: "1"}}

	require.NoError(t, connector.ProcessBatch(ctx, batch(second)))
	afterSecond := nodeVersion(t, cli, node)
	require.NotEqual(t, afterFirst, afterSecond, "a new fault joining an existing one changes the message")

	require.NoError(t, connector.ProcessBatch(ctx, batch(second)))
	require.Equal(t, afterSecond, nodeVersion(t, cli, node))
}

// TestUpdateOnlyOnChange_OffKeepsHeartbeatUpdates: the DaemonSet keeps
// today's behavior, one status write per batch; the heartbeat time follows
// the event, so a later repeat is a real write.
func TestUpdateOnlyOnChange_OffKeepsHeartbeatUpdates(t *testing.T) {
	connector, cli, node := envtestConnector(t, false)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(node, now, false))))
	afterFirst := nodeVersion(t, cli, node)

	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(node, now.Add(time.Minute), false))))
	require.NotEqual(t, afterFirst, nodeVersion(t, cli, node), "every batch is written")
}

// thermalEvent is a non-fatal fault, the kind that is announced as a
// Kubernetes Event rather than a node condition.
func thermalEvent(nodeName string, at time.Time, healthy bool, gpu string) *protos.HealthEvent {
	return &protos.HealthEvent{
		CheckName:          "GpuThermalWatch",
		IsHealthy:          healthy,
		IsFatal:            false,
		EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: gpu}},
		ErrorCode:          []string{"THERMAL_WARNING"},
		GeneratedTimestamp: timestamppb.New(at),
		ComponentClass:     "GPU",
		RecommendedAction:  protos.RecommendedAction_NONE,
		Message:            "GPU " + gpu + " is hot",
		NodeName:           nodeName,
	}
}

// TestUpdateOnlyOnChange_EventsWrittenOnChange: a fault's first report
// creates its Event; repeats write nothing; a fault on another GPU is a
// change; after the check recovers, the same fault is announced again by
// refreshing the Event that still exists in the cluster.
func TestUpdateOnlyOnChange_EventsWrittenOnChange(t *testing.T) {
	connector, cli, node := envtestConnector(t, true)
	ctx := context.Background()
	now := time.Now()
	hot0 := connector.fetchHealthEventMessage(thermalEvent(node, now, false, "0"))

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now, false, "0"))))
	require.Len(t, nodeEvents(t, cli, node), 1, "the first report of a fault creates its Event")

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now, false, "0"))))
	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(time.Minute), false, "0"))))
	require.Len(t, nodeEvents(t, cli, node), 1, "a repeat, or a resent batch, writes nothing")
	require.Equal(t, int32(1), eventCount(t, cli, node, hot0))

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(time.Minute), false, "1"))))
	require.Len(t, nodeEvents(t, cli, node), 2, "a fault on another GPU is a change")

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(2*time.Minute), true, "0"))))
	require.Len(t, nodeEvents(t, cli, node), 2, "a recovery writes no Event")

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(3*time.Minute), false, "0"))))
	require.Len(t, nodeEvents(t, cli, node), 2, "the fault's return reuses its Event, which still exists in the cluster")
	require.Equal(t, int32(2), eventCount(t, cli, node, hot0), "the return after a recovery is announced by refreshing that Event")
}

// TestUpdateOnlyOnChange_EventRefreshedAfterInterval: once the refresh
// interval has passed, the next repeat refreshes the existing Event (count and
// timestamp) instead of creating another one.
func TestUpdateOnlyOnChange_EventRefreshedAfterInterval(t *testing.T) {
	connector, cli, node := envtestConnector(t, true)
	ctx := context.Background()
	now := time.Now()
	hot0 := connector.fetchHealthEventMessage(thermalEvent(node, now, false, "0"))

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now, false, "0"))))
	require.Len(t, nodeEvents(t, cli, node), 1)

	k8sEvent := connector.createK8sEvent(ctx, thermalEvent(node, now, false, "0"))

	connector.nodeEventMu.Lock()
	written, ok := connector.nodeEventMemory().Get(nodeCheckKey(node, k8sEvent.Type))
	require.True(t, ok)
	remembered := written[k8sEvent.Message]
	remembered.writtenAt = now.Add(-nodeEventRefreshInterval - time.Second)
	written[k8sEvent.Message] = remembered
	connector.nodeEventMu.Unlock()

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(time.Minute), false, "0"))))
	require.Len(t, nodeEvents(t, cli, node), 1, "the refresh reuses the existing Event")
	require.Equal(t, int32(2), eventCount(t, cli, node, hot0), "the refresh bumps its count")

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(2*time.Minute), false, "0"))))
	require.Equal(t, int32(2), eventCount(t, cli, node, hot0), "the refresh restarts the interval")
}

// TestUpdateOnlyOnChange_OffBumpsEventCount: the DaemonSet keeps today's
// behavior, every repeat bumps the Event's count.
func TestUpdateOnlyOnChange_OffBumpsEventCount(t *testing.T) {
	connector, cli, node := envtestConnector(t, false)
	ctx := context.Background()
	now := time.Now()
	hot0 := connector.fetchHealthEventMessage(thermalEvent(node, now, false, "0"))

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now, false, "0"))))
	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now, false, "0"))))
	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(time.Minute), true, "0"))))
	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(2*time.Minute), false, "0"))))
	require.Len(t, nodeEvents(t, cli, node), 1)
	require.Equal(t, int32(3), eventCount(t, cli, node, hot0), "repeats bump the count, also after a recovery")
}

// TestUpdateOnlyOnChange_EventsFollowTimestampOrder: a batch is processed in
// timestamp order, like the condition path, so an older recovery that arrives
// after a newer fault in the same batch does not erase the memory of that
// fault, which would announce it again on its next repeat.
func TestUpdateOnlyOnChange_EventsFollowTimestampOrder(t *testing.T) {
	connector, cli, node := envtestConnector(t, true)
	ctx := context.Background()
	now := time.Now()
	hot0 := connector.fetchHealthEventMessage(thermalEvent(node, now, false, "0"))

	// Wire order: the fault first, then a recovery that is a minute older.
	require.NoError(t, connector.ProcessBatch(ctx, batch(
		thermalEvent(node, now.Add(time.Minute), false, "0"),
		thermalEvent(node, now, true, "0"),
	)))
	require.Len(t, nodeEvents(t, cli, node), 1, "the fault, the latest word on GPU 0, is announced")

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(2*time.Minute), false, "0"))))
	require.Len(t, nodeEvents(t, cli, node), 1)
	require.Equal(t, int32(1), eventCount(t, cli, node, hot0), "the fault is still remembered: the older recovery did not erase it")
}

// TestUpdateOnlyOnChange_PartialRecoveryKeepsOtherFaults: a recovery names
// the entities that recovered, so only their Events are forgotten; a fault on
// another entity of the same check stays a repeat.
func TestUpdateOnlyOnChange_PartialRecoveryKeepsOtherFaults(t *testing.T) {
	connector, cli, node := envtestConnector(t, true)
	ctx := context.Background()
	now := time.Now()
	hot0 := connector.fetchHealthEventMessage(thermalEvent(node, now, false, "0"))
	hot1 := connector.fetchHealthEventMessage(thermalEvent(node, now, false, "1"))

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now, false, "0"))))
	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now, false, "1"))))
	require.Len(t, nodeEvents(t, cli, node), 2)

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(time.Minute), true, "0"))))

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(2*time.Minute), false, "1"))))
	require.Len(t, nodeEvents(t, cli, node), 2, "GPU 1 is still the same fault; GPU 0 recovering does not re-announce it")
	require.Equal(t, int32(1), eventCount(t, cli, node, hot1))

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(3*time.Minute), false, "0"))))
	require.Len(t, nodeEvents(t, cli, node), 2)
	require.Equal(t, int32(2), eventCount(t, cli, node, hot0), "GPU 0 faulting again after its recovery is announced by refreshing its Event")

	// A recovery naming no entity clears the whole check.
	recoveredAll := thermalEvent(node, now.Add(4*time.Minute), true, "0")
	recoveredAll.EntitiesImpacted = nil
	require.NoError(t, connector.ProcessBatch(ctx, batch(recoveredAll)))

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(5*time.Minute), false, "1"))))
	require.Len(t, nodeEvents(t, cli, node), 2)
	require.Equal(t, int32(2), eventCount(t, cli, node, hot1), "GPU 1 is announced again the same way")
}

// TestNodeEventMemory_BoundsMessagesPerCheck: the message is producer
// controlled, so the Events remembered for one check on one node are bounded;
// past the bound the check's memory is dropped and starts again with the
// entry being written. A refresh of a message already remembered is not a
// new entry and must not cost the others their memory.
func TestNodeEventMemory_BoundsMessagesPerCheck(t *testing.T) {
	connector := &K8sConnector{}
	remembered := func(message string) bool {
		_, ok := connector.rememberedNodeEvent("node-a", &corev1.Event{Type: "check", Message: message})

		return ok
	}

	for i := range maxRememberedMessagesPerCheck {
		connector.rememberNodeEvent("node-a", &corev1.Event{Type: "check", Message: fmt.Sprintf("message-%d", i)}, nil)
	}

	// The memory is full; refreshing a known message keeps every other one.
	connector.rememberNodeEvent("node-a", &corev1.Event{Type: "check", Message: "message-0"}, nil)
	require.True(t, remembered("message-0"))
	require.True(t, remembered(fmt.Sprintf("message-%d", maxRememberedMessagesPerCheck-1)), "a refresh at capacity keeps the other messages")

	for i := maxRememberedMessagesPerCheck; i < maxRememberedMessagesPerCheck+8; i++ {
		connector.rememberNodeEvent("node-a", &corev1.Event{Type: "check", Message: fmt.Sprintf("message-%d", i)}, nil)
	}

	connector.nodeEventMu.Lock()
	written, ok := connector.nodeEventMemory().Get(nodeCheckKey("node-a", "check"))
	connector.nodeEventMu.Unlock()

	require.True(t, ok)
	require.LessOrEqual(t, len(written), maxRememberedMessagesPerCheck)
	require.True(t, remembered(fmt.Sprintf("message-%d", maxRememberedMessagesPerCheck+7)), "the newest entry is kept")
	require.False(t, remembered("message-0"), "the memory was dropped when a new message arrived at capacity")
}

// TestNodeEventName_DerivedFromTheFault: the same fault gets the same name on
// every replica and across restarts; a different message is a different
// Event.
func TestNodeEventName_DerivedFromTheFault(t *testing.T) {
	connector, _, node := envtestConnector(t, true)
	ctx := context.Background()
	now := time.Now()

	first := connector.createK8sEvent(ctx, thermalEvent(node, now, false, "0"))
	again := connector.createK8sEvent(ctx, thermalEvent(node, now.Add(time.Hour), false, "0"))
	other := connector.createK8sEvent(ctx, thermalEvent(node, now, false, "1"))

	require.Equal(t, first.Name, again.Name, "the time of the report does not change the name")
	require.NotEqual(t, first.Name, other.Name, "another GPU is another Event")
	require.Regexp(t, `^`+node+`\.[0-9a-f]{16}$`, first.Name)
}

// TestNodeEvents_ReplicaWithoutMemoryRefreshesTheExistingEvent: a replica that
// has never seen a fault (or lost its memory of it) finds the Event another
// replica wrote and bumps it instead of writing a second one; the API server
// answers its create with AlreadyExists for real. The same holds for the
// DaemonSet after a restart.
func TestNodeEvents_ReplicaWithoutMemoryRefreshesTheExistingEvent(t *testing.T) {
	for _, onlyOnChange := range []bool{true, false} {
		t.Run(fmt.Sprintf("onlyOnChange=%v", onlyOnChange), func(t *testing.T) {
			first, cli, node := envtestConnector(t, onlyOnChange)
			ctx := context.Background()
			now := time.Now()
			hot0 := first.fetchHealthEventMessage(thermalEvent(node, now, false, "0"))

			require.NoError(t, first.ProcessBatch(ctx, batch(thermalEvent(node, now, false, "0"))))

			// Another replica, or the same process after a restart: empty memory.
			second := NewK8sConnector(cli, nil, nil, ctx, first.config)
			require.NoError(t, second.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(time.Minute), false, "0"))))

			require.Len(t, nodeEvents(t, cli, node), 1, "one Event per fault, however many replicas saw it")
			require.Equal(t, int32(2), eventCount(t, cli, node, hot0), "the second replica bumped the existing Event")

			_, known := second.rememberedNodeEvent(node, second.createK8sEvent(ctx, thermalEvent(node, now, false, "0")))
			require.True(t, known, "the second replica now remembers the Event it refreshed")
		})
	}
}
