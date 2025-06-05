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
		{NicType: Infiniband, Name: "mlx5_0", Message: nicIsDetected, IsHealthyEvent: true},
		{NicType: Infiniband, Name: "mlx5_0_1", Message: portIsHealthy, IsHealthyEvent: true},
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
		{NicType: Infiniband, Name: "mlx5_0_1", Message: "Port is healthy", IsHealthyEvent: true},
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
		{NicType: Infiniband, Name: "mlx5_new", Message: nicIsDetected, IsHealthyEvent: true},
		{
			NicType:        Infiniband,
			Name:           "mlx5_new_1",
			Message:        "state: 1: Down, phys_state: 1: Disabled",
			IsHealthyEvent: false,
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
			// mlx5_1 with port 1
			"sys/class/infiniband/mlx5_1/ports/1":            {Mode: fs.ModeDir},
			"sys/class/infiniband/mlx5_1/ports/1/phys_state": {Data: []byte("5: LinkUp")},
			"sys/class/infiniband/mlx5_1/ports/1/state":      {Data: []byte("4: ACTIVE")},
			// mlx5_2 with port 1
			"sys/class/infiniband/mlx5_2/ports/1":            {Mode: fs.ModeDir},
			"sys/class/infiniband/mlx5_2/ports/1/phys_state": {Data: []byte("5: LinkUp")},
			"sys/class/infiniband/mlx5_2/ports/1/state":      {Data: []byte("4: ACTIVE")},
		},
	}

	mockFS := fileSystem.(*MockFileSystem)

	expectedNoError := []NicHealthEvent{
		{NicType: Infiniband, Name: "mlx5_0", Message: nicIsDetected, IsHealthyEvent: true},
		{NicType: Infiniband, Name: "mlx5_0_1", Message: "Port is healthy", IsHealthyEvent: true},
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
			NicType: Infiniband,
			Name:    "mlx5_0_1",
			Message: "state: 1: Down",
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
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true},
				{Name: "mlx5_test_1", NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true},
			},
		},
		{
			name:                 "MonitorTypeAll_LinkLayerEth_Healthy",
			monitorNetworkType:   MonitorNetworkTypeAll,
			linkLayer:            "Ethernet",
			initialPortState:     "4: ACTIVE",
			initialPortPhysState: "5: LinkUp",
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true},
				{Name: "mlx5_test_1", NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true},
			},
		},
		{
			name:                 "MonitorTypeRoCE_LinkLayerEth_Healthy",
			monitorNetworkType:   MonitorNetworkTypeRoCE,
			linkLayer:            "Ethernet",
			initialPortState:     "4: ACTIVE",
			initialPortPhysState: "5: LinkUp",
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true},
				{Name: "mlx5_test_1", NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true},
			},
		},
		{
			name:                 "MonitorTypeRoCE_LinkLayerIB_Healthy_ShouldBeSkipped",
			monitorNetworkType:   MonitorNetworkTypeRoCE,
			linkLayer:            "InfiniBand",
			initialPortState:     "4: ACTIVE",
			initialPortPhysState: "5: LinkUp",
			expectedEvents: []NicHealthEvent{ // Only device detection, port is skipped
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true},
			},
		},
		{
			name:                 "MonitorTypeInfiniBand_LinkLayerIB_Healthy",
			monitorNetworkType:   MonitorNetworkTypeInfiniBand,
			linkLayer:            "InfiniBand",
			initialPortState:     "4: ACTIVE",
			initialPortPhysState: "5: LinkUp",
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true},
				{Name: "mlx5_test_1", NicType: Infiniband, Message: portIsHealthy, IsHealthyEvent: true},
			},
		},
		{
			name:                 "MonitorTypeInfiniBand_LinkLayerEth_Healthy_ShouldBeSkipped",
			monitorNetworkType:   MonitorNetworkTypeInfiniBand,
			linkLayer:            "Ethernet",
			initialPortState:     "4: ACTIVE",
			initialPortPhysState: "5: LinkUp",
			expectedEvents: []NicHealthEvent{ // Only device detection, port is skipped
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true},
			},
		},
		{
			name:                 "MonitorTypeRoCE_LinkLayerEth_NewUnhealthyPort",
			monitorNetworkType:   MonitorNetworkTypeRoCE,
			linkLayer:            "Ethernet",
			initialPortState:     "1: DOWN",
			initialPortPhysState: "2: Polling",
			expectedEvents: []NicHealthEvent{
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true},
				{
					Name:           "mlx5_test_1",
					NicType:        Infiniband,
					Message:        "state: 1: DOWN, phys_state: 2: Polling",
					IsHealthyEvent: false,
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
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true},
				{
					Name:           "mlx5_test_1",
					NicType:        Infiniband,
					Message:        "state: 1: DOWN, phys_state: 2: Polling",
					IsHealthyEvent: false,
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
				{Name: "mlx5_test", NicType: Infiniband, Message: nicIsDetected, IsHealthyEvent: true},
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
			require.ElementsMatch(t, tc.expectedEvents, actualEvents)
		})
	}
}
