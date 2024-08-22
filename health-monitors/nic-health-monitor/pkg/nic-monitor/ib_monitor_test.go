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
	fileSystem = MockFileSystem{
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

	actualDevs, err := GetInfinibandDevices(nil)
	require.NoError(t, err)
	require.NotNil(t, actualDevs)
	require.Equal(t, expected, actualDevs)
}

func TestInfinibandMonitor(t *testing.T) {
	fileSystem = MockFileSystem{
		Fs: fstest.MapFS{
			// mlx5_0 with port 1
			"sys/class/infiniband/mlx5_0/ports/1":            {Mode: fs.ModeDir},
			"sys/class/infiniband/mlx5_0/ports/1/phys_state": {Data: []byte("5: LinkUp")},
			"sys/class/infiniband/mlx5_0/ports/1/state":      {Data: []byte("4: ACTIVE")},
		},
	}

	mockFS := fileSystem.(MockFileSystem)

	expectedNoError := []NicErrorEvent{}

	ibMonitor := &InfinibandDeviceMonitor{}

	nicConfig := &NicMonitorConfig{ExclusionRegexes: nil}

	// mlx5_0 port 1 is up, so no error expected
	actualErrors, err := ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualErrors)
	require.Equal(t, expectedNoError, actualErrors)

	// mlx5_0 port 1 physical link is not ready, so report nic down event
	mockFS.Fs["sys/class/infiniband/mlx5_0/ports/1/phys_state"].Data = []byte("2: Polling")

	expectedPhyStatePolling := []NicErrorEvent{
		{
			NicType: Infiniband,
			Name:    "mlx5_0_1",
			Message: "phys_state: 2: Polling",
		},
	}

	actualErrors, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualErrors)
	require.Equal(t, expectedPhyStatePolling, actualErrors)

	// mlx5_0 port 1 is still down, but it is not a new error, so do not report it
	actualErrors, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualErrors)
	require.Equal(t, expectedNoError, actualErrors)

	// mlx5_0 port 1 become up - no error reports
	mockFS.Fs["sys/class/infiniband/mlx5_0/ports/1/phys_state"].Data = []byte("5: LinkUp")
	actualErrors, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualErrors)
	require.Equal(t, expectedNoError, actualErrors)

	// // eth0 become down again, so report nic down event
	mockFS.Fs["sys/class/infiniband/mlx5_0/ports/1/phys_state"].Data = []byte("2: Polling")
	actualErrors, err = ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualErrors)
	require.Equal(t, expectedPhyStatePolling, actualErrors)

	// mlx5_0 port 1 physical link is not ready, so report nic down event
	mockFS.Fs["sys/class/infiniband/mlx5_0/ports/1/state"].Data = []byte("1: Down")

	expectedStateDown := []NicErrorEvent{
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

func TestInfinibandMonitorWithExclusionRegexes(t *testing.T) {
	fileSystem = MockFileSystem{
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

	mockFS := fileSystem.(MockFileSystem)

	expectedNoError := []NicErrorEvent{}

	ibMonitor := &InfinibandDeviceMonitor{}

	// exclude mlx5_1 and mlx5_2
	nicConfig := &NicMonitorConfig{ExclusionRegexes: []string{"^mlx5_1$", "^mlx5_2$"}}

	// mlx5_0 port 1 is up, and mlx5_1 and mlx5_2 are excluded, so no error expected
	actualErrors, err := ibMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualErrors)
	require.Equal(t, expectedNoError, actualErrors)

	// update mlx5_1 state and verify it is not detected as  it is excluded
	mockFS.Fs["sys/class/infiniband/mlx5_1/ports/1/state"].Data = []byte("1: Down")

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

	expectedStateDown := []NicErrorEvent{
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
