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

const (
	MLX5_0_PORT1_PATH         = "sys/class/infiniband/mlx5_0/ports/1"
	MLX5_1_PORT1_PATH         = "sys/class/infiniband/mlx5_1/ports/1"
	MLX5_2_PORT1_PATH         = "sys/class/infiniband/mlx5_2/ports/1"
	MLX5_1_PORT2_PATH         = "sys/class/infiniband/mlx5_1/ports/2"
	MLX5_NEW_PORT1_PATH       = "sys/class/infiniband/mlx5_new/ports/1"
	MLX5_TEST_PORT1_PATH      = "sys/class/infiniband/mlx5_test/ports/1"
	MLX5_0_DEVICE_NET         = "sys/class/infiniband/mlx5_0/device/net"
	SYS_CLASS_INFINIBAND_PATH = "sys/class/infiniband"

	PHYS_STATE = "/phys_state"
	STATE      = "/state"
	LINK_LAYER = "/link_layer"

	PORT_PHYS_STATE_LINK_UP = "5: LinkUp"
	PORT_STATE_ACTIVE       = "4: ACTIVE"
	PORT_STATE_DOWN         = "1: DOWN"
	PORT_PHYS_STATE_POLLING = "2: Polling"

	LINK_LAYER_INFINIBAND = "InfiniBand"
	LINK_LAYER_ETHERNET   = "Ethernet"

	HEALTH_STATE_DOWN_POLLING = "state: 1: DOWN, phys_state: 2: Polling"

	ETH0        = "/eth0"
	RDMA0       = "/rdma0"
	MLX5_0      = "mlx5_0"
	MLX5_0_1    = "mlx5_0_1"
	MLX5_0_PATH = "/mlx5_0"

	RDMA_INTERFACE_REGEX = "^rdma\\d+$"
	ETH_INTERFACE_REGEX  = "^eth\\d+$"
)

func TestScrapInfinibandDevices(t *testing.T) {
	sysClassInfinibandPath := SYS_CLASS_INFINIBAND_PATH
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			// mlx5_0 with port 1
			MLX5_0_PORT1_PATH:              {Mode: fs.ModeDir},
			MLX5_0_PORT1_PATH + PHYS_STATE: {Data: []byte(PORT_PHYS_STATE_LINK_UP)},
			MLX5_0_PORT1_PATH + STATE:      {Data: []byte(PORT_STATE_ACTIVE)},
			// mlx5_1 with port 1 and 2
			MLX5_1_PORT1_PATH:              {Mode: fs.ModeDir},
			MLX5_1_PORT1_PATH + PHYS_STATE: {Data: []byte(PORT_PHYS_STATE_LINK_UP)},
			MLX5_1_PORT1_PATH + STATE:      {Data: []byte(PORT_STATE_ACTIVE)},
			MLX5_1_PORT2_PATH:              {Mode: fs.ModeDir},
			MLX5_1_PORT2_PATH + PHYS_STATE: {Data: []byte(PORT_PHYS_STATE_LINK_UP)},
			MLX5_1_PORT2_PATH + STATE:      {Data: []byte(PORT_STATE_ACTIVE)},
		},
	}

	expected := map[string]InfiniBandDevice{
		"mlx5_1": {
			Name: "mlx5_1",
			Ports: map[string]InfiniBandPort{
				"2": {Name: "2", State: PORT_STATE_ACTIVE, PhysState: PORT_PHYS_STATE_LINK_UP},
				"1": {Name: "1", State: PORT_STATE_ACTIVE, PhysState: PORT_PHYS_STATE_LINK_UP},
			},
		},
		"mlx5_0": {
			Name:  MLX5_0,
			Ports: map[string]InfiniBandPort{"1": {Name: "1", State: PORT_STATE_ACTIVE, PhysState: PORT_PHYS_STATE_LINK_UP}},
		},
	}

	actualDevs, err := GetInfinibandDevices(nil, sysClassInfinibandPath)
	require.NoError(t, err)
	require.NotNil(t, actualDevs)
	require.Equal(t, expected, actualDevs)
}

func TestInfinibandMonitor(t *testing.T) {
	sysClassInfinibandPath := SYS_CLASS_INFINIBAND_PATH
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			// mlx5_0 with port 1
			MLX5_0_PORT1_PATH:              {Mode: fs.ModeDir},
			MLX5_0_PORT1_PATH + PHYS_STATE: {Data: []byte(PORT_PHYS_STATE_LINK_UP)},
			MLX5_0_PORT1_PATH + STATE:      {Data: []byte(PORT_STATE_ACTIVE)},
			MLX5_0_PORT1_PATH + LINK_LAYER: {Data: []byte(LINK_LAYER_INFINIBAND)}, // Default link layer
		},
	}

	mockFS := fileSystem.(*MockFileSystem)

	expectedHealthyDeviceAndPort := []NicHealthEvent{
		{NicType: Infiniband, Name: MLX5_0, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_INFINIBAND},
		{NicType: Infiniband, Name: MLX5_0_1, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: LINK_LAYER_INFINIBAND},
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
	mockFS.Fs[MLX5_0_PORT1_PATH+PHYS_STATE].Data = []byte(PORT_PHYS_STATE_POLLING)

	expectedPhyStatePolling := []NicHealthEvent{
		{
			NicType:        Infiniband,
			Name:           MLX5_0_1,
			Message:        "phys_state: 2: Polling",
			IsHealthyEvent: false,
			LinkLayer:      LINK_LAYER_INFINIBAND,
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
	mockFS.Fs[MLX5_0_PORT1_PATH+PHYS_STATE].Data = []byte(PORT_PHYS_STATE_LINK_UP)
	expectedPortBecomesHealthy := []NicHealthEvent{
		{NicType: Infiniband, Name: MLX5_0_1, Message: "Port is healthy", IsHealthyEvent: true, LinkLayer: LINK_LAYER_INFINIBAND},
	}
	actualEvents, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)
	require.ElementsMatch(t, expectedPortBecomesHealthy, actualEvents)

	// eth0 become down again, so report nic down event
	mockFS.Fs[MLX5_0_PORT1_PATH+PHYS_STATE].Data = []byte(PORT_PHYS_STATE_POLLING)
	actualEvents, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)
	require.ElementsMatch(t, expectedPhyStatePolling, actualEvents)

	// mlx5_0 port 1 physical link is not ready, so report nic down event
	mockFS.Fs[MLX5_0_PORT1_PATH+STATE].Data = []byte(PORT_STATE_DOWN)

	expectedStateDownAndPhyPolling := []NicHealthEvent{
		{
			NicType:        Infiniband,
			Name:           MLX5_0_1,
			Message:        HEALTH_STATE_DOWN_POLLING,
			IsHealthyEvent: false,
			LinkLayer:      LINK_LAYER_INFINIBAND,
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
		MLX5_NEW_PORT1_PATH:              {Mode: fs.ModeDir},
		MLX5_NEW_PORT1_PATH + PHYS_STATE: {Data: []byte("1: Disabled")},
		MLX5_NEW_PORT1_PATH + STATE:      {Data: []byte(PORT_STATE_DOWN)},
		MLX5_NEW_PORT1_PATH + LINK_LAYER: {Data: []byte(LINK_LAYER_INFINIBAND)},
	}
	expectedNewUnhealthyPortEvents := []NicHealthEvent{
		{NicType: Infiniband, Name: "mlx5_new", Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_INFINIBAND},
		{
			NicType:        Infiniband,
			Name:           "mlx5_new_1",
			Message:        "state: 1: DOWN, phys_state: 1: Disabled",
			IsHealthyEvent: false,
			LinkLayer:      LINK_LAYER_INFINIBAND,
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
	sysClassInfinibandPath := SYS_CLASS_INFINIBAND_PATH
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			// mlx5_0 with port 1
			MLX5_0_PORT1_PATH:              {Mode: fs.ModeDir},
			MLX5_0_PORT1_PATH + PHYS_STATE: {Data: []byte(PORT_PHYS_STATE_LINK_UP)},
			MLX5_0_PORT1_PATH + STATE:      {Data: []byte(PORT_STATE_ACTIVE)},
			MLX5_0_PORT1_PATH + LINK_LAYER: {Data: []byte(LINK_LAYER_INFINIBAND)},
			// mlx5_1 with port 1
			MLX5_1_PORT1_PATH:              {Mode: fs.ModeDir},
			MLX5_1_PORT1_PATH + PHYS_STATE: {Data: []byte(PORT_PHYS_STATE_LINK_UP)},
			MLX5_1_PORT1_PATH + STATE:      {Data: []byte(PORT_STATE_ACTIVE)},
			MLX5_1_PORT1_PATH + LINK_LAYER: {Data: []byte(LINK_LAYER_INFINIBAND)},
			// mlx5_2 with port 1
			MLX5_2_PORT1_PATH:              {Mode: fs.ModeDir},
			MLX5_2_PORT1_PATH + PHYS_STATE: {Data: []byte(PORT_PHYS_STATE_LINK_UP)},
			MLX5_2_PORT1_PATH + STATE:      {Data: []byte(PORT_STATE_ACTIVE)},
			MLX5_2_PORT1_PATH + LINK_LAYER: {Data: []byte(LINK_LAYER_INFINIBAND)},
		},
	}

	mockFS := fileSystem.(*MockFileSystem)

	expectedNoError := []NicHealthEvent{
		{NicType: Infiniband, Name: MLX5_0, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_INFINIBAND},
		{NicType: Infiniband, Name: MLX5_0_1, Message: "Port is healthy", IsHealthyEvent: true, LinkLayer: LINK_LAYER_INFINIBAND},
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
	mockFS.Fs[MLX5_1_PORT1_PATH+STATE].Data = []byte(PORT_STATE_DOWN)
	expectedNoError = []NicHealthEvent{}
	actualErrors, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualErrors)
	require.Equal(t, expectedNoError, actualErrors)

	// udate mlx5_2 phys_state and verify it is not detected as it is excluded
	mockFS.Fs[MLX5_2_PORT1_PATH+PHYS_STATE].Data = []byte(PORT_PHYS_STATE_POLLING)

	actualErrors, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualErrors)
	require.Equal(t, expectedNoError, actualErrors)

	// update mlx5_0 to have a state change, it should be detected
	mockFS.Fs[MLX5_0_PORT1_PATH+STATE].Data = []byte(PORT_STATE_DOWN)

	expectedStateDown := []NicHealthEvent{
		{
			NicType:   Infiniband,
			Name:      MLX5_0_1,
			Message:   "state: 1: DOWN",
			LinkLayer: LINK_LAYER_INFINIBAND,
		},
	}

	actualErrors, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualErrors)
	require.Equal(t, expectedStateDown, actualErrors)
}

func TestInfinibandMonitorNetworkTypeFiltering(t *testing.T) {
	const sysClassInfinibandPath = SYS_CLASS_INFINIBAND_PATH
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
			linkLayer:            LINK_LAYER_INFINIBAND,
			initialPortState:     PORT_STATE_ACTIVE,
			initialPortPhysState: PORT_PHYS_STATE_LINK_UP,
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_INFINIBAND},
				{Name: "mlx5_test_1", NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: LINK_LAYER_INFINIBAND},
			},
		},
		{
			name:                 "MonitorTypeAll_LinkLayerEth_Healthy",
			monitorNetworkType:   MonitorNetworkTypeAll,
			linkLayer:            LINK_LAYER_ETHERNET,
			initialPortState:     PORT_STATE_ACTIVE,
			initialPortPhysState: PORT_PHYS_STATE_LINK_UP,
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
				{Name: "mlx5_test_1", NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
			},
		},
		{
			name:                 "MonitorTypeRoCE_LinkLayerEth_Healthy",
			monitorNetworkType:   MonitorNetworkTypeRoCE,
			linkLayer:            LINK_LAYER_ETHERNET,
			initialPortState:     PORT_STATE_ACTIVE,
			initialPortPhysState: PORT_PHYS_STATE_LINK_UP,
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
				{Name: "mlx5_test_1", NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
			},
		},
		{
			name:                 "MonitorTypeRoCE_LinkLayerIB_Healthy_ShouldBeSkipped",
			monitorNetworkType:   MonitorNetworkTypeRoCE,
			linkLayer:            LINK_LAYER_INFINIBAND,
			initialPortState:     PORT_STATE_ACTIVE,
			initialPortPhysState: PORT_PHYS_STATE_LINK_UP,
			expectedEvents: []NicHealthEvent{ // Only device detection, port is skipped
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_INFINIBAND},
			},
		},
		{
			name:                 "MonitorTypeInfiniBand_LinkLayerIB_Healthy",
			monitorNetworkType:   MonitorNetworkTypeInfiniBand,
			linkLayer:            LINK_LAYER_INFINIBAND,
			initialPortState:     PORT_STATE_ACTIVE,
			initialPortPhysState: PORT_PHYS_STATE_LINK_UP,
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_INFINIBAND},
				{Name: "mlx5_test_1", NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: LINK_LAYER_INFINIBAND},
			},
		},
		{
			name:                 "MonitorTypeInfiniBand_LinkLayerEth_Healthy_ShouldBeSkipped",
			monitorNetworkType:   MonitorNetworkTypeInfiniBand,
			linkLayer:            LINK_LAYER_ETHERNET,
			initialPortState:     PORT_STATE_ACTIVE,
			initialPortPhysState: PORT_PHYS_STATE_LINK_UP,
			expectedEvents: []NicHealthEvent{ // Only device detection, port is skipped
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
			},
		},
		{
			name:                 "MonitorTypeRoCE_LinkLayerEth_NewUnhealthyPort",
			monitorNetworkType:   MonitorNetworkTypeRoCE,
			linkLayer:            LINK_LAYER_ETHERNET,
			initialPortState:     PORT_STATE_DOWN,
			initialPortPhysState: PORT_PHYS_STATE_POLLING,
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
				{
					Name:           "mlx5_test_1",
					NicType:        Infiniband,
					Message:        HEALTH_STATE_DOWN_POLLING,
					IsHealthyEvent: false,
					LinkLayer:      LINK_LAYER_ETHERNET,
				},
			},
		},
		{
			name:                 "MonitorTypeInfiniBand_LinkLayerIB_NewUnhealthyPort",
			monitorNetworkType:   MonitorNetworkTypeInfiniBand,
			linkLayer:            LINK_LAYER_INFINIBAND,
			initialPortState:     PORT_STATE_DOWN,
			initialPortPhysState: PORT_PHYS_STATE_POLLING,
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_INFINIBAND},
				{
					Name:           "mlx5_test_1",
					NicType:        Infiniband,
					Message:        HEALTH_STATE_DOWN_POLLING,
					IsHealthyEvent: false,
					LinkLayer:      LINK_LAYER_INFINIBAND,
				},
			},
		},
		{
			name:                 "MonitorTypeRoCE_LinkLayerMissing_ShouldSkipPort",
			monitorNetworkType:   MonitorNetworkTypeRoCE,
			linkLayer:            "MISSING", // Special value to indicate file should not be created
			initialPortState:     PORT_STATE_ACTIVE,
			initialPortPhysState: PORT_PHYS_STATE_LINK_UP,
			expectedEvents: []NicHealthEvent{ // Only device detection, port is skipped due to missing link_layer
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: UNKNOWN_LINK_LAYER},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsMap := fstest.MapFS{
				MLX5_TEST_PORT1_PATH:              {Mode: fs.ModeDir},
				MLX5_TEST_PORT1_PATH + STATE:      {Data: []byte(tc.initialPortState)},
				MLX5_TEST_PORT1_PATH + PHYS_STATE: {Data: []byte(tc.initialPortPhysState)},
			}
			if tc.linkLayer != "MISSING" {
				fsMap[MLX5_TEST_PORT1_PATH+LINK_LAYER] = &fstest.MapFile{Data: []byte(tc.linkLayer)}
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
			deviceName:           MLX5_0,
			roCEInterfaceRegexes: []string{},
			fsMap: fstest.MapFS{
				MLX5_0_DEVICE_NET + ETH0: {Mode: fs.ModeDir},
			},
			expectedResult: true,
			expectError:    false,
		},
		{
			name:                 "SingleMatchingInterface",
			deviceName:           MLX5_0,
			roCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX},
			fsMap: fstest.MapFS{
				MLX5_0_DEVICE_NET + RDMA0: {Mode: fs.ModeDir},
				MLX5_0_DEVICE_NET + ETH0:  {Mode: fs.ModeDir},
			},
			expectedResult: true,
			expectError:    false,
		},
		{
			name:                 "NoMatchingInterface",
			deviceName:           MLX5_0,
			roCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX},
			fsMap: fstest.MapFS{
				MLX5_0_DEVICE_NET + ETH0:   {Mode: fs.ModeDir},
				MLX5_0_DEVICE_NET + "/ib0": {Mode: fs.ModeDir},
			},
			expectedResult: false,
			expectError:    false,
		},
		{
			name:                 "MultipleRegexes_OneMatches",
			deviceName:           MLX5_0,
			roCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX, "^mlx5_\\d+$"},
			fsMap: fstest.MapFS{
				MLX5_0_DEVICE_NET + MLX5_0_PATH: {Mode: fs.ModeDir},
				MLX5_0_DEVICE_NET + ETH0:        {Mode: fs.ModeDir},
			},
			expectedResult: true,
			expectError:    false,
		},
		{
			name:                 "MultipleRegexes_MultipleMatches",
			deviceName:           MLX5_0,
			roCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX, "^mlx5_.*$"},
			fsMap: fstest.MapFS{
				MLX5_0_DEVICE_NET + RDMA0:       {Mode: fs.ModeDir},
				MLX5_0_DEVICE_NET + "/rdma1":    {Mode: fs.ModeDir},
				MLX5_0_DEVICE_NET + MLX5_0_PATH: {Mode: fs.ModeDir},
				MLX5_0_DEVICE_NET + "/mlx5_1":   {Mode: fs.ModeDir},
			},
			expectedResult: true,
			expectError:    false,
		},
		{
			name:                 "NoNetDirectory_ShouldReturnFalse",
			deviceName:           MLX5_0,
			roCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX},
			fsMap: fstest.MapFS{
				"sys/class/infiniband/mlx5_0/device": {Mode: fs.ModeDir},
				// No net directory
			},
			expectedResult: false,
			expectError:    false,
		},
		{
			name:                 "InvalidRegex",
			deviceName:           MLX5_0,
			roCEInterfaceRegexes: []string{"[invalid(regex"},
			fsMap: fstest.MapFS{
				MLX5_0_DEVICE_NET + RDMA0: {Mode: fs.ModeDir},
			},
			expectedResult: false,
			expectError:    true,
			errorContains:  "invalid RoCE interface regex",
		},
		{
			name:                 "EmptyNetDirectory",
			deviceName:           MLX5_0,
			roCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX},
			fsMap: fstest.MapFS{
				MLX5_0_DEVICE_NET + "": {Mode: fs.ModeDir},
			},
			expectedResult: false,
			expectError:    false,
		},
		{
			name:                 "ComplexRegexPattern",
			deviceName:           MLX5_0,
			roCEInterfaceRegexes: []string{"^(rdma|roce|ib)\\d+$"},
			fsMap: fstest.MapFS{
				MLX5_0_DEVICE_NET + "/roce0": {Mode: fs.ModeDir},
				MLX5_0_DEVICE_NET + ETH0:     {Mode: fs.ModeDir},
			},
			expectedResult: true,
			expectError:    false,
		},
		{
			name:                 "CaseSensitiveMatch",
			deviceName:           MLX5_0,
			roCEInterfaceRegexes: []string{"^RDMA\\d+$"}, // Uppercase
			fsMap: fstest.MapFS{
				MLX5_0_DEVICE_NET + RDMA0: {Mode: fs.ModeDir}, // Lowercase
			},
			expectedResult: false, // Should not match due to case sensitivity
			expectError:    false,
		},
		{
			name:                 "DefaultPatterns_MatchRdma",
			deviceName:           MLX5_0,
			roCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX, ETH_INTERFACE_REGEX}, // Default patterns
			fsMap: fstest.MapFS{
				MLX5_0_DEVICE_NET + RDMA0:  {Mode: fs.ModeDir},
				MLX5_0_DEVICE_NET + "/ib0": {Mode: fs.ModeDir},
			},
			expectedResult: true,
			expectError:    false,
		},
		{
			name:                 "DefaultPatterns_MatchEth",
			deviceName:           MLX5_0,
			roCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX, ETH_INTERFACE_REGEX}, // Default patterns
			fsMap: fstest.MapFS{
				MLX5_0_DEVICE_NET + ETH0:   {Mode: fs.ModeDir},
				MLX5_0_DEVICE_NET + "/ib0": {Mode: fs.ModeDir},
			},
			expectedResult: true,
			expectError:    false,
		},
		{
			name:                 "DefaultPatterns_NoMatch",
			deviceName:           MLX5_0,
			roCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX, ETH_INTERFACE_REGEX}, // Default patterns
			fsMap: fstest.MapFS{
				MLX5_0_DEVICE_NET + "/ib0":      {Mode: fs.ModeDir},
				MLX5_0_DEVICE_NET + MLX5_0_PATH: {Mode: fs.ModeDir},
			},
			expectedResult: false,
			expectError:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fileSystem = &MockFileSystem{Fs: tc.fsMap}

			result, err := hasMatchingRoCEInterface(tc.deviceName, tc.roCEInterfaceRegexes, &NicMonitorConfig{
				SysClassInfinibandPath: SYS_CLASS_INFINIBAND_PATH,
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
			roCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX},
			fsMap: fstest.MapFS{
				// mlx5_0 with rdma0 interface
				MLX5_0_DEVICE_NET + RDMA0:      {Mode: fs.ModeDir},
				MLX5_0_PORT1_PATH:              {Mode: fs.ModeDir},
				MLX5_0_PORT1_PATH + STATE:      {Data: []byte(PORT_STATE_ACTIVE)},
				MLX5_0_PORT1_PATH + PHYS_STATE: {Data: []byte(PORT_PHYS_STATE_LINK_UP)},
				MLX5_0_PORT1_PATH + LINK_LAYER: {Data: []byte(LINK_LAYER_ETHERNET)},
			},
			expectedEvents: []NicHealthEvent{
				{Name: MLX5_0, NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
				{Name: MLX5_0_1, NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
			},
			description: "Device with matching RoCE interface should be monitored",
		},
		{
			name:                 "RoCEMode_DeviceWithoutMatchingInterface",
			roCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX},
			fsMap: fstest.MapFS{
				// mlx5_0 with only eth0 interface (no rdma)
				MLX5_0_DEVICE_NET + ETH0:       {Mode: fs.ModeDir},
				MLX5_0_PORT1_PATH:              {Mode: fs.ModeDir},
				MLX5_0_PORT1_PATH + STATE:      {Data: []byte(PORT_STATE_ACTIVE)},
				MLX5_0_PORT1_PATH + PHYS_STATE: {Data: []byte(PORT_PHYS_STATE_LINK_UP)},
				MLX5_0_PORT1_PATH + LINK_LAYER: {Data: []byte(LINK_LAYER_ETHERNET)},
			},
			expectedEvents: []NicHealthEvent{
				{Name: MLX5_0, NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
				// No port events because device is filtered out after detection
			},
			description: "Device without matching RoCE interface should be filtered out",
		},
		{
			name:                 "RoCEMode_MultipleDevices_MixedInterfaces",
			roCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX, "^roce\\d+$"},
			fsMap: fstest.MapFS{
				// mlx5_0 with rdma0 - should be monitored
				MLX5_0_DEVICE_NET + RDMA0:      {Mode: fs.ModeDir},
				MLX5_0_PORT1_PATH:              {Mode: fs.ModeDir},
				MLX5_0_PORT1_PATH + STATE:      {Data: []byte(PORT_STATE_ACTIVE)},
				MLX5_0_PORT1_PATH + PHYS_STATE: {Data: []byte(PORT_PHYS_STATE_LINK_UP)},
				MLX5_0_PORT1_PATH + LINK_LAYER: {Data: []byte(LINK_LAYER_ETHERNET)},
				// mlx5_1 with roce0 - should be monitored
				"sys/class/infiniband/mlx5_1/device/net/roce0": {Mode: fs.ModeDir},
				MLX5_1_PORT1_PATH:              {Mode: fs.ModeDir},
				MLX5_1_PORT1_PATH + STATE:      {Data: []byte(PORT_STATE_ACTIVE)},
				MLX5_1_PORT1_PATH + PHYS_STATE: {Data: []byte(PORT_PHYS_STATE_LINK_UP)},
				MLX5_1_PORT1_PATH + LINK_LAYER: {Data: []byte(LINK_LAYER_ETHERNET)},
				// mlx5_2 with only eth0 - should be filtered out
				"sys/class/infiniband/mlx5_2/device/net/eth0": {Mode: fs.ModeDir},
				MLX5_2_PORT1_PATH:              {Mode: fs.ModeDir},
				MLX5_2_PORT1_PATH + STATE:      {Data: []byte(PORT_STATE_ACTIVE)},
				MLX5_2_PORT1_PATH + PHYS_STATE: {Data: []byte(PORT_PHYS_STATE_LINK_UP)},
				MLX5_2_PORT1_PATH + LINK_LAYER: {Data: []byte(LINK_LAYER_ETHERNET)},
			},
			expectedEvents: []NicHealthEvent{
				{Name: MLX5_0, NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
				{Name: MLX5_0_1, NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
				{Name: "mlx5_1", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
				{Name: "mlx5_1_1", NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
				{Name: "mlx5_2", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
				// No mlx5_2 port events because it's filtered out
			},
			description: "Multiple devices with different interfaces, only matching ones monitored",
		},
		{
			name:                 "RoCEMode_DeviceNoNetDirectory",
			roCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX},
			fsMap: fstest.MapFS{
				// mlx5_0 without net directory
				"sys/class/infiniband/mlx5_0/device": {Mode: fs.ModeDir},
				MLX5_0_PORT1_PATH:                    {Mode: fs.ModeDir},
				MLX5_0_PORT1_PATH + STATE:            {Data: []byte(PORT_STATE_ACTIVE)},
				MLX5_0_PORT1_PATH + PHYS_STATE:       {Data: []byte(PORT_PHYS_STATE_LINK_UP)},
				MLX5_0_PORT1_PATH + LINK_LAYER:       {Data: []byte(LINK_LAYER_ETHERNET)},
			},
			expectedEvents: []NicHealthEvent{
				{Name: MLX5_0, NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
				// No port events because device has no net directory
			},
			description: "Device without net directory should be filtered out",
		},
		{
			name:                 "RoCEMode_UnhealthyPortWithMatchingInterface",
			roCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX},
			fsMap: fstest.MapFS{
				// mlx5_0 with rdma0 interface and unhealthy port
				MLX5_0_DEVICE_NET + RDMA0:      {Mode: fs.ModeDir},
				MLX5_0_PORT1_PATH:              {Mode: fs.ModeDir},
				MLX5_0_PORT1_PATH + STATE:      {Data: []byte(PORT_STATE_DOWN)},
				MLX5_0_PORT1_PATH + PHYS_STATE: {Data: []byte(PORT_PHYS_STATE_POLLING)},
				MLX5_0_PORT1_PATH + LINK_LAYER: {Data: []byte(LINK_LAYER_ETHERNET)},
			},
			expectedEvents: []NicHealthEvent{
				{Name: MLX5_0, NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
				{
					Name:           MLX5_0_1,
					NicType:        Infiniband,
					Message:        HEALTH_STATE_DOWN_POLLING,
					IsHealthyEvent: false,
					LinkLayer:      LINK_LAYER_ETHERNET,
				},
			},
			description: "Unhealthy port on device with matching interface should be reported",
		},
		{
			name:                 "RoCEMode_EmptyRegexList_ShouldMonitorAll",
			roCEInterfaceRegexes: []string{},
			fsMap: fstest.MapFS{
				// mlx5_0 with any interface
				MLX5_0_DEVICE_NET + ETH0:       {Mode: fs.ModeDir},
				MLX5_0_PORT1_PATH:              {Mode: fs.ModeDir},
				MLX5_0_PORT1_PATH + STATE:      {Data: []byte(PORT_STATE_ACTIVE)},
				MLX5_0_PORT1_PATH + PHYS_STATE: {Data: []byte(PORT_PHYS_STATE_LINK_UP)},
				MLX5_0_PORT1_PATH + LINK_LAYER: {Data: []byte(LINK_LAYER_ETHERNET)},
			},
			expectedEvents: []NicHealthEvent{
				{Name: MLX5_0, NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
				{Name: MLX5_0_1, NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
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
				SysClassInfinibandPath: SYS_CLASS_INFINIBAND_PATH,
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
		MLX5_0_DEVICE_NET + RDMA0:      {Mode: fs.ModeDir},
		MLX5_0_PORT1_PATH + "":         {Mode: fs.ModeDir},
		MLX5_0_PORT1_PATH + STATE:      {Data: []byte(PORT_STATE_ACTIVE)},
		MLX5_0_PORT1_PATH + PHYS_STATE: {Data: []byte(PORT_PHYS_STATE_LINK_UP)},
		MLX5_0_PORT1_PATH + LINK_LAYER: {Data: []byte(LINK_LAYER_ETHERNET)},
	}

	fileSystem = &MockFileSystem{Fs: initialFS}
	ibMonitor := &InfinibandDeviceMonitor{}
	ibMonitor.Devices = make(map[string]InfiniBandDevice)

	nicConfig := &NicMonitorConfig{
		ExclusionRegexes:       nil,
		MonitorNetworkType:     MonitorNetworkTypeRoCE,
		RoCEInterfaceRegexes:   []string{RDMA_INTERFACE_REGEX},
		SysClassInfinibandPath: SYS_CLASS_INFINIBAND_PATH,
	}

	// First monitor call - device should be detected
	actualEvents, err := ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	expectedEvents := []NicHealthEvent{
		{Name: MLX5_0, NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
		{Name: MLX5_0_1, NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true, LinkLayer: LINK_LAYER_ETHERNET},
	}
	require.ElementsMatch(t, expectedEvents, actualEvents)

	// Second monitor call - no changes, no events
	actualEvents, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.Empty(t, actualEvents)

	// Change interface from rdma0 to eth0 (device should disappear)
	mockFS := fileSystem.(*MockFileSystem)
	delete(mockFS.Fs, MLX5_0_DEVICE_NET+RDMA0)
	mockFS.Fs[MLX5_0_DEVICE_NET+ETH0] = &fstest.MapFile{Mode: fs.ModeDir}

	// Third monitor call - device is still detected but ports are skipped
	actualEvents, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	// The device is still in the list but doesn't match the filter, so no port events
	require.Empty(t, actualEvents)

	// Add rdma0 back (device should start being monitored again)
	mockFS.Fs[MLX5_0_DEVICE_NET+RDMA0] = &fstest.MapFile{Mode: fs.ModeDir}

	// Fourth monitor call - ports should be monitored again
	actualEvents, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	// Since the device was already known and the port state hasn't changed, no events should be generated
	require.Empty(t, actualEvents)
}

func TestInfinibandMonitorWithLinkLayerBasedRoCEFiltering(t *testing.T) {
	const sysClassInfinibandPath = SYS_CLASS_INFINIBAND_PATH

	testCases := []struct {
		name                 string
		linkLayer            string
		hasMatchingInterface bool // Whether device has matching RoCE interface
		expectedPortEvents   int  // Number of port events expected
		description          string
	}{
		{
			name:                 "InfiniBand_LinkLayer_FilteredOutByRoCEMode",
			linkLayer:            LINK_LAYER_INFINIBAND,
			hasMatchingInterface: false, // Interface doesn't matter for InfiniBand link layer
			expectedPortEvents:   0,     // Port should be filtered out by RoCE mode
			description:          "InfiniBand link layer should be filtered out when MonitorNetworkType is RoCE",
		},
		{
			name:                 "Ethernet_LinkLayer_WithMatchingInterface",
			linkLayer:            LINK_LAYER_ETHERNET,
			hasMatchingInterface: true, // Has matching interface, should be monitored
			expectedPortEvents:   1,    // Port event should be generated
			description:          "Ethernet link layer with matching RoCE interface should be monitored",
		},
		{
			name:                 "Ethernet_LinkLayer_WithoutMatchingInterface",
			linkLayer:            LINK_LAYER_ETHERNET,
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
				MLX5_TEST_PORT1_PATH:              {Mode: fs.ModeDir},
				MLX5_TEST_PORT1_PATH + STATE:      {Data: []byte(PORT_STATE_ACTIVE)},
				MLX5_TEST_PORT1_PATH + PHYS_STATE: {Data: []byte(PORT_PHYS_STATE_LINK_UP)},
			}

			// Add link_layer file if not "unknown"
			if tc.linkLayer != UNKNOWN_LINK_LAYER {
				fsMap[MLX5_TEST_PORT1_PATH+LINK_LAYER] = &fstest.MapFile{Data: []byte(tc.linkLayer)}
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
				RoCEInterfaceRegexes:   []string{ETH_INTERFACE_REGEX, RDMA_INTERFACE_REGEX}, // Only eth* and rdma* patterns
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

func TestGetDeviceLinkLayer(t *testing.T) {
	tests := []struct {
		name              string
		device            InfiniBandDevice
		fsMap             fstest.MapFS
		expectedLinkLayer string
		description       string
	}{
		{
			name: "SinglePort_InfiniBandLinkLayer",
			device: InfiniBandDevice{
				Name:  "mlx5_0",
				Ports: map[string]InfiniBandPort{"1": {Name: "1"}},
			},
			fsMap: fstest.MapFS{
				MLX5_0_PORT1_PATH + LINK_LAYER: {Data: []byte(LINK_LAYER_INFINIBAND)},
			},
			expectedLinkLayer: LINK_LAYER_INFINIBAND,
			description:       "Should return InfiniBand for device with InfiniBand port",
		},
		{
			name: "SinglePort_EthernetLinkLayer",
			device: InfiniBandDevice{
				Name:  "mlx5_0",
				Ports: map[string]InfiniBandPort{"1": {Name: "1"}},
			},
			fsMap: fstest.MapFS{
				MLX5_0_PORT1_PATH + LINK_LAYER: {Data: []byte(LINK_LAYER_ETHERNET)},
			},
			expectedLinkLayer: LINK_LAYER_ETHERNET,
			description:       "Should return Ethernet for device with Ethernet port",
		},
		{
			name: "SinglePort_UnknownLinkLayer_Lowercase",
			device: InfiniBandDevice{
				Name:  "mlx5_0",
				Ports: map[string]InfiniBandPort{"1": {Name: "1"}},
			},
			fsMap: fstest.MapFS{
				MLX5_0_PORT1_PATH + LINK_LAYER: {Data: []byte("unknown")},
			},
			expectedLinkLayer: UNKNOWN_LINK_LAYER,
			description:       "Should return unknown constant for lowercase 'unknown' in link_layer",
		},
		{
			name: "SinglePort_UnknownLinkLayer_Uppercase",
			device: InfiniBandDevice{
				Name:  "mlx5_0",
				Ports: map[string]InfiniBandPort{"1": {Name: "1"}},
			},
			fsMap: fstest.MapFS{
				MLX5_0_PORT1_PATH + LINK_LAYER: {Data: []byte("Unknown")},
			},
			expectedLinkLayer: UNKNOWN_LINK_LAYER,
			description:       "Should return unknown constant for capitalized 'Unknown' in link_layer",
		},
		{
			name: "SinglePort_UnknownLinkLayer_MixedCase",
			device: InfiniBandDevice{
				Name:  "mlx5_0",
				Ports: map[string]InfiniBandPort{"1": {Name: "1"}},
			},
			fsMap: fstest.MapFS{
				MLX5_0_PORT1_PATH + LINK_LAYER: {Data: []byte("UNKNOWN")},
			},
			expectedLinkLayer: UNKNOWN_LINK_LAYER,
			description:       "Should return unknown constant for uppercase 'UNKNOWN' in link_layer",
		},
		{
			name: "SinglePort_EmptyLinkLayer",
			device: InfiniBandDevice{
				Name:  "mlx5_0",
				Ports: map[string]InfiniBandPort{"1": {Name: "1"}},
			},
			fsMap: fstest.MapFS{
				MLX5_0_PORT1_PATH + LINK_LAYER: {Data: []byte("")},
			},
			expectedLinkLayer: UNKNOWN_LINK_LAYER,
			description:       "Should return unknown constant for empty link_layer content",
		},
		{
			name: "SinglePort_WhitespaceLinkLayer",
			device: InfiniBandDevice{
				Name:  "mlx5_0",
				Ports: map[string]InfiniBandPort{"1": {Name: "1"}},
			},
			fsMap: fstest.MapFS{
				MLX5_0_PORT1_PATH + LINK_LAYER: {Data: []byte("   \n\t  ")},
			},
			expectedLinkLayer: UNKNOWN_LINK_LAYER,
			description:       "Should return unknown constant for whitespace-only link_layer content",
		},
		{
			name: "MultiplePorts_SameLinkLayers",
			device: InfiniBandDevice{
				Name: "mlx5_1",
				Ports: map[string]InfiniBandPort{
					"1": {Name: "1"},
					"2": {Name: "2"},
				},
			},
			fsMap: fstest.MapFS{
				MLX5_1_PORT1_PATH + LINK_LAYER: {Data: []byte(LINK_LAYER_INFINIBAND)},
				MLX5_1_PORT2_PATH + LINK_LAYER: {Data: []byte(LINK_LAYER_INFINIBAND)},
			},
			expectedLinkLayer: LINK_LAYER_INFINIBAND, // Both ports have same link layer
			description:       "Should return link layer when multiple ports have same type",
		},
		{
			name: "NoReadableLinkLayer",
			device: InfiniBandDevice{
				Name:  "mlx5_0",
				Ports: map[string]InfiniBandPort{"1": {Name: "1"}},
			},
			fsMap: fstest.MapFS{
				// No link_layer file
			},
			expectedLinkLayer: UNKNOWN_LINK_LAYER,
			description:       "Should return unknown when link_layer cannot be read",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fileSystem = &MockFileSystem{Fs: tc.fsMap}

			config := &NicMonitorConfig{
				SysClassInfinibandPath: SYS_CLASS_INFINIBAND_PATH,
			}

			actualLinkLayer := getDeviceLinkLayer(config, tc.device)
			require.Equal(t, tc.expectedLinkLayer, actualLinkLayer, tc.description)
		})
	}
}

func TestDeviceLevelEventsWithCorrectLinkLayer(t *testing.T) {
	tests := []struct {
		name                    string
		linkLayer               string
		expectedDeviceLinkLayer string
		description             string
	}{
		{
			name:                    "InfiniBandDevice_ShouldHaveInfiniBandLinkLayer",
			linkLayer:               LINK_LAYER_INFINIBAND,
			expectedDeviceLinkLayer: LINK_LAYER_INFINIBAND,
			description:             "Device detection event should have InfiniBand link layer for IB device",
		},
		{
			name:                    "EthernetDevice_ShouldHaveEthernetLinkLayer",
			linkLayer:               LINK_LAYER_ETHERNET,
			expectedDeviceLinkLayer: LINK_LAYER_ETHERNET,
			description:             "Device detection event should have Ethernet link layer for Ethernet device",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fileSystem = &MockFileSystem{
				Fs: fstest.MapFS{
					MLX5_0_PORT1_PATH:              {Mode: fs.ModeDir},
					MLX5_0_PORT1_PATH + STATE:      {Data: []byte(PORT_STATE_ACTIVE)},
					MLX5_0_PORT1_PATH + PHYS_STATE: {Data: []byte(PORT_PHYS_STATE_LINK_UP)},
					MLX5_0_PORT1_PATH + LINK_LAYER: {Data: []byte(tc.linkLayer)},
				},
			}

			ibMonitor := &InfinibandDeviceMonitor{}
			ibMonitor.Devices = make(map[string]InfiniBandDevice)

			nicConfig := &NicMonitorConfig{
				ExclusionRegexes:       nil,
				MonitorNetworkType:     MonitorNetworkTypeAll,
				SysClassInfinibandPath: SYS_CLASS_INFINIBAND_PATH,
			}

			actualEvents, err := ibMonitor.Monitor(nicConfig)
			require.NoError(t, err, tc.description)
			require.Len(t, actualEvents, 2, "Should have device detection and port health events")

			// Find the device detection event
			var deviceEvent *NicHealthEvent
			for i := range actualEvents {
				if actualEvents[i].Message == nicIsDetected {
					deviceEvent = &actualEvents[i]
					break
				}
			}

			require.NotNil(t, deviceEvent, "Should have device detection event")
			require.Equal(t, tc.expectedDeviceLinkLayer, deviceEvent.LinkLayer, tc.description)
		})
	}
}
