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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

const efaPatternsTOML = `
[[nicDriverDetection.patterns]]
name = "efa_admin_cmd_timeout"
enabled = true

[[nicDriverDetection.patterns]]
name = "efa_admin_queue_closed"
enabled = true

[[nicDriverDetection.patterns]]
name = "efa_admin_cmd_failed"
enabled = true

[[nicDriverDetection.patterns]]
name = "efa_device_reset_failed"
enabled = true

[[nicDriverDetection.patterns]]
name = "efa_device_not_ready"
enabled = true
`

func loadEFAPatterns(t *testing.T) map[string]CompiledPattern {
	t.Helper()

	path := writeTOML(t, t.TempDir(), efaPatternsTOML)

	patterns, err := LoadConfig(path)
	require.NoError(t, err)
	require.Len(t, patterns, 5)

	byName := make(map[string]CompiledPattern, len(patterns))
	for _, p := range patterns {
		byName[p.Name] = p
	}

	return byName
}

func TestLoadConfig_EFAPatterns(t *testing.T) {
	patterns := loadEFAPatterns(t)

	cases := []struct {
		name    string
		fatal   bool
		action  pb.RecommendedAction
		matches []string
		rejects []string
	}{
		{
			name:   "efa_admin_cmd_timeout",
			fatal:  true,
			action: pb.RecommendedAction_REPLACE_VM,
			matches: []string{
				"infiniband rdmap0s6: Wait for completion (polling) timeout",
				"kernel: infiniband rdmap16s27: The device didn't send any completion for admin cmd " +
					"CREATE_QP(6) status 0 (ctx 0x000000001d2f9d31, sq producer: 12, sq consumer: 11, cq consumer: 11)",
				"infiniband efa_0: Wait for completion (polling) timeout",
				"efa 0000:00:06.0: Wait for completion (polling) timeout",
			},
			rejects: []string{
				"ena 0000:00:05.0 eth0: Wait for completion (polling) timeout",
				"infiniband mlx5_0: Wait for completion (polling) timeout",
			},
		},
		{
			name:   "efa_admin_queue_closed",
			fatal:  true,
			action: pb.RecommendedAction_REPLACE_VM,
			matches: []string{
				"infiniband rdmap0s6: Admin queue is closed",
			},
			rejects: []string{
				"ena 0000:00:05.0 eth0: Admin queue is closed",
			},
		},
		{
			name:   "efa_admin_cmd_failed",
			fatal:  false,
			action: pb.RecommendedAction_NONE,
			matches: []string{
				"infiniband rdmap0s6: Failed to process command CREATE_QP (opcode 6) comp_status 1 err -22",
				"infiniband rdmap0s6: Failed to submit command REG_MR (opcode 9) err -12",
			},
			rejects: []string{
				"infiniband rdmap0s6: some unrelated message",
				"Failed to process command CREATE_QP without an efa prefix",
			},
		},
		{
			name:   "efa_device_reset_failed",
			fatal:  true,
			action: pb.RecommendedAction_REPLACE_VM,
			matches: []string{
				"infiniband rdmap0s6: Reset indication didn't turn on",
				"efa 0000:00:06.0: Reset indication didn't turn off",
				"efa 0000:00:06.0: Device isn't ready, can't reset device",
			},
			rejects: []string{
				// The ENA network driver logs the same text; it must not
				// be attributed to EFA.
				"ena 0000:00:05.0: Reset indication didn't turn on",
				"ena 0000:00:05.0 eth0: Device isn't ready, can't reset device",
			},
		},
		{
			name:   "efa_device_not_ready",
			fatal:  true,
			action: pb.RecommendedAction_REPLACE_VM,
			matches: []string{
				"efa 0000:00:06.0: Device isn't ready, abort com init",
			},
			rejects: []string{
				"ena 0000:00:05.0: Device isn't ready, abort com init",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := patterns[tc.name]
			require.True(t, ok, "pattern %s must be defined", tc.name)
			assert.Equal(t, tc.fatal, p.IsFatal)
			assert.Equal(t, tc.action, p.RecommendedAction)
			assert.NotEmpty(t, p.Description)

			for _, line := range tc.matches {
				assert.True(t, p.Re.MatchString(line), "should match: %s", line)
			}

			for _, line := range tc.rejects {
				assert.False(t, p.Re.MatchString(line), "should not match: %s", line)
			}
		})
	}
}

func TestProcessLine_EFAEntityFromIBDeviceName(t *testing.T) {
	patterns := loadEFAPatterns(t)
	h := makeHandler(t, []CompiledPattern{patterns["efa_admin_cmd_timeout"]}, newMockResolver(nil))

	events, err := h.ProcessLine("infiniband rdmap16s27: Wait for completion (polling) timeout")
	require.NoError(t, err)
	require.NotNil(t, events)

	evt := events.Events[0]
	assert.True(t, evt.IsFatal)
	assert.Equal(t, "NIC", evt.ComponentClass)
	assert.Equal(t, []string{"efa_admin_cmd_timeout"}, evt.ErrorCode)
	require.Len(t, evt.EntitiesImpacted, 1, "ibdev-prefixed lines carry the RDMA device name")
	assert.Equal(t, "NIC", evt.EntitiesImpacted[0].EntityType)
	assert.Equal(t, "rdmap16s27", evt.EntitiesImpacted[0].EntityValue)
}

func TestProcessLine_EFAEntityFromBDFResolvesEFADriver(t *testing.T) {
	patterns := loadEFAPatterns(t)
	resolver := newMockResolver(map[string]mockResult{
		"0000:00:06.0": {driver: "efa", device: "rdmap0s6"},
		"0000:00:05.0": {driver: "ena", device: "eth0"},
	})
	h := makeHandler(t, []CompiledPattern{patterns["efa_device_reset_failed"]}, resolver)

	events, err := h.ProcessLine("efa 0000:00:06.0: Reset indication didn't turn on")
	require.NoError(t, err)
	require.NotNil(t, events)
	require.Len(t, events.Events[0].EntitiesImpacted, 1)
	assert.Equal(t, "rdmap0s6", events.Events[0].EntitiesImpacted[0].EntityValue)

	events, err = h.ProcessLine("ena 0000:00:05.0: Reset indication didn't turn on")
	require.NoError(t, err)
	assert.Nil(t, events, "ENA lines must not match EFA patterns")
}

func TestSysfsResolver_EFADriver(t *testing.T) {
	root := t.TempDir()
	deviceDir := filepath.Join(root, "bus", "pci", "devices", "0000:00:06.0")
	driverDir := filepath.Join(root, "bus", "pci", "drivers", "efa")
	require.NoError(t, os.MkdirAll(filepath.Join(deviceDir, "infiniband", "rdmap0s6"), 0o755))
	require.NoError(t, os.MkdirAll(driverDir, 0o755))
	require.NoError(t, os.Symlink(driverDir, filepath.Join(deviceDir, "driver")))

	driver, device, ok := NewSysfsResolver(root).Resolve("0000:00:06.0")
	require.True(t, ok)
	assert.Equal(t, "efa", driver)
	assert.Equal(t, "rdmap0s6", device)
	assert.True(t, nicDrivers[driver])
}

func TestExtractIBDevice(t *testing.T) {
	dev, ok := extractIBDevice("Sep  4 10:00:00 host kernel: infiniband rdmap0s6: Admin queue is closed")
	require.True(t, ok)
	assert.Equal(t, "rdmap0s6", dev)

	dev, ok = extractIBDevice("infiniband efa_0: Failed to process command")
	require.True(t, ok)
	assert.Equal(t, "efa_0", dev)

	_, ok = extractIBDevice("efa 0000:00:06.0: Device isn't ready, abort com init")
	assert.False(t, ok)
}
