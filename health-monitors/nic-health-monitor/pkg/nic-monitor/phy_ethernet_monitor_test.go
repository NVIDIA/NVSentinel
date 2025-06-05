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
	"sort"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/require"
)

var (
	sysClassNetPath = "/sys/class/net"
)

func TestScrapPhyEthernetDevices(t *testing.T) {
	fileSystem = &MockFileSystem{
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

	actualDevs, err := GetPhyEthernetDevices(nil, sysClassNetPath)
	require.NoError(t, err)
	require.NotNil(t, actualDevs)
	require.Equal(t, expected, actualDevs)
}

func TestPhyEthernetMonitor(t *testing.T) {
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			"sys/class/net/eth0/device":    {Mode: fs.ModeDir},
			"sys/class/net/eth0/operstate": {Data: []byte("up")},
			"sys/class/net/eth0/type":      {Data: []byte("1")},
		},
	}

	mockFS := fileSystem.(*MockFileSystem)

	expectedHealthyEvent := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: "Device is healthy", IsHealthyEvent: true},
	}
	expectedDownEvent := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: "state: down"},
	}

	ethMonitor := &EthernetDeviceMonitor{}
	ethMonitor.devices = make(map[string]EthernetDevice)

	nicConfig := &NicMonitorConfig{
		ExclusionRegexes: nil,
		MaxRetryDurationForDownDetectedNICInMilliseconds: 500,
		RetryIntervalForDownDetectedNICInMilliseconds:    100,
		MonitorNetworkType: MonitorNetworkTypeAll,
		SysClassNetPath: sysClassNetPath,
	}

	// eth0 is up, so expect a healthy event
	actualEvents, err := ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)
	require.Equal(t, expectedHealthyEvent, actualEvents)

	// eth0 becomes down, so report NIC down event
	mockFS.Fs["sys/class/net/eth0/operstate"].Data = []byte("down")
	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)
	require.Equal(t, expectedDownEvent, actualEvents)

	// eth0 is still down, but it is not a new event, so do not report it
	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.Empty(t, actualEvents)

	// eth0 becomes up again, expect a healthy event
	mockFS.Fs["sys/class/net/eth0/operstate"].Data = []byte("up")
	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)
	require.Equal(t, expectedHealthyEvent, actualEvents)

	// eth0 becomes down again, so report NIC down event
	mockFS.Fs["sys/class/net/eth0/operstate"].Data = []byte("down")
	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)
	require.Equal(t, expectedDownEvent, actualEvents)

	// Test for newly discovered unhealthy Ethernet device
	ethMonitor = &EthernetDeviceMonitor{}
	ethMonitor.devices = make(map[string]EthernetDevice)
	mockFS.Fs = fstest.MapFS{
		"sys/class/net/eth_new/device":    {Mode: fs.ModeDir},
		"sys/class/net/eth_new/operstate": {Data: []byte("lowerlayerdown")},
		"sys/class/net/eth_new/type":      {Data: []byte("1")},
	}
	expectedNewUnhealthyEthDeviceEvents := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth_new", Message: "state: lowerlayerdown", IsHealthyEvent: false},
	}
	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.ElementsMatch(
		t,
		expectedNewUnhealthyEthDeviceEvents,
		actualEvents,
		"Newly discovered unhealthy Ethernet device events mismatch",
	)
}

func TestPhyEthernetMonitorWithExclusionRegexes(t *testing.T) {
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			"sys/class/net/eth0/device":    {Mode: fs.ModeDir},
			"sys/class/net/eth0/operstate": {Data: []byte("up")},
			"sys/class/net/eth0/type":      {Data: []byte("1")},
			"sys/class/net/eth1/device":    {Mode: fs.ModeDir},
			"sys/class/net/eth1/operstate": {Data: []byte("down")},
			"sys/class/net/eth1/type":      {Data: []byte("1")},
			"sys/class/net/eth2/device":    {Mode: fs.ModeDir},
			"sys/class/net/eth2/operstate": {Data: []byte("up")},
			"sys/class/net/eth2/type":      {Data: []byte("1")},
			"sys/class/net/eth3/device":    {Mode: fs.ModeDir},
			"sys/class/net/eth3/operstate": {Data: []byte("down")},
			"sys/class/net/eth3/type":      {Data: []byte("1")},
		},
	}

	mockFS := fileSystem.(*MockFileSystem)

	expectedHealthyEvents := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: "Device is healthy", IsHealthyEvent: true},
		{NicType: Ethernet, Name: "eth2", Message: "Device is healthy", IsHealthyEvent: true},
	}

	ethMonitor := &EthernetDeviceMonitor{}

	// exclusion regexes to exclude eth1 and eth3
	nicConfig := &NicMonitorConfig{
		ExclusionRegexes: []string{"^eth1$", "^eth3$"},
		MaxRetryDurationForDownDetectedNICInMilliseconds: 500,
		RetryIntervalForDownDetectedNICInMilliseconds:    100,
		SysClassNetPath: sysClassNetPath,
	}

	// eth0 and eth2 are not excluded, expect healthy events
	actualEvents, err := ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)
	// sort for comparison
	sort.Slice(
		expectedHealthyEvents,
		func(i, j int) bool { return expectedHealthyEvents[i].Name < expectedHealthyEvents[j].Name },
	)
	sort.Slice(actualEvents, func(i, j int) bool { return actualEvents[i].Name < actualEvents[j].Name })
	require.Equal(t, expectedHealthyEvents, actualEvents)

	// update eth1 state to "up" and verify it is not detected as it is excluded
	mockFS.Fs["sys/class/net/eth1/operstate"].Data = []byte("up")
	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.Empty(t, actualEvents)

	// update eth2 to have a state change, expect a down event
	mockFS.Fs["sys/class/net/eth2/operstate"].Data = []byte("down")
	expectedDownEvent := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth2", Message: "state: down"},
	}
	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)
	require.Equal(t, expectedDownEvent, actualEvents)

	// update eth3 state to "up" and verify it is not detected as it is excluded
	mockFS.Fs["sys/class/net/eth3/operstate"].Data = []byte("up")
	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.Empty(t, actualEvents)
}

func TestMonitorDeviceGoesDownAndStaysDown(t *testing.T) {
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			"sys/class/net/eth0/device":    {Mode: fs.ModeDir},
			"sys/class/net/eth0/operstate": {Data: []byte("up")},
			"sys/class/net/eth0/type":      {Data: []byte("1")},
		},
	}

	mockFS := fileSystem.(*MockFileSystem)
	ethMonitor := &EthernetDeviceMonitor{}

	nicConfig := &NicMonitorConfig{
		ExclusionRegexes: nil,
		MaxRetryDurationForDownDetectedNICInMilliseconds: 500,
		RetryIntervalForDownDetectedNICInMilliseconds:    100,
		SysClassNetPath: sysClassNetPath,
	}

	// initial state is up
	actualEvents, err := ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.Equal(t, []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: deviceIsHealthy, IsHealthyEvent: true},
	}, actualEvents)

	// change state to down
	mockFS.Fs["sys/class/net/eth0/operstate"].Data = []byte("down")

	startTime := time.Now()
	actualEvents, err = ethMonitor.Monitor(nicConfig)
	elapsedTime := time.Since(startTime)
	require.NoError(t, err)
	require.Equal(t, []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: "state: down"},
	}, actualEvents)
	// ensure that it retried for at least MaxRetryDuration
	require.GreaterOrEqual(
		t,
		elapsedTime.Milliseconds(),
		int64(nicConfig.MaxRetryDurationForDownDetectedNICInMilliseconds),
	)
}

func TestMonitorDeviceRecoversBeforeMaxRetryDuration(t *testing.T) {
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			"sys/class/net/eth0/device":    &fstest.MapFile{Mode: fs.ModeDir},
			"sys/class/net/eth0/operstate": &fstest.MapFile{Data: []byte("up")},
			"sys/class/net/eth0/type":      &fstest.MapFile{Data: []byte("1")},
		},
	}

	mockFS := fileSystem.(*MockFileSystem)
	ethMonitor := &EthernetDeviceMonitor{}

	nicConfig := &NicMonitorConfig{
		ExclusionRegexes: nil,
		MaxRetryDurationForDownDetectedNICInMilliseconds: 500,
		RetryIntervalForDownDetectedNICInMilliseconds:    100,
		SysClassNetPath: sysClassNetPath,
	}

	// initial state is up
	actualEvents, err := ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.Equal(t, []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: deviceIsHealthy, IsHealthyEvent: true},
	}, actualEvents)

	mockFS.mu.Lock()
	mockFS.Fs["sys/class/net/eth0/operstate"].Data = []byte("down")
	mockFS.mu.Unlock()

	// After 200ms, change state back to up with locking
	time.AfterFunc(200*time.Millisecond, func() {
		mockFS.mu.Lock()
		mockFS.Fs["sys/class/net/eth0/operstate"].Data = []byte("up")
		mockFS.mu.Unlock()
	})

	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.Empty(t, actualEvents)
}

func TestMonitorDevicesAddedAndRemoved(t *testing.T) {
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			"sys/class/net/eth0/device":    {Mode: fs.ModeDir},
			"sys/class/net/eth0/operstate": {Data: []byte("up")},
			"sys/class/net/eth0/type":      {Data: []byte("1")},
		},
	}

	mockFS := fileSystem.(*MockFileSystem)
	ethMonitor := &EthernetDeviceMonitor{}

	nicConfig := &NicMonitorConfig{
		ExclusionRegexes: nil,
		MaxRetryDurationForDownDetectedNICInMilliseconds: 500,
		RetryIntervalForDownDetectedNICInMilliseconds:    100,
		SysClassNetPath: sysClassNetPath,
	}

	actualEvents, err := ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	expectedEvents := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: deviceIsHealthy, IsHealthyEvent: true},
	}
	require.Equal(t, expectedEvents, actualEvents)

	// add eth1
	mockFS.Fs["sys/class/net/eth1/device"] = &fstest.MapFile{Mode: fs.ModeDir}
	mockFS.Fs["sys/class/net/eth1/operstate"] = &fstest.MapFile{Data: []byte("up")}
	mockFS.Fs["sys/class/net/eth1/type"] = &fstest.MapFile{Data: []byte("1")}

	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	expectedEvents = []NicHealthEvent{
		{NicType: Ethernet, Name: "eth1", Message: deviceIsHealthy, IsHealthyEvent: true},
	}
	require.Equal(t, expectedEvents, actualEvents)

	// remove eth0
	delete(mockFS.Fs, "sys/class/net/eth0/device")
	delete(mockFS.Fs, "sys/class/net/eth0/operstate")
	delete(mockFS.Fs, "sys/class/net/eth0/type")

	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	// no events because eth0 removal is not reported
	require.Empty(t, actualEvents)
}
