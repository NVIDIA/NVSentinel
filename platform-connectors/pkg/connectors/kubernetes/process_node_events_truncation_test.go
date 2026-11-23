// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// gpuEntities mirrors the entities gpu-health-monitor attaches to a DCGM watch
// event for GPU i on an 8-GPU node: index, PCI address and GPU UUID.
func gpuEntities(i int) []*protos.Entity {
	return []*protos.Entity{
		{EntityType: "GPU", EntityValue: fmt.Sprintf("%d", i)},
		{EntityType: "PCI", EntityValue: fmt.Sprintf("0000:%02x:00.0", 0x17+i*8)},
		{EntityType: "GPU_UUID", EntityValue: fmt.Sprintf("GPU-8614c5d9-371d-1d8a-9bab-78d04344270%d", i)},
	}
}

func gpuFaultEvent(check, code string, action protos.RecommendedAction, i int, ts int64) *protos.HealthEvent {
	return &protos.HealthEvent{
		Agent:              "gpu-health-monitor",
		ComponentClass:     "GPU",
		CheckName:          check,
		IsFatal:            true,
		ErrorCode:          []string{code},
		EntitiesImpacted:   gpuEntities(i),
		Message:            fmt.Sprintf("Detected %s for GPU %d", code, i),
		RecommendedAction:  action,
		NodeName:           "node",
		GeneratedTimestamp: timestamppb.New(time.Unix(ts, 0)),
	}
}

func gpuRecoveryEvent(check string, i int, ts int64) *protos.HealthEvent {
	return &protos.HealthEvent{
		Agent:              "gpu-health-monitor",
		ComponentClass:     "GPU",
		CheckName:          check,
		IsHealthy:          true,
		ErrorCode:          []string{},
		EntitiesImpacted:   gpuEntities(i),
		Message:            "watch reported no errors",
		RecommendedAction:  protos.RecommendedAction_NONE,
		NodeName:           "node",
		GeneratedTimestamp: timestamppb.New(time.Unix(ts, 0)),
	}
}

func TestShortenIdentityForSpace(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want string
	}{
		{
			name: "GPU_UUID is shortened when GPU and PCI are present, diagnostic text dropped",
			msg:  "ErrorCode:X GPU:3 PCI:0000:c4:00.0 GPU_UUID:GPU-8614c5d9-371d-1d8a-9bab-78d0434427ec some text Recommended Action=RESTART_VM",
			want: "ErrorCode:X GPU:3 PCI:0000:c4:00.0 GPU_UUID:GPU-8614... ... Recommended Action=RESTART_VM",
		},
		{
			name: "GPU_UUID is kept when it is the only entity",
			msg:  "ErrorCode:X GPU_UUID:GPU-8614c5d9-371d-1d8a-9bab-78d0434427ec some text Recommended Action=RESTART_VM",
			want: "ErrorCode:X GPU_UUID:GPU-8614c5d9-371d-1d8a-9bab-78d0434427ec ... Recommended Action=RESTART_VM",
		},
		{
			name: "entry without a Recommended Action is unchanged",
			msg:  "free text only",
			want: "free text only",
		},
		{
			name: "entry without diagnostic text or UUID is unchanged",
			msg:  "ErrorCode:X GPU:3 PCI:0000:c4:00.0 Recommended Action=RESTART_VM",
			want: "ErrorCode:X GPU:3 PCI:0000:c4:00.0 Recommended Action=RESTART_VM",
		},
		{
			name: "diagnostic shorter than the truncation suffix is unchanged",
			msg:  "ErrorCode:X GPU:3 PCI:0000:c4:00.0 x Recommended Action=RESTART_VM",
			want: "ErrorCode:X GPU:3 PCI:0000:c4:00.0 x Recommended Action=RESTART_VM",
		},
		{
			name: "rewriting without saving space keeps the original diagnostic",
			msg:  "ErrorCode:X GPU:3 PCI:0000:c4:00.0 err Recommended Action=RESTART_VM",
			want: "ErrorCode:X GPU:3 PCI:0000:c4:00.0 err Recommended Action=RESTART_VM",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shortenIdentityForSpace(tc.msg)
			assert.Equal(t, tc.want, got)
			assert.LessOrEqual(t, len(got), len(tc.msg), "compaction must preserve or reduce the byte length")

			if tc.want != tc.msg {
				assert.True(t, messagesMatchByIdentity(tc.msg, got), "shortened entry must still match its full form")
			}
		})
	}
}

func TestProcessNodeCondition_EmptyDiagnosticsStillClearAfterRecovery(t *testing.T) {
	connector := &K8sConnector{config: K8sConnectorConfig{MaxNodeConditionMessageLength: 1024, CompactedHealthEventMsgLen: 72}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}}

	const check = "GpuDriverWatch"

	var faults, recoveries []*protos.HealthEvent

	for i := range 16 {
		fault := gpuFaultEvent(check, fmt.Sprintf("%04d", i), protos.RecommendedAction_RESTART_VM, i%8, 1000+int64(i))
		fault.EntitiesImpacted = fault.EntitiesImpacted[:2]
		fault.Message = ""
		faults = append(faults, fault)
	}

	for i := range 8 {
		recovery := gpuRecoveryEvent(check, i, 2000+int64(i))
		recovery.EntitiesImpacted = recovery.EntitiesImpacted[:2]
		recoveries = append(recoveries, recovery)
	}

	connector.processNodeCondition(context.Background(), node, check, faults)
	require.Len(t, node.Status.Conditions, 1)
	require.Equal(t, corev1.ConditionTrue, node.Status.Conditions[0].Status)
	require.LessOrEqual(t, len(node.Status.Conditions[0].Message), 1024)

	connector.processNodeCondition(context.Background(), node, check, recoveries)

	condition := node.Status.Conditions[0]
	assert.Equal(t, corev1.ConditionFalse, condition.Status,
		"all GPUs recovered but compaction left message %q", condition.Message)
	assert.Equal(t, NoHealthFailureMsg, condition.Message)
}

// Every fault must stay in the message with the tokens a per-GPU recovery
// matches on, even when the entries have to be shrunk to fit the limit.
func TestTruncateConditionMessage_KeepsEveryEntryMatchable(t *testing.T) {
	connector := &K8sConnector{config: K8sConnectorConfig{MaxNodeConditionMessageLength: 1024, CompactedHealthEventMsgLen: 72}}

	var messages []string
	for i := range 8 {
		messages = append(messages,
			connector.constructHealthEventMessage(gpuFaultEvent("GpuPowerWatch", "DCGM_FR_POWER_UNREADABLE",
				protos.RecommendedAction_RUN_FIELDDIAG, i, int64(i))))
	}

	for i, msg := range messages {
		messages[i] = strings.TrimSuffix(msg, ";")
	}

	result := connector.truncateNodeConditionMessage(messages)
	require.LessOrEqual(t, len(result), 1024)

	stored := parseMessages(result)
	require.Len(t, stored, 8, "every fault must stay in the message: %q", result)

	for i, entry := range stored {
		assert.Contains(t, entry, fmt.Sprintf(" GPU:%d ", i), "entry must keep its GPU token: %q", entry)
		assert.Contains(t, entry, " PCI:0000:", "entry must keep its PCI token: %q", entry)
		assert.True(t, messagesMatchByIdentity(messages[i], entry), "entry must still match a repeat of the same fault: %q", entry)
	}
}

// Eight GPUs fault on one watch. While one GPU is still faulty the condition
// must stay True and keep naming it; once every GPU recovers it must return to
// False, whatever the error code and recommended action, even though the stored
// message had to be shrunk to fit the limit.
func TestProcessNodeCondition_ClearsAfterAllGPUsRecoverWhenTruncated(t *testing.T) {
	connector := &K8sConnector{config: K8sConnectorConfig{MaxNodeConditionMessageLength: 1024, CompactedHealthEventMsgLen: 72}}

	tests := []struct {
		code   string
		action protos.RecommendedAction
	}{
		{"DCGM_FR_POWER_UNREADABLE", protos.RecommendedAction_RUN_FIELDDIAG},
		{"DCGM_FR_POWER_UNREADABLE", protos.RecommendedAction_RESTART_VM},
		{"DCGM_FR_GPU_RECOVERY_REBOOT", protos.RecommendedAction_RESTART_BM},
		{"DCGM_FR_NVLINK_DOWN", protos.RecommendedAction_CONTACT_SUPPORT},
		{"DCGM_FR_NVLINK_DOWN", protos.RecommendedAction_RESTART_VM},
		{"DCGM_FR_XID_ERROR", protos.RecommendedAction_RESTART_VM},
		{"DCGM_FR_THERMAL_VIOLATION", protos.RecommendedAction_RESTART_VM},
		{"DCGM_FR_UNCONTAINED_ERROR", protos.RecommendedAction_RESTART_VM},
	}

	for _, tc := range tests {
		t.Run(tc.code+"/"+tc.action.String(), func(t *testing.T) {
			const check = "GpuDriverWatch"

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}}

			var faults, recoveries []*protos.HealthEvent
			for i := range 8 {
				faults = append(faults, gpuFaultEvent(check, tc.code, tc.action, i, 1000+int64(i)))
				recoveries = append(recoveries, gpuRecoveryEvent(check, i, 2000+int64(i)))
			}

			connector.processNodeCondition(context.Background(), node, check, faults)
			require.Len(t, node.Status.Conditions, 1)
			require.Equal(t, corev1.ConditionTrue, node.Status.Conditions[0].Status)
			require.LessOrEqual(t, len(node.Status.Conditions[0].Message), 1024)

			connector.processNodeCondition(context.Background(), node, check, recoveries[:7])

			partial := node.Status.Conditions[0]
			require.Equal(t, corev1.ConditionTrue, partial.Status,
				"GPU 7 has not recovered but the condition cleared; message %q", partial.Message)
			require.Contains(t, partial.Message, "GPU:7 ", "the remaining fault must still name GPU 7: %q", partial.Message)

			connector.processNodeCondition(context.Background(), node, check, recoveries[7:])

			condition := node.Status.Conditions[0]
			assert.Equal(t, corev1.ConditionFalse, condition.Status,
				"all GPUs recovered but the condition kept message %q", condition.Message)
			assert.Equal(t, NoHealthFailureMsg, condition.Message)
		})
	}
}
