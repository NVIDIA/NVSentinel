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

package nicdriver

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func makeHandler(t *testing.T, patterns []CompiledPattern, resolver Resolver) *NICDriverHandler {
	t.Helper()

	return newWithDeps("test-node", "syslog-health-monitor", "SysLogsNICDriverError",
		patterns, resolver, pb.ProcessingStrategy_EXECUTE_REMEDIATION)
}

type mockResolver struct {
	results map[string]mockResult
}

type mockResult struct {
	driver string
	device string
}

func newMockResolver(results map[string]mockResult) Resolver {
	return &mockResolver{results: results}
}

func (m *mockResolver) Resolve(bdf string) (string, string, bool) {
	r, ok := m.results[bdf]
	if !ok {
		return "", "", false
	}

	return r.driver, r.device, true
}

var defaultTestPatterns = []CompiledPattern{
	{
		Name:                  "cmd_exec_timeout",
		Re:                    regexp.MustCompile(`mlx5_core.*timeout\. Will cause a leak of a command resource`),
		IsFatal:               true,
		RecommendedAction:     pb.RecommendedAction_REPLACE_VM,
		ProcessingStrategy:    pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		HasProcessingStrategy: true,
	},
	{
		Name:                  "health_poll_failed",
		Re:                    regexp.MustCompile(`mlx5_core.*device's health compromised.*reached miss count`),
		IsFatal:               true,
		RecommendedAction:     pb.RecommendedAction_REPLACE_VM,
		ProcessingStrategy:    pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		HasProcessingStrategy: true,
	},
	{
		Name:                  "unrecoverable_err",
		Re:                    regexp.MustCompile(`mlx5_core.*unrecoverable hardware error`),
		IsFatal:               true,
		RecommendedAction:     pb.RecommendedAction_REPLACE_VM,
		ProcessingStrategy:    pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		HasProcessingStrategy: true,
	},
	{
		Name: "netdev_watchdog",
		Re: regexp.MustCompile(
			`(NETDEV WATCHDOG.*mlx5_core|mlx5_core.*NETDEV WATCHDOG).*transmit queue.*timed out`),
		IsFatal:               false,
		RecommendedAction:     pb.RecommendedAction_NONE,
		ProcessingStrategy:    pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		HasProcessingStrategy: true,
	},
	{
		Name:                  "mlx5_tx_timeout_detected",
		Re:                    regexp.MustCompile(`mlx5_core.*TX timeout detected`),
		IsFatal:               false,
		RecommendedAction:     pb.RecommendedAction_NONE,
		ProcessingStrategy:    pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		HasProcessingStrategy: true,
	},
	{
		Name:                  "mlx5_rx_timeout_detected",
		Re:                    regexp.MustCompile(`mlx5_core.*RX timeout on channel`),
		IsFatal:               false,
		RecommendedAction:     pb.RecommendedAction_NONE,
		ProcessingStrategy:    pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		HasProcessingStrategy: true,
	},
	{
		Name:                  "pci_power_insufficient",
		Re:                    regexp.MustCompile(`mlx5_core.*Detected insufficient power on the PCIe slot`),
		IsFatal:               false,
		RecommendedAction:     pb.RecommendedAction_NONE,
		ProcessingStrategy:    pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		HasProcessingStrategy: true,
	},
	{
		Name:                  "port_module_high_temp",
		Re:                    regexp.MustCompile(`mlx5_core.*Port module event.*High Temperature`),
		IsFatal:               false,
		RecommendedAction:     pb.RecommendedAction_NONE,
		ProcessingStrategy:    pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		HasProcessingStrategy: true,
	},
	{
		Name:                  "access_reg_failed",
		Re:                    regexp.MustCompile(`mlx5_cmd_out_err.*ACCESS_REG.*failed`),
		IsFatal:               false,
		RecommendedAction:     pb.RecommendedAction_NONE,
		ProcessingStrategy:    pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		HasProcessingStrategy: true,
	},
	{
		Name:                  "module_unplugged",
		Re:                    regexp.MustCompile(`mlx5_core.*Port module event.*Cable unplugged`),
		IsFatal:               false,
		RecommendedAction:     pb.RecommendedAction_NONE,
		ProcessingStrategy:    pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		HasProcessingStrategy: true,
	},
}

func TestProcessLine_FatalPatterns(t *testing.T) {
	resolver := newMockResolver(map[string]mockResult{
		"0000:03:00.0": {driver: "mlx5_core", device: "mlx5_0"},
		"0000:d2:00.0": {driver: "mlx5_core", device: "mlx5_1"},
	})
	h := makeHandler(t, defaultTestPatterns, resolver)

	tests := []struct {
		name        string
		message     string
		wantPattern string
		wantFatal   bool
		wantAction  pb.RecommendedAction
	}{
		{
			name:        "cmd_exec_timeout",
			message:     "mlx5_core 0000:03:00.0: wait_func:1195:(pid 1967079): ENABLE_HCA(0x104) timeout. Will cause a leak of a command resource",
			wantPattern: "cmd_exec_timeout",
			wantFatal:   true,
			wantAction:  pb.RecommendedAction_REPLACE_VM,
		},
		{
			name:        "health_poll_failed",
			message:     "mlx5_core 0000:d2:00.0: poll_health:825: device's health compromised - reached miss count.",
			wantPattern: "health_poll_failed",
			wantFatal:   true,
			wantAction:  pb.RecommendedAction_REPLACE_VM,
		},
		{
			name:        "unrecoverable_err",
			message:     "mlx5_core: INFO: synd 0x8: unrecoverable hardware error.",
			wantPattern: "unrecoverable_err",
			wantFatal:   true,
			wantAction:  pb.RecommendedAction_REPLACE_VM,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events, err := h.ProcessLine(tc.message)
			require.NoError(t, err)
			require.NotNil(t, events)
			require.Len(t, events.Events, 1)

			event := events.Events[0]
			assert.Equal(t, tc.wantFatal, event.IsFatal)
			assert.False(t, event.IsHealthy)
			assert.Equal(t, tc.wantAction, event.RecommendedAction)
			assert.Equal(t, componentClassNIC, event.ComponentClass)
			assert.Equal(t, []string{tc.wantPattern}, event.ErrorCode)
			assert.Equal(t, tc.message, event.Message)
			assert.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, event.ProcessingStrategy)
		})
	}
}

func TestProcessLine_NonFatalPatterns(t *testing.T) {
	resolver := newMockResolver(map[string]mockResult{
		"0000:12:00.0": {driver: "mlx5_core", device: "mlx5_0"},
		"0000:41:00.1": {driver: "mlx5_core", device: "mlx5_1"},
	})
	h := makeHandler(t, defaultTestPatterns, resolver)

	tests := []struct {
		name        string
		message     string
		wantPattern string
	}{
		{
			name:        "netdev_watchdog",
			message:     "NETDEV WATCHDOG: eth0 (mlx5_core): transmit queue 0 timed out",
			wantPattern: "netdev_watchdog",
		},
		{
			name:        "netdev_watchdog_post_v6.8_format",
			message:     "mlx5_core 0000:65:00.0 ens15np0: NETDEV WATCHDOG: CPU: 94: transmit queue 3 timed out 5032 ms",
			wantPattern: "netdev_watchdog",
		},
		{
			name:        "mlx5_tx_timeout_detected",
			message:     "mlx5_core 0000:65:00.0 ens15np0: TX timeout detected",
			wantPattern: "mlx5_tx_timeout_detected",
		},
		{
			name:        "mlx5_rx_timeout_detected",
			message:     "mlx5_core 0000:65:00.0 ens15np0: RX timeout on channel: 20, ICOSQ: 0x1ee0, RQ: 0x1e43, CQ: 0x3ea6",
			wantPattern: "mlx5_rx_timeout_detected",
		},
		{
			name:        "pci_power_insufficient",
			message:     "mlx5_core 0000:12:00.0: mlx5_pcie_event:299: Detected insufficient power on the PCIe slot (27W).",
			wantPattern: "pci_power_insufficient",
		},
		{
			name:        "port_module_high_temp",
			message:     "mlx5_core 0000:5c:00.0: Port module event[error]: module 0, Cable error, High Temperature",
			wantPattern: "port_module_high_temp",
		},
		{
			name:        "access_reg_failed",
			message:     "mlx5_cmd_out_err:838:(pid 1441871): ACCESS_REG(0x805) op_mod(0x1) failed, status bad operation(0x2), syndrome (0x305684), err(-22)",
			wantPattern: "access_reg_failed",
		},
		{
			name:        "module_unplugged",
			message:     "mlx5_core 0000:41:00.1: Port module event: module 1, Cable unplugged",
			wantPattern: "module_unplugged",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events, err := h.ProcessLine(tc.message)
			require.NoError(t, err)
			require.NotNil(t, events)
			require.Len(t, events.Events, 1)

			event := events.Events[0]
			assert.False(t, event.IsFatal)
			assert.Equal(t, pb.RecommendedAction_NONE, event.RecommendedAction)
			assert.Equal(t, []string{tc.wantPattern}, event.ErrorCode)
		})
	}
}

func TestProcessLine_NoMatch(t *testing.T) {
	h := makeHandler(t, defaultTestPatterns, newMockResolver(nil))

	events, err := h.ProcessLine("some random kernel message that matches nothing")
	require.NoError(t, err)
	assert.Nil(t, events)
}

// TestProcessLine_NoBDFStillEmits verifies that a regex match without a BDF
// in the line still emits an event, but without NIC entity enrichment.
func TestProcessLine_NoBDFStillEmits(t *testing.T) {
	h := makeHandler(t, defaultTestPatterns, newMockResolver(nil))

	events, err := h.ProcessLine(
		"mlx5_core: INFO: synd 0x8: unrecoverable hardware error.")
	require.NoError(t, err)
	require.NotNil(t, events, "unrecoverable_err with no BDF should still emit")
	assert.Empty(t, events.Events[0].EntitiesImpacted, "no BDF means no NIC entity")
}

// TestProcessLine_NonMlx5BDFNoEntity verifies that a regex match on a line
// whose BDF resolves to a non-mlx5 driver (e.g. nvidia, nvme) still emits
// the event but does not attach a NIC entity. This protects against an
// operator-supplied generic regex matching a GPU/NVMe log line.
func TestProcessLine_NonMlx5BDFNoEntity(t *testing.T) {
	resolver := newMockResolver(map[string]mockResult{
		"0000:b3:00.0": {driver: "nvidia", device: "gpu0"},
	})

	customPattern := []CompiledPattern{{
		Name:              "custom_generic",
		Re:                regexp.MustCompile(`some generic kernel message`),
		IsFatal:           false,
		RecommendedAction: pb.RecommendedAction_NONE,
	}}

	h := makeHandler(t, customPattern, resolver)

	events, err := h.ProcessLine("0000:b3:00.0: some generic kernel message about a GPU")
	require.NoError(t, err)
	require.NotNil(t, events, "match emits event regardless of BDF driver type")
	assert.Empty(t, events.Events[0].EntitiesImpacted,
		"non-mlx5 BDF must not be tagged with a NIC entity")
}

func TestProcessLine_EntityEnrichment(t *testing.T) {
	resolver := newMockResolver(map[string]mockResult{
		"0000:03:00.0": {driver: "mlx5_core", device: "mlx5_0"},
	})
	h := makeHandler(t, defaultTestPatterns, resolver)

	events, err := h.ProcessLine(
		"mlx5_core 0000:03:00.0: wait_func:1195:(pid 100): ENABLE_HCA(0x104) timeout. Will cause a leak of a command resource")
	require.NoError(t, err)
	require.NotNil(t, events)
	require.Len(t, events.Events[0].EntitiesImpacted, 1)

	entity := events.Events[0].EntitiesImpacted[0]
	assert.Equal(t, "NIC", entity.EntityType)
	assert.Equal(t, "mlx5_0", entity.EntityValue)
}

func TestProcessLine_ProcessingStrategyPassthrough(t *testing.T) {
	pattern := []CompiledPattern{{
		Name:              "test",
		Re:                regexp.MustCompile(`test_pattern`),
		IsFatal:           false,
		RecommendedAction: pb.RecommendedAction_NONE,
	}}

	h := newWithDeps("node", "agent", "check", pattern, newMockResolver(nil),
		pb.ProcessingStrategy_STORE_ONLY)

	events, err := h.ProcessLine("test_pattern")
	require.NoError(t, err)
	require.NotNil(t, events)
	assert.Equal(t, pb.ProcessingStrategy_STORE_ONLY, events.Events[0].ProcessingStrategy)
}

func TestProcessLine_PatternProcessingStrategyOverridesHandlerDefault(t *testing.T) {
	pattern := []CompiledPattern{{
		Name:                  "test",
		Re:                    regexp.MustCompile(`test_pattern`),
		IsFatal:               false,
		RecommendedAction:     pb.RecommendedAction_NONE,
		ProcessingStrategy:    pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		HasProcessingStrategy: true,
	}}

	h := newWithDeps("node", "agent", "check", pattern, newMockResolver(nil),
		pb.ProcessingStrategy_STORE_ONLY)

	events, err := h.ProcessLine("test_pattern")
	require.NoError(t, err)
	require.NotNil(t, events)
	assert.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, events.Events[0].ProcessingStrategy)
}

func TestProcessLine_FirstMatchWins(t *testing.T) {
	patterns := []CompiledPattern{
		{
			Name:              "specific",
			Re:                regexp.MustCompile(`mlx5_core.*timeout\. Will cause`),
			IsFatal:           true,
			RecommendedAction: pb.RecommendedAction_REPLACE_VM,
		},
		{
			Name:              "general",
			Re:                regexp.MustCompile(`mlx5_core`),
			IsFatal:           false,
			RecommendedAction: pb.RecommendedAction_NONE,
		},
	}

	h := makeHandler(t, patterns, newMockResolver(nil))

	events, err := h.ProcessLine(
		"mlx5_core 0000:03:00.0: timeout. Will cause a leak of a command resource")
	require.NoError(t, err)
	require.NotNil(t, events)
	assert.Equal(t, "specific", events.Events[0].ErrorCode[0])
	assert.True(t, events.Events[0].IsFatal)
}
