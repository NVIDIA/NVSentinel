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

func TestScrapPhyEthernetDevices(t *testing.T) {
	fileSystem = MockFileSystem{
		Fs: fstest.MapFS{
			// DO NOT MONITOR: interface without device and type 772 (lo) should not be monitored
			"sys/class/net/lo":           {Mode: fs.ModeDir},
			"sys/class/net/lo/operstate": {Data: []byte("unknown")},
			"sys/class/net/lo/type":      {Data: []byte("772")},
			// DO NOT MONITOR: ethernet interface (1), but virtual (no device) should not be monitored
			"sys/class/net/docker0/":          {Mode: fs.ModeDir},
			"sys/class/net/docker0/operstate": {Data: []byte("down")},
			"sys/class/net/docker0/type":      {Data: []byte("1")},
			// DO NOT MONITOR: infiniband device is not monitored here (use ib_monitor)
			"sys/class/net/ibP259s154043/":          {Mode: fs.ModeDir},
			"sys/class/net/ibP259s154043/operstate": {Data: []byte("down")},
			"sys/class/net/ibP259s154043/type":      {Data: []byte("32")},
			// DO NOT MONITOR: virtual interface should not be monitored
			"sys/class/net/enP17912s1/device":    {Mode: fs.ModeDir},
			"sys/class/net/enP17912s1/master":    {Mode: fs.ModeDir},
			"sys/class/net/enP17912s1/operstate": {Data: []byte("down")},
			"sys/class/net/enP17912s1/type":      {Data: []byte("1")},
			// DO NOT MONITOR: skip when type attribute is not available
			"sys/class/net/enP17912s2/device":    {Mode: fs.ModeDir},
			"sys/class/net/enP17912s2/operstate": {Data: []byte("down")},
			// MONITOR: ethernet interface with device - up and down
			"sys/class/net/eth0/device":    {Mode: fs.ModeDir},
			"sys/class/net/eth0/operstate": {Data: []byte("up")},
			"sys/class/net/eth0/type":      {Data: []byte("1")},
			"sys/class/net/eth1/device":    {Mode: fs.ModeDir},
			"sys/class/net/eth1/operstate": {Data: []byte("down")},
			"sys/class/net/eth1/type":      {Data: []byte("1")},
		},
	}

	expected := map[string]EthernetDevice{
		"eth1": {Name: "eth1", Operstate: "down"},
		"eth0": {Name: "eth0", Operstate: "up"},
	}

	actualDevs, err := GetPhyEthernetDevices()
	require.NoError(t, err)
	require.NotNil(t, actualDevs)
	require.Equal(t, expected, actualDevs)
}

func TestPhyEthernetMonitor(t *testing.T) {
	fileSystem = MockFileSystem{
		Fs: fstest.MapFS{
			"sys/class/net/eth0/device":    {Mode: fs.ModeDir},
			"sys/class/net/eth0/operstate": {Data: []byte("up")},
			"sys/class/net/eth0/type":      {Data: []byte("1")},
		},
	}

	mockFS := fileSystem.(MockFileSystem)

	expectedNoError := []NicErrorEvent{}
	expectedDown := []NicErrorEvent{
		{
			NicType: Ethernet,
			Name:    "eth0",
			Message: "state: down",
		},
	}

	ethMonitor := &EthernetDeviceMonitor{}

	// eth0 is up, so no error expected
	actualErrors, err := ethMonitor.Monitor()
	require.NoError(t, err)
	require.NotNil(t, actualErrors)
	require.Equal(t, expectedNoError, actualErrors)

	// eth0 become down, so report nic down event
	mockFS.Fs["sys/class/net/eth0/operstate"].Data = []byte("down")
	actualErrors, err = ethMonitor.Monitor()
	require.NoError(t, err)
	require.NotNil(t, actualErrors)
	require.Equal(t, expectedDown, actualErrors)

	// eth0 is still down, but it is not a new error, so do not report it
	actualErrors, err = ethMonitor.Monitor()
	require.NoError(t, err)
	require.NotNil(t, actualErrors)
	require.Equal(t, expectedNoError, actualErrors)

	// eth0 become up - no error reports
	mockFS.Fs["sys/class/net/eth0/operstate"].Data = []byte("up")
	actualErrors, err = ethMonitor.Monitor()
	require.NoError(t, err)
	require.NotNil(t, actualErrors)
	require.Equal(t, expectedNoError, actualErrors)

	// eth0 become down again, so report nic down event
	mockFS.Fs["sys/class/net/eth0/operstate"].Data = []byte("down")
	actualErrors, err = ethMonitor.Monitor()
	require.NoError(t, err)
	require.NotNil(t, actualErrors)
	require.Equal(t, expectedDown, actualErrors)
}
