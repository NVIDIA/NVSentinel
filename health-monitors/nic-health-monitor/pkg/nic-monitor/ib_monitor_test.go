/*
 * Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package nic_monitor

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

func TestScrapInfinibandDevices(t *testing.T) {
	sysClassInfinibandPath := "/sys/class/infiniband"
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			// mlx5_0 with port 1
			"sys/class/infiniband/mlx5_0/ports/1":            {Mode: fs.ModeDir},
			"sys/class/infiniband/mlx5_0/ports/1/phys_state": {Data: []byte("5: LinkUp")},
			"sys/class/infiniband/mlx5_0/ports/1/state":      {Data: []byte("4: ACTIVE")},
			// mlx5_1 with port 1 and 2
			"sys/class/infiniband/mlx5_1/ports/1":            {Mode: fs.ModeDir},
			"sys/class/infiniband/mlx5_1/ports/1/phys_state": {Data: []byte("5: LinkUp")},
			"sys/class/infiniband/mlx5_1/ports/1/state":      {Data: []byte("4: ACTIVE")},
			"sys/class/infiniband/mlx5_1/ports/2":            {Mode: fs.ModeDir},
			"sys/class/infiniband/mlx5_1/ports/2/phys_state": {Data: []byte("5: LinkUp")},
			"sys/class/infiniband/mlx5_1/ports/2/state":      {Data: []byte("4: ACTIVE")},
		},
	}

	expected := map[string]InfiniBandDevice{
		"mlx5_1": {
			Name: "mlx5_1",
			Ports: map[string]InfiniBandPort{
				"2": {Name: "2", State: "4: ACTIVE", PhysState: "5: LinkUp"},
				"1": {Name: "1", State: "4: ACTIVE", PhysState: "5: LinkUp"},
			},
		},
		"mlx5_0": {
			Name:  "mlx5_0",
			Ports: map[string]InfiniBandPort{"1": {Name: "1", State: "4: ACTIVE", PhysState: "5: LinkUp"}},
		},
	}

	actualDevs, err := GetInfinibandDevices(nil, sysClassInfinibandPath)
	require.NoError(t, err)
	require.NotNil(t, actualDevs)
	require.Equal(t, expected, actualDevs)
}

func TestInfinibandMonitor(t *testing.T) {
	sysClassInfinibandPath := "/sys/class/infiniband"
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			// mlx5_0 with port 1
			"sys/class/infiniband/mlx5_0/ports/1":            {Mode: fs.ModeDir},
			"sys/class/infiniband/mlx5_0/ports/1/phys_state": {Data: []byte("5: LinkUp")},
			"sys/class/infiniband/mlx5_0/ports/1/state":      {Data: []byte("4: ACTIVE")},
			"sys/class/infiniband/mlx5_0/ports/1/link_layer": {Data: []byte("InfiniBand")}, // Default link layer
		},
	}

	mockFS := fileSystem.(*MockFileSystem)

	expectedHealthyDeviceAndPort := []NicHealthEvent{
		{NicType: Infiniband, Name: "mlx5_0", Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
		{NicType: Infiniband, Name: "mlx5_0_1", Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: "InfiniBand"},
	}

	ibMonitor := &InfinibandDeviceMonitor{}
	// Initialize Devices map for the monitor
	ibMonitor.Devices = make(map[string]InfiniBandDevice)

	nicConfig := &NicMonitorConfig{ExclusionRegexes: nil, MonitorNetworkType: MonitorNetworkTypeAll, SysClassInfinibandPath: sysClassInfinibandPath}

	// mlx5_0 port 1 is up, so no error expected
	actualEvents, err := ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)
	require.ElementsMatch(t, expectedHealthyDeviceAndPort, actualEvents)

	// mlx5_0 port 1 physical link is not ready, so report nic down event
	mockFS.Fs["sys/class/infiniband/mlx5_0/ports/1/phys_state"].Data = []byte("2: Polling")

	expectedPhyStatePolling := []NicHealthEvent{
		{
			NicType:        Infiniband,
			Name:           "mlx5_0_1",
			Message:        "phys_state: 2: Polling",
			IsHealthyEvent: false,
			LinkLayer:      "InfiniBand",
		},
	}

	actualEvents, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)
	require.ElementsMatch(t, expectedPhyStatePolling, actualEvents)

	// mlx5_0 port 1 is still down, but it is not a new error, so do not report it
	expectedNoEvents := []NicHealthEvent{}
	actualEvents, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	// require.NotNil(t, actualEvents) // An empty slice is not nil
	require.ElementsMatch(t, expectedNoEvents, actualEvents)

	// mlx5_0 port 1 become up - no error reports
	mockFS.Fs["sys/class/infiniband/mlx5_0/ports/1/phys_state"].Data = []byte("5: LinkUp")
	expectedPortBecomesHealthy := []NicHealthEvent{
		{NicType: Infiniband, Name: "mlx5_0_1", Message: "Port is healthy", IsHealthyEvent: true, LinkLayer: "InfiniBand"},
	}
	actualEvents, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)
	require.ElementsMatch(t, expectedPortBecomesHealthy, actualEvents)

	// eth0 become down again, so report nic down event
	mockFS.Fs["sys/class/infiniband/mlx5_0/ports/1/phys_state"].Data = []byte("2: Polling")
	actualEvents, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)
	require.ElementsMatch(t, expectedPhyStatePolling, actualEvents)

	// mlx5_0 port 1 physical link is not ready, so report nic down event
	mockFS.Fs["sys/class/infiniband/mlx5_0/ports/1/state"].Data = []byte("1: Down")

	expectedStateDownAndPhyPolling := []NicHealthEvent{
		{
			NicType:        Infiniband,
			Name:           "mlx5_0_1",
			Message:        "state: 1: Down, phys_state: 2: Polling",
			IsHealthyEvent: false,
			LinkLayer:      "InfiniBand",
		},
	}

	actualEvents, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)
	require.ElementsMatch(t, expectedStateDownAndPhyPolling, actualEvents)

	// Test for newly discovered unhealthy port
	ibMonitor = &InfinibandDeviceMonitor{} // Reset monitor state
	ibMonitor.Devices = make(map[string]InfiniBandDevice)
	mockFS.Fs = fstest.MapFS{ // Reset FS to only one device, initially unhealthy
		"sys/class/infiniband/mlx5_new/ports/1":            {Mode: fs.ModeDir},
		"sys/class/infiniband/mlx5_new/ports/1/phys_state": {Data: []byte("1: Disabled")},
		"sys/class/infiniband/mlx5_new/ports/1/state":      {Data: []byte("1: Down")},
		"sys/class/infiniband/mlx5_new/ports/1/link_layer": {Data: []byte("InfiniBand")},
	}
	expectedNewUnhealthyPortEvents := []NicHealthEvent{
		{NicType: Infiniband, Name: "mlx5_new", Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
		{
			NicType:        Infiniband,
			Name:           "mlx5_new_1",
			Message:        "state: 1: Down, phys_state: 1: Disabled",
			IsHealthyEvent: false,
			LinkLayer:      "InfiniBand",
		},
	}
	actualEvents, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.ElementsMatch(
		t,
		expectedNewUnhealthyPortEvents,
		actualEvents,
		"Newly discovered unhealthy port events mismatch",
	)
}

func TestInfinibandMonitorWithExclusionRegexes(t *testing.T) {
	sysClassInfinibandPath := "/sys/class/infiniband"
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			// mlx5_0 with port 1
			"sys/class/infiniband/mlx5_0/ports/1":            {Mode: fs.ModeDir},
			"sys/class/infiniband/mlx5_0/ports/1/phys_state": {Data: []byte("5: LinkUp")},
			"sys/class/infiniband/mlx5_0/ports/1/state":      {Data: []byte("4: ACTIVE")},
			"sys/class/infiniband/mlx5_0/ports/1/link_layer": {Data: []byte("InfiniBand")},
			// mlx5_1 with port 1
			"sys/class/infiniband/mlx5_1/ports/1":            {Mode: fs.ModeDir},
			"sys/class/infiniband/mlx5_1/ports/1/phys_state": {Data: []byte("5: LinkUp")},
			"sys/class/infiniband/mlx5_1/ports/1/state":      {Data: []byte("4: ACTIVE")},
			"sys/class/infiniband/mlx5_1/ports/1/link_layer": {Data: []byte("InfiniBand")},
			// mlx5_2 with port 1
			"sys/class/infiniband/mlx5_2/ports/1":            {Mode: fs.ModeDir},
			"sys/class/infiniband/mlx5_2/ports/1/phys_state": {Data: []byte("5: LinkUp")},
			"sys/class/infiniband/mlx5_2/ports/1/state":      {Data: []byte("4: ACTIVE")},
			"sys/class/infiniband/mlx5_2/ports/1/link_layer": {Data: []byte("InfiniBand")},
		},
	}

	mockFS := fileSystem.(*MockFileSystem)

	expectedNoError := []NicHealthEvent{
		{NicType: Infiniband, Name: "mlx5_0", Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
		{NicType: Infiniband, Name: "mlx5_0_1", Message: "Port is healthy", IsHealthyEvent: true, LinkLayer: "InfiniBand"},
	}

	ibMonitor := &InfinibandDeviceMonitor{}

	// exclude mlx5_1 and mlx5_2
	nicConfig := &NicMonitorConfig{ExclusionRegexes: []string{"^mlx5_1$", "^mlx5_2$"}, SysClassInfinibandPath: sysClassInfinibandPath}

	// mlx5_0 port 1 is up, and mlx5_1 and mlx5_2 are excluded, so no error expected
	actualErrors, err := ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualErrors)
	require.Equal(t, expectedNoError, actualErrors)

	// update mlx5_1 state and verify it is not detected as  it is excluded
	mockFS.Fs["sys/class/infiniband/mlx5_1/ports/1/state"].Data = []byte("1: Down")
	expectedNoError = []NicHealthEvent{}
	actualErrors, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualErrors)
	require.Equal(t, expectedNoError, actualErrors)

	// udate mlx5_2 phys_state and verify it is not detected as it is excluded
	mockFS.Fs["sys/class/infiniband/mlx5_2/ports/1/phys_state"].Data = []byte("2: Polling")

	actualErrors, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualErrors)
	require.Equal(t, expectedNoError, actualErrors)

	// update mlx5_0 to have a state change, it should be detected
	mockFS.Fs["sys/class/infiniband/mlx5_0/ports/1/state"].Data = []byte("1: Down")

	expectedStateDown := []NicHealthEvent{
		{
			NicType:   Infiniband,
			Name:      "mlx5_0_1",
			Message:   "state: 1: Down",
			LinkLayer: "InfiniBand",
		},
	}

	actualErrors, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualErrors)
	require.Equal(t, expectedStateDown, actualErrors)
}

func TestInfinibandMonitorNetworkTypeFiltering(t *testing.T) {
	const sysClassInfinibandPath = "/sys/class/infiniband"
	tests := []struct {
		name                 string
		monitorNetworkType   MonitorNetworkType
		linkLayer            string // Content of the link_layer file for the port
		initialPortState     string
		initialPortPhysState string
		expectedEvents       []NicHealthEvent
	}{
		{
			name:                 "MonitorTypeAll_LinkLayerIB_Healthy",
			monitorNetworkType:   MonitorNetworkTypeAll,
			linkLayer:            "InfiniBand",
			initialPortState:     "4: ACTIVE",
			initialPortPhysState: "5: LinkUp",
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
				{Name: "mlx5_test_1", NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: "InfiniBand"},
			},
		},
		{
			name:                 "MonitorTypeAll_LinkLayerEth_Healthy",
			monitorNetworkType:   MonitorNetworkTypeAll,
			linkLayer:            "Ethernet",
			initialPortState:     "4: ACTIVE",
			initialPortPhysState: "5: LinkUp",
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
				{Name: "mlx5_test_1", NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: "Ethernet"},
			},
		},
		{
			name:                 "MonitorTypeRoCE_LinkLayerEth_Healthy",
			monitorNetworkType:   MonitorNetworkTypeRoCE,
			linkLayer:            "Ethernet",
			initialPortState:     "4: ACTIVE",
			initialPortPhysState: "5: LinkUp",
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
				{Name: "mlx5_test_1", NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: "Ethernet"},
			},
		},
		{
			name:                 "MonitorTypeRoCE_LinkLayerIB_Healthy_ShouldBeSkipped",
			monitorNetworkType:   MonitorNetworkTypeRoCE,
			linkLayer:            "InfiniBand",
			initialPortState:     "4: ACTIVE",
			initialPortPhysState: "5: LinkUp",
			expectedEvents: []NicHealthEvent{ // Only device detection, port is skipped
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
			},
		},
		{
			name:                 "MonitorTypeInfiniBand_LinkLayerIB_Healthy",
			monitorNetworkType:   MonitorNetworkTypeInfiniBand,
			linkLayer:            "InfiniBand",
			initialPortState:     "4: ACTIVE",
			initialPortPhysState: "5: LinkUp",
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
				{Name: "mlx5_test_1", NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: "InfiniBand"},
			},
		},
		{
			name:                 "MonitorTypeInfiniBand_LinkLayerEth_Healthy_ShouldBeSkipped",
			monitorNetworkType:   MonitorNetworkTypeInfiniBand,
			linkLayer:            "Ethernet",
			initialPortState:     "4: ACTIVE",
			initialPortPhysState: "5: LinkUp",
			expectedEvents: []NicHealthEvent{ // Only device detection, port is skipped
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
			},
		},
		{
			name:                 "MonitorTypeRoCE_LinkLayerEth_NewUnhealthyPort",
			monitorNetworkType:   MonitorNetworkTypeRoCE,
			linkLayer:            "Ethernet",
			initialPortState:     "1: DOWN",
			initialPortPhysState: "2: Polling",
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
				{
					Name:           "mlx5_test_1",
					NicType:        Infiniband,
					Message:        "state: 1: DOWN, phys_state: 2: Polling",
					IsHealthyEvent: false,
					LinkLayer:      "Ethernet",
				},
			},
		},
		{
			name:                 "MonitorTypeInfiniBand_LinkLayerIB_NewUnhealthyPort",
			monitorNetworkType:   MonitorNetworkTypeInfiniBand,
			linkLayer:            "InfiniBand",
			initialPortState:     "1: DOWN",
			initialPortPhysState: "2: Polling",
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
				{
					Name:           "mlx5_test_1",
					NicType:        Infiniband,
					Message:        "state: 1: DOWN, phys_state: 2: Polling",
					IsHealthyEvent: false,
					LinkLayer:      "InfiniBand",
				},
			},
		},
		{
			name:                 "MonitorTypeRoCE_LinkLayerMissing_ShouldSkipPort",
			monitorNetworkType:   MonitorNetworkTypeRoCE,
			linkLayer:            "MISSING", // Special value to indicate file should not be created
			initialPortState:     "4: ACTIVE",
			initialPortPhysState: "5: LinkUp",
			expectedEvents: []NicHealthEvent{ // Only device detection, port is skipped due to missing link_layer
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsMap := fstest.MapFS{
				"sys/class/infiniband/mlx5_test/ports/1":            {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_test/ports/1/state":      {Data: []byte(tc.initialPortState)},
				"sys/class/infiniband/mlx5_test/ports/1/phys_state": {Data: []byte(tc.initialPortPhysState)},
			}
			if tc.linkLayer != "MISSING" {
				fsMap["sys/class/infiniband/mlx5_test/ports/1/link_layer"] = &fstest.MapFile{Data: []byte(tc.linkLayer)}
			}

			fileSystem = &MockFileSystem{Fs: fsMap}

			ibMonitor := &InfinibandDeviceMonitor{}
			ibMonitor.Devices = make(map[string]InfiniBandDevice)

			nicConfig := &NicMonitorConfig{
				ExclusionRegexes:       nil,
				MonitorNetworkType:     tc.monitorNetworkType,
				SysClassInfinibandPath: sysClassInfinibandPath,
			}

			actualEvents, err := ibMonitor.Monitor(nicConfig)
			require.NoError(t, err)

			// Debug output
			t.Logf("DEBUG: Test case: %s", tc.name)
			t.Logf("DEBUG: Total events returned: %d", len(actualEvents))
			for i, event := range actualEvents {
				isPortEvent := strings.Contains(event.Name, "_")
				t.Logf("DEBUG:   Event %d: Name=%s, Message=%s, LinkLayer=%s, IsPortEvent=%v",
					i, event.Name, event.Message, event.LinkLayer, isPortEvent)
			}

			require.ElementsMatch(t, tc.expectedEvents, actualEvents)
		})
	}
}

func TestHasMatchingRoCEInterface(t *testing.T) {
	tests := []struct {
		name                 string
		deviceName           string
		roCEInterfaceRegexes []string
		fsMap                fstest.MapFS
		expectedResult       bool
		expectError          bool
		errorContains        string
	}{
		{
			name:                 "EmptyRegexList_ShouldAllowAll",
			deviceName:           "mlx5_0",
			roCEInterfaceRegexes: []string{},
			fsMap: fstest.MapFS{
				"sys/class/infiniband/mlx5_0/device/net/eth0": {Mode: fs.ModeDir},
			},
			expectedResult: true,
			expectError:    false,
		},
		{
			name:                 "SingleMatchingInterface",
			deviceName:           "mlx5_0",
			roCEInterfaceRegexes: []string{"^rdma\\d+$"},
			fsMap: fstest.MapFS{
				"sys/class/infiniband/mlx5_0/device/net/rdma0": {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/device/net/eth0":  {Mode: fs.ModeDir},
			},
			expectedResult: true,
			expectError:    false,
		},
		{
			name:                 "NoMatchingInterface",
			deviceName:           "mlx5_0",
			roCEInterfaceRegexes: []string{"^rdma\\d+$"},
			fsMap: fstest.MapFS{
				"sys/class/infiniband/mlx5_0/device/net/eth0": {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/device/net/ib0":  {Mode: fs.ModeDir},
			},
			expectedResult: false,
			expectError:    false,
		},
		{
			name:                 "MultipleRegexes_OneMatches",
			deviceName:           "mlx5_0",
			roCEInterfaceRegexes: []string{"^rdma\\d+$", "^mlx5_\\d+$"},
			fsMap: fstest.MapFS{
				"sys/class/infiniband/mlx5_0/device/net/mlx5_0": {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/device/net/eth0":   {Mode: fs.ModeDir},
			},
			expectedResult: true,
			expectError:    false,
		},
		{
			name:                 "MultipleRegexes_MultipleMatches",
			deviceName:           "mlx5_0",
			roCEInterfaceRegexes: []string{"^rdma\\d+$", "^mlx5_.*$"},
			fsMap: fstest.MapFS{
				"sys/class/infiniband/mlx5_0/device/net/rdma0":  {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/device/net/rdma1":  {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/device/net/mlx5_0": {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/device/net/mlx5_1": {Mode: fs.ModeDir},
			},
			expectedResult: true,
			expectError:    false,
		},
		{
			name:                 "NoNetDirectory_ShouldReturnFalse",
			deviceName:           "mlx5_0",
			roCEInterfaceRegexes: []string{"^rdma\\d+$"},
			fsMap: fstest.MapFS{
				"sys/class/infiniband/mlx5_0/device": {Mode: fs.ModeDir},
				// No net directory
			},
			expectedResult: false,
			expectError:    false,
		},
		{
			name:                 "InvalidRegex",
			deviceName:           "mlx5_0",
			roCEInterfaceRegexes: []string{"[invalid(regex"},
			fsMap: fstest.MapFS{
				"sys/class/infiniband/mlx5_0/device/net/rdma0": {Mode: fs.ModeDir},
			},
			expectedResult: false,
			expectError:    true,
			errorContains:  "invalid RoCE interface regex",
		},
		{
			name:                 "EmptyNetDirectory",
			deviceName:           "mlx5_0",
			roCEInterfaceRegexes: []string{"^rdma\\d+$"},
			fsMap: fstest.MapFS{
				"sys/class/infiniband/mlx5_0/device/net": {Mode: fs.ModeDir},
			},
			expectedResult: false,
			expectError:    false,
		},
		{
			name:                 "ComplexRegexPattern",
			deviceName:           "mlx5_0",
			roCEInterfaceRegexes: []string{"^(rdma|roce|ib)\\d+$"},
			fsMap: fstest.MapFS{
				"sys/class/infiniband/mlx5_0/device/net/roce0": {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/device/net/eth0":  {Mode: fs.ModeDir},
			},
			expectedResult: true,
			expectError:    false,
		},
		{
			name:                 "CaseSensitiveMatch",
			deviceName:           "mlx5_0",
			roCEInterfaceRegexes: []string{"^RDMA\\d+$"}, // Uppercase
			fsMap: fstest.MapFS{
				"sys/class/infiniband/mlx5_0/device/net/rdma0": {Mode: fs.ModeDir}, // Lowercase
			},
			expectedResult: false, // Should not match due to case sensitivity
			expectError:    false,
		},
		{
			name:                 "DefaultPatterns_MatchRdma",
			deviceName:           "mlx5_0",
			roCEInterfaceRegexes: []string{"^rdma\\d+$", "^eth\\d+$"}, // Default patterns
			fsMap: fstest.MapFS{
				"sys/class/infiniband/mlx5_0/device/net/rdma0": {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/device/net/ib0":   {Mode: fs.ModeDir},
			},
			expectedResult: true,
			expectError:    false,
		},
		{
			name:                 "DefaultPatterns_MatchEth",
			deviceName:           "mlx5_0",
			roCEInterfaceRegexes: []string{"^rdma\\d+$", "^eth\\d+$"}, // Default patterns
			fsMap: fstest.MapFS{
				"sys/class/infiniband/mlx5_0/device/net/eth0": {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/device/net/ib0":  {Mode: fs.ModeDir},
			},
			expectedResult: true,
			expectError:    false,
		},
		{
			name:                 "DefaultPatterns_NoMatch",
			deviceName:           "mlx5_0",
			roCEInterfaceRegexes: []string{"^rdma\\d+$", "^eth\\d+$"}, // Default patterns
			fsMap: fstest.MapFS{
				"sys/class/infiniband/mlx5_0/device/net/ib0":    {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/device/net/mlx5_0": {Mode: fs.ModeDir},
			},
			expectedResult: false,
			expectError:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fileSystem = &MockFileSystem{Fs: tc.fsMap}

			result, err := hasMatchingRoCEInterface(tc.deviceName, tc.roCEInterfaceRegexes, &NicMonitorConfig{
				SysClassInfinibandPath: "/sys/class/infiniband",
			})

			if tc.expectError {
				require.Error(t, err)
				if tc.errorContains != "" {
					require.Contains(t, err.Error(), tc.errorContains)
				}
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.expectedResult, result)
			}
		})
	}
}

func TestInfinibandMonitorWithRoCEInterfaceFiltering(t *testing.T) {
	tests := []struct {
		name                 string
		roCEInterfaceRegexes []string
		fsMap                fstest.MapFS
		expectedEvents       []NicHealthEvent
		description          string
	}{
		{
			name:                 "RoCEMode_DeviceWithMatchingInterface",
			roCEInterfaceRegexes: []string{"^rdma\\d+$"},
			fsMap: fstest.MapFS{
				// mlx5_0 with rdma0 interface
				"sys/class/infiniband/mlx5_0/device/net/rdma0":   {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/ports/1":            {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/ports/1/state":      {Data: []byte("4: ACTIVE")},
				"sys/class/infiniband/mlx5_0/ports/1/phys_state": {Data: []byte("5: LinkUp")},
				"sys/class/infiniband/mlx5_0/ports/1/link_layer": {Data: []byte("Ethernet")},
			},
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_0", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
				{Name: "mlx5_0_1", NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: "Ethernet"},
			},
			description: "Device with matching RoCE interface should be monitored",
		},
		{
			name:                 "RoCEMode_DeviceWithoutMatchingInterface",
			roCEInterfaceRegexes: []string{"^rdma\\d+$"},
			fsMap: fstest.MapFS{
				// mlx5_0 with only eth0 interface (no rdma)
				"sys/class/infiniband/mlx5_0/device/net/eth0":    {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/ports/1":            {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/ports/1/state":      {Data: []byte("4: ACTIVE")},
				"sys/class/infiniband/mlx5_0/ports/1/phys_state": {Data: []byte("5: LinkUp")},
				"sys/class/infiniband/mlx5_0/ports/1/link_layer": {Data: []byte("Ethernet")},
			},
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_0", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
				// No port events because device is filtered out after detection
			},
			description: "Device without matching RoCE interface should be filtered out",
		},
		{
			name:                 "RoCEMode_MultipleDevices_MixedInterfaces",
			roCEInterfaceRegexes: []string{"^rdma\\d+$", "^roce\\d+$"},
			fsMap: fstest.MapFS{
				// mlx5_0 with rdma0 - should be monitored
				"sys/class/infiniband/mlx5_0/device/net/rdma0":   {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/ports/1":            {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/ports/1/state":      {Data: []byte("4: ACTIVE")},
				"sys/class/infiniband/mlx5_0/ports/1/phys_state": {Data: []byte("5: LinkUp")},
				"sys/class/infiniband/mlx5_0/ports/1/link_layer": {Data: []byte("Ethernet")},
				// mlx5_1 with roce0 - should be monitored
				"sys/class/infiniband/mlx5_1/device/net/roce0":   {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_1/ports/1":            {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_1/ports/1/state":      {Data: []byte("4: ACTIVE")},
				"sys/class/infiniband/mlx5_1/ports/1/phys_state": {Data: []byte("5: LinkUp")},
				"sys/class/infiniband/mlx5_1/ports/1/link_layer": {Data: []byte("Ethernet")},
				// mlx5_2 with only eth0 - should be filtered out
				"sys/class/infiniband/mlx5_2/device/net/eth0":    {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_2/ports/1":            {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_2/ports/1/state":      {Data: []byte("4: ACTIVE")},
				"sys/class/infiniband/mlx5_2/ports/1/phys_state": {Data: []byte("5: LinkUp")},
				"sys/class/infiniband/mlx5_2/ports/1/link_layer": {Data: []byte("Ethernet")},
			},
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_0", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
				{Name: "mlx5_0_1", NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: "Ethernet"},
				{Name: "mlx5_1", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
				{Name: "mlx5_1_1", NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: "Ethernet"},
				{Name: "mlx5_2", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
				// No mlx5_2 port events because it's filtered out
			},
			description: "Multiple devices with different interfaces, only matching ones monitored",
		},
		{
			name:                 "RoCEMode_DeviceNoNetDirectory",
			roCEInterfaceRegexes: []string{"^rdma\\d+$"},
			fsMap: fstest.MapFS{
				// mlx5_0 without net directory
				"sys/class/infiniband/mlx5_0/device":             {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/ports/1":            {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/ports/1/state":      {Data: []byte("4: ACTIVE")},
				"sys/class/infiniband/mlx5_0/ports/1/phys_state": {Data: []byte("5: LinkUp")},
				"sys/class/infiniband/mlx5_0/ports/1/link_layer": {Data: []byte("Ethernet")},
			},
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_0", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
				// No port events because device has no net directory
			},
			description: "Device without net directory should be filtered out",
		},
		{
			name:                 "RoCEMode_UnhealthyPortWithMatchingInterface",
			roCEInterfaceRegexes: []string{"^rdma\\d+$"},
			fsMap: fstest.MapFS{
				// mlx5_0 with rdma0 interface and unhealthy port
				"sys/class/infiniband/mlx5_0/device/net/rdma0":   {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/ports/1":            {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/ports/1/state":      {Data: []byte("1: DOWN")},
				"sys/class/infiniband/mlx5_0/ports/1/phys_state": {Data: []byte("2: Polling")},
				"sys/class/infiniband/mlx5_0/ports/1/link_layer": {Data: []byte("Ethernet")},
			},
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_0", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
				{
					Name:           "mlx5_0_1",
					NicType:        Infiniband,
					Message:        "state: 1: DOWN, phys_state: 2: Polling",
					IsHealthyEvent: false,
					LinkLayer:      "Ethernet",
				},
			},
			description: "Unhealthy port on device with matching interface should be reported",
		},
		{
			name:                 "RoCEMode_EmptyRegexList_ShouldMonitorAll",
			roCEInterfaceRegexes: []string{},
			fsMap: fstest.MapFS{
				// mlx5_0 with any interface
				"sys/class/infiniband/mlx5_0/device/net/eth0":    {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/ports/1":            {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_0/ports/1/state":      {Data: []byte("4: ACTIVE")},
				"sys/class/infiniband/mlx5_0/ports/1/phys_state": {Data: []byte("5: LinkUp")},
				"sys/class/infiniband/mlx5_0/ports/1/link_layer": {Data: []byte("Ethernet")},
			},
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_0", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
				{Name: "mlx5_0_1", NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: "Ethernet"},
			},
			description: "Empty regex list should allow all devices",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fileSystem = &MockFileSystem{Fs: tc.fsMap}

			ibMonitor := &InfinibandDeviceMonitor{}
			ibMonitor.Devices = make(map[string]InfiniBandDevice)

			nicConfig := &NicMonitorConfig{
				ExclusionRegexes:       nil,
				MonitorNetworkType:     MonitorNetworkTypeRoCE,
				RoCEInterfaceRegexes:   tc.roCEInterfaceRegexes,
				SysClassInfinibandPath: "/sys/class/infiniband",
			}

			actualEvents, err := ibMonitor.Monitor(nicConfig)
			require.NoError(t, err, tc.description)
			require.ElementsMatch(t, tc.expectedEvents, actualEvents, tc.description)
		})
	}
}

func TestInfinibandMonitorRoCEFilteringStateTransitions(t *testing.T) {
	// Initial state: mlx5_0 with rdma0 interface
	initialFS := fstest.MapFS{
		"sys/class/infiniband/mlx5_0/device/net/rdma0":   {Mode: fs.ModeDir},
		"sys/class/infiniband/mlx5_0/ports/1":            {Mode: fs.ModeDir},
		"sys/class/infiniband/mlx5_0/ports/1/state":      {Data: []byte("4: ACTIVE")},
		"sys/class/infiniband/mlx5_0/ports/1/phys_state": {Data: []byte("5: LinkUp")},
		"sys/class/infiniband/mlx5_0/ports/1/link_layer": {Data: []byte("Ethernet")},
	}

	fileSystem = &MockFileSystem{Fs: initialFS}
	ibMonitor := &InfinibandDeviceMonitor{}
	ibMonitor.Devices = make(map[string]InfiniBandDevice)

	nicConfig := &NicMonitorConfig{
		ExclusionRegexes:       nil,
		MonitorNetworkType:     MonitorNetworkTypeRoCE,
		RoCEInterfaceRegexes:   []string{"^rdma\\d+$"},
		SysClassInfinibandPath: "/sys/class/infiniband",
	}

	// First monitor call - device should be detected
	actualEvents, err := ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	expectedEvents := []NicHealthEvent{
		{Name: "mlx5_0", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
		{Name: "mlx5_0_1", NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: "Ethernet"},
	}
	require.ElementsMatch(t, expectedEvents, actualEvents)

	// Second monitor call - no changes, no events
	actualEvents, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.Empty(t, actualEvents)

	// Change interface from rdma0 to eth0 (device should disappear)
	mockFS := fileSystem.(*MockFileSystem)
	delete(mockFS.Fs, "sys/class/infiniband/mlx5_0/device/net/rdma0")
	mockFS.Fs["sys/class/infiniband/mlx5_0/device/net/eth0"] = &fstest.MapFile{Mode: fs.ModeDir}

	// Third monitor call - device is still detected but ports are skipped
	actualEvents, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	// The device is still in the list but doesn't match the filter, so no port events
	require.Empty(t, actualEvents)

	// Add rdma0 back (device should start being monitored again)
	mockFS.Fs["sys/class/infiniband/mlx5_0/device/net/rdma0"] = &fstest.MapFile{Mode: fs.ModeDir}

	// Fourth monitor call - ports should be monitored again
	actualEvents, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	// Since the device was already known and the port state hasn't changed, no events should be generated
	require.Empty(t, actualEvents)
}

func TestInfinibandMonitorWithLinkLayerBasedRoCEFiltering(t *testing.T) {
	const sysClassInfinibandPath = "/sys/class/infiniband"

	testCases := []struct {
		name                 string
		linkLayer            string
		hasMatchingInterface bool // Whether device has matching RoCE interface
		expectedPortEvents   int  // Number of port events expected
		description          string
	}{
		{
			name:                 "InfiniBand_LinkLayer_FilteredOutByRoCEMode",
			linkLayer:            "InfiniBand",
			hasMatchingInterface: false, // Interface doesn't matter for InfiniBand link layer
			expectedPortEvents:   0,     // Port should be filtered out by RoCE mode
			description:          "InfiniBand link layer should be filtered out when MonitorNetworkType is RoCE",
		},
		{
			name:                 "Ethernet_LinkLayer_WithMatchingInterface",
			linkLayer:            "Ethernet",
			hasMatchingInterface: true, // Has matching interface, should be monitored
			expectedPortEvents:   1,    // Port event should be generated
			description:          "Ethernet link layer with matching RoCE interface should be monitored",
		},
		{
			name:                 "Ethernet_LinkLayer_WithoutMatchingInterface",
			linkLayer:            "Ethernet",
			hasMatchingInterface: false, // No matching interface, should be filtered out
			expectedPortEvents:   0,     // No port events should be generated
			description:          "Ethernet link layer without matching RoCE interface should be filtered out",
		},
		{
			name:                 "Unknown_LinkLayer_WithMatchingInterface",
			linkLayer:            UNKNOWN_LINK_LAYER,
			hasMatchingInterface: true, // Has matching interface, but should be handled by EthernetErrorCheck
			expectedPortEvents:   0,    // No port events - should be handled by EthernetErrorCheck
			description:          "Unknown link layer should be handled by EthernetErrorCheck, not InfiniBand monitor",
		},
		{
			name:                 "Unknown_LinkLayer_WithoutMatchingInterface",
			linkLayer:            UNKNOWN_LINK_LAYER,
			hasMatchingInterface: false, // No matching interface, should be filtered out
			expectedPortEvents:   0,     // No port events should be generated
			description:          "Unknown link layer without matching RoCE interface should be filtered out",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fsMap := fstest.MapFS{
				"sys/class/infiniband/mlx5_test/ports/1":            {Mode: fs.ModeDir},
				"sys/class/infiniband/mlx5_test/ports/1/state":      {Data: []byte("4: ACTIVE")},
				"sys/class/infiniband/mlx5_test/ports/1/phys_state": {Data: []byte("5: LinkUp")},
			}

			// Add link_layer file if not "unknown"
			if tc.linkLayer != UNKNOWN_LINK_LAYER {
				fsMap["sys/class/infiniband/mlx5_test/ports/1/link_layer"] = &fstest.MapFile{Data: []byte(tc.linkLayer)}
			}

			// Add network interface directory and matching interface if needed
			if tc.hasMatchingInterface {
				fsMap["sys/class/infiniband/mlx5_test/device/net"] = &fstest.MapFile{Mode: fs.ModeDir}
				fsMap["sys/class/infiniband/mlx5_test/device/net/eth0"] = &fstest.MapFile{Mode: fs.ModeDir}
			} else {
				// Add net directory but with non-matching interface
				fsMap["sys/class/infiniband/mlx5_test/device/net"] = &fstest.MapFile{Mode: fs.ModeDir}
				fsMap["sys/class/infiniband/mlx5_test/device/net/ens123"] = &fstest.MapFile{Mode: fs.ModeDir}
			}

			fileSystem = &MockFileSystem{Fs: fsMap}

			ibMonitor := &InfinibandDeviceMonitor{}
			ibMonitor.Devices = make(map[string]InfiniBandDevice)

			nicConfig := &NicMonitorConfig{
				ExclusionRegexes:       nil,
				MonitorNetworkType:     MonitorNetworkTypeRoCE,
				RoCEInterfaceRegexes:   []string{"^eth\\d+$", "^rdma\\d+$"}, // Only eth* and rdma* patterns
				SysClassInfinibandPath: sysClassInfinibandPath,
			}

			actualEvents, err := ibMonitor.Monitor(nicConfig)
			require.NoError(t, err)

			// Should always have device detection event
			require.GreaterOrEqual(t, len(actualEvents), 1, tc.description)
			require.Equal(t, "mlx5_test", actualEvents[0].Name)
			require.Equal(t, nicIsDetected, actualEvents[0].Message)

			// Check port events based on expectation
			portEvents := 0
			for _, event := range actualEvents {
				// Port events have device_port format where port is a number
				// Device events don't have this pattern
				if strings.Contains(event.Name, "mlx5_test_") { // Port events have mlx5_test_<port> format
					portEvents++
				}
			}

			require.Equal(t, tc.expectedPortEvents, portEvents, tc.description)
		})
	}
}
