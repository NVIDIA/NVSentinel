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
			"sys/class/net/lo/operstate": {Data: []byte(UNKNOWN_LINK_LAYER)},
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
		{NicType: Ethernet, Name: "eth0", Message: "Device is healthy", IsHealthyEvent: true, LinkLayer: "Ethernet"},
	}
	expectedDownEvent := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: "state: down", LinkLayer: "Ethernet"},
	}

	ethMonitor := &EthernetDeviceMonitor{}
	ethMonitor.devices = make(map[string]EthernetDevice)

	nicConfig := &NicMonitorConfig{
		ExclusionRegexes: nil,
		MaxRetryDurationForDownDetectedNICInMilliseconds: 500,
		RetryIntervalForDownDetectedNICInMilliseconds:    100,
		MonitorNetworkType: MonitorNetworkTypeAll,
		SysClassNetPath:    sysClassNetPath,
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
		{NicType: Ethernet, Name: "eth_new", Message: "state: lowerlayerdown", IsHealthyEvent: false, LinkLayer: "Ethernet"},
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
		{NicType: Ethernet, Name: "eth0", Message: "Device is healthy", IsHealthyEvent: true, LinkLayer: "Ethernet"},
		{NicType: Ethernet, Name: "eth2", Message: "Device is healthy", IsHealthyEvent: true, LinkLayer: "Ethernet"},
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
		{NicType: Ethernet, Name: "eth2", Message: "state: down", LinkLayer: "Ethernet"},
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
		{NicType: Ethernet, Name: "eth0", Message: deviceIsHealthy, IsHealthyEvent: true, LinkLayer: "Ethernet"},
	}, actualEvents)

	// change state to down
	mockFS.Fs["sys/class/net/eth0/operstate"].Data = []byte("down")

	startTime := time.Now()
	actualEvents, err = ethMonitor.Monitor(nicConfig)
	elapsedTime := time.Since(startTime)
	require.NoError(t, err)
	require.Equal(t, []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: "state: down", LinkLayer: "Ethernet"},
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
		{NicType: Ethernet, Name: "eth0", Message: deviceIsHealthy, IsHealthyEvent: true, LinkLayer: "Ethernet"},
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
		{NicType: Ethernet, Name: "eth0", Message: deviceIsHealthy, IsHealthyEvent: true, LinkLayer: "Ethernet"},
	}
	require.Equal(t, expectedEvents, actualEvents)

	// add eth1
	mockFS.Fs["sys/class/net/eth1/device"] = &fstest.MapFile{Mode: fs.ModeDir}
	mockFS.Fs["sys/class/net/eth1/operstate"] = &fstest.MapFile{Data: []byte("up")}
	mockFS.Fs["sys/class/net/eth1/type"] = &fstest.MapFile{Data: []byte("1")}

	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	expectedEvents = []NicHealthEvent{
		{NicType: Ethernet, Name: "eth1", Message: deviceIsHealthy, IsHealthyEvent: true, LinkLayer: "Ethernet"},
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

func TestPhyEthernetMonitorWithRoCEInterfaceFiltering(t *testing.T) {
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			// MONITOR: These match the default RoCE regex patterns
			"sys/class/net/eth0/device":     {Mode: fs.ModeDir},
			"sys/class/net/eth0/operstate":  {Data: []byte("up")},
			"sys/class/net/eth0/type":       {Data: []byte("1")},
			"sys/class/net/rdma0/device":    {Mode: fs.ModeDir},
			"sys/class/net/rdma0/operstate": {Data: []byte("up")},
			"sys/class/net/rdma0/type":      {Data: []byte("1")},
			// DO NOT MONITOR: These don't match the RoCE regex patterns
			"sys/class/net/ens340f1np1/device":    {Mode: fs.ModeDir},
			"sys/class/net/ens340f1np1/operstate": {Data: []byte("down")},
			"sys/class/net/ens340f1np1/type":      {Data: []byte("1")},
			"sys/class/net/ens1100v0/device":      {Mode: fs.ModeDir},
			"sys/class/net/ens1100v0/operstate":   {Data: []byte("down")},
			"sys/class/net/ens1100v0/type":        {Data: []byte("1")},
		},
	}

	expectedHealthyEvents := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: "Device is healthy", IsHealthyEvent: true, LinkLayer: "Ethernet"},
		{NicType: Ethernet, Name: "rdma0", Message: "Device is healthy", IsHealthyEvent: true, LinkLayer: "Ethernet"},
	}

	ethMonitor := &EthernetDeviceMonitor{}

	// RoCE configuration with default regex patterns
	nicConfig := &NicMonitorConfig{
		ExclusionRegexes: []string{},
		MaxRetryDurationForDownDetectedNICInMilliseconds: 500,
		RetryIntervalForDownDetectedNICInMilliseconds:    100,
		MonitorNetworkType:   MonitorNetworkTypeRoCE,
		RoCEInterfaceRegexes: []string{"^rdma\\d+$", "^eth\\d+$"},
		SysClassNetPath:      sysClassNetPath,
	}

	// Only eth0 and rdma0 should be monitored, ens* interfaces should be filtered out
	actualEvents, err := ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)

	// Sort for comparison
	sort.Slice(
		expectedHealthyEvents,
		func(i, j int) bool { return expectedHealthyEvents[i].Name < expectedHealthyEvents[j].Name },
	)
	sort.Slice(
		actualEvents,
		func(i, j int) bool { return actualEvents[i].Name < actualEvents[j].Name },
	)
	require.Equal(t, expectedHealthyEvents, actualEvents)
}

func TestPhyEthernetMonitorWithRoCEInterfaceFilteringAllNetworkType(t *testing.T) {
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			// MONITOR: These match the RoCE regex patterns
			"sys/class/net/eth0/device":     {Mode: fs.ModeDir},
			"sys/class/net/eth0/operstate":  {Data: []byte("up")},
			"sys/class/net/eth0/type":       {Data: []byte("1")},
			"sys/class/net/rdma0/device":    {Mode: fs.ModeDir},
			"sys/class/net/rdma0/operstate": {Data: []byte("up")},
			"sys/class/net/rdma0/type":      {Data: []byte("1")},
			// DO NOT MONITOR: These don't match the RoCE regex patterns (should be filtered out even with "all")
			"sys/class/net/ens340f1np1/device":    {Mode: fs.ModeDir},
			"sys/class/net/ens340f1np1/operstate": {Data: []byte("up")},
			"sys/class/net/ens340f1np1/type":      {Data: []byte("1")},
		},
	}

	expectedHealthyEvents := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: "Device is healthy", IsHealthyEvent: true, LinkLayer: "Ethernet"},
		{NicType: Ethernet, Name: "rdma0", Message: "Device is healthy", IsHealthyEvent: true, LinkLayer: "Ethernet"},
	}

	ethMonitor := &EthernetDeviceMonitor{}

	// All network type configuration - should still apply RoCE filtering for Ethernet interfaces
	nicConfig := &NicMonitorConfig{
		ExclusionRegexes: []string{},
		MaxRetryDurationForDownDetectedNICInMilliseconds: 500,
		RetryIntervalForDownDetectedNICInMilliseconds:    100,
		MonitorNetworkType:   MonitorNetworkTypeAll,
		RoCEInterfaceRegexes: []string{"^rdma\\d+$", "^eth\\d+$"},
		SysClassNetPath:      sysClassNetPath,
	}

	// Only RoCE-compatible interfaces should be monitored even when MonitorNetworkType is All
	actualEvents, err := ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)

	// Sort for comparison
	sort.Slice(
		expectedHealthyEvents,
		func(i, j int) bool { return expectedHealthyEvents[i].Name < expectedHealthyEvents[j].Name },
	)
	sort.Slice(
		actualEvents,
		func(i, j int) bool { return actualEvents[i].Name < actualEvents[j].Name },
	)
	require.Equal(t, expectedHealthyEvents, actualEvents)
}

func TestIsRoCEInterfaceAllowed(t *testing.T) {
	tests := []struct {
		name                 string
		deviceName           string
		roCEInterfaceRegexes []string
		expectedAllowed      bool
	}{
		{
			name:                 "EmptyRegexList_AllowsAll",
			deviceName:           "ens340f1np1",
			roCEInterfaceRegexes: []string{},
			expectedAllowed:      true,
		},
		{
			name:                 "EthPattern_MatchesEth0",
			deviceName:           "eth0",
			roCEInterfaceRegexes: []string{"^eth\\d+$"},
			expectedAllowed:      true,
		},
		{
			name:                 "EthPattern_DoesNotMatchEns",
			deviceName:           "ens340f1np1",
			roCEInterfaceRegexes: []string{"^eth\\d+$"},
			expectedAllowed:      false,
		},
		{
			name:                 "RdmaPattern_MatchesRdma0",
			deviceName:           "rdma0",
			roCEInterfaceRegexes: []string{"^rdma\\d+$"},
			expectedAllowed:      true,
		},
		{
			name:                 "DefaultPatterns_MatchesEth",
			deviceName:           "eth1",
			roCEInterfaceRegexes: []string{"^rdma\\d+$", "^eth\\d+$"},
			expectedAllowed:      true,
		},
		{
			name:                 "DefaultPatterns_MatchesRdma",
			deviceName:           "rdma2",
			roCEInterfaceRegexes: []string{"^rdma\\d+$", "^eth\\d+$"},
			expectedAllowed:      true,
		},
		{
			name:                 "DefaultPatterns_DoesNotMatchEns",
			deviceName:           "ens1100v0",
			roCEInterfaceRegexes: []string{"^rdma\\d+$", "^eth\\d+$"},
			expectedAllowed:      false,
		},
		{
			name:                 "InvalidRegex_SkipsAndContinues",
			deviceName:           "eth0",
			roCEInterfaceRegexes: []string{"[invalid(regex", "^eth\\d+$"},
			expectedAllowed:      true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actualAllowed := isRoCEInterfaceAllowed(tc.deviceName, tc.roCEInterfaceRegexes)
			require.Equal(t, tc.expectedAllowed, actualAllowed)
		})
	}
}

func TestPhyEthernetMonitorWithEmptyRoCERegexes(t *testing.T) {
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			// MONITOR: All should be monitored when RoCEInterfaceRegexes is empty
			"sys/class/net/eth0/device":           {Mode: fs.ModeDir},
			"sys/class/net/eth0/operstate":        {Data: []byte("up")},
			"sys/class/net/eth0/type":             {Data: []byte("1")},
			"sys/class/net/ens340f1np1/device":    {Mode: fs.ModeDir},
			"sys/class/net/ens340f1np1/operstate": {Data: []byte("up")},
			"sys/class/net/ens340f1np1/type":      {Data: []byte("1")},
			"sys/class/net/ens1100v0/device":      {Mode: fs.ModeDir},
			"sys/class/net/ens1100v0/operstate":   {Data: []byte("up")},
			"sys/class/net/ens1100v0/type":        {Data: []byte("1")},
		},
	}

	ethMonitor := &EthernetDeviceMonitor{}
	ethMonitor.devices = make(map[string]EthernetDevice)

	nicConfig := &NicMonitorConfig{
		ExclusionRegexes:     nil,
		MonitorNetworkType:   MonitorNetworkTypeAll,
		RoCEInterfaceRegexes: []string{}, // Empty - should allow all devices
		SysClassNetPath:      sysClassNetPath,
		MaxRetryDurationForDownDetectedNICInMilliseconds: 1000, // Required for Ethernet monitor
		RetryIntervalForDownDetectedNICInMilliseconds:    100,  // Required for Ethernet monitor
	}

	actualEvents, err := ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)

	// When RoCEInterfaceRegexes is empty, all devices should be monitored
	expectedHealthyEvents := []NicHealthEvent{
		{NicType: Ethernet, Name: "ens1100v0", Message: "Device is healthy", IsHealthyEvent: true, LinkLayer: "Ethernet"},
		{NicType: Ethernet, Name: "ens340f1np1", Message: "Device is healthy", IsHealthyEvent: true, LinkLayer: "Ethernet"},
		{NicType: Ethernet, Name: "eth0", Message: "Device is healthy", IsHealthyEvent: true, LinkLayer: "Ethernet"},
	}

	sort.Slice(actualEvents, func(i, j int) bool {
		return actualEvents[i].Name < actualEvents[j].Name
	})
	sort.Slice(expectedHealthyEvents, func(i, j int) bool {
		return expectedHealthyEvents[i].Name < expectedHealthyEvents[j].Name
	})

	require.Equal(t, expectedHealthyEvents, actualEvents)
}

func TestPhyEthernetMonitorWithRoCEFilteringForAllEthernetDevices(t *testing.T) {
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			// MONITOR: These match the RoCE regex patterns
			"sys/class/net/eth0/device":     {Mode: fs.ModeDir},
			"sys/class/net/eth0/operstate":  {Data: []byte("up")},
			"sys/class/net/eth0/type":       {Data: []byte("1")},
			"sys/class/net/rdma0/device":    {Mode: fs.ModeDir},
			"sys/class/net/rdma0/operstate": {Data: []byte("up")},
			"sys/class/net/rdma0/type":      {Data: []byte("1")},
			// DO NOT MONITOR: These don't match the RoCE regex patterns
			"sys/class/net/ens340f1np1/device":    {Mode: fs.ModeDir},
			"sys/class/net/ens340f1np1/operstate": {Data: []byte("up")},
			"sys/class/net/ens340f1np1/type":      {Data: []byte("1")},
			"sys/class/net/ens1100v0/device":      {Mode: fs.ModeDir},
			"sys/class/net/ens1100v0/operstate":   {Data: []byte("up")},
			"sys/class/net/ens1100v0/type":        {Data: []byte("1")},
		},
	}

	ethMonitor := &EthernetDeviceMonitor{}
	ethMonitor.devices = make(map[string]EthernetDevice)

	nicConfig := &NicMonitorConfig{
		ExclusionRegexes:     nil,
		MonitorNetworkType:   MonitorNetworkTypeAll,
		RoCEInterfaceRegexes: []string{"^rdma\\d+$", "^eth\\d+$"}, // Only rdma* and eth* patterns
		SysClassNetPath:      sysClassNetPath,
		MaxRetryDurationForDownDetectedNICInMilliseconds: 1000, // Required for Ethernet monitor
		RetryIntervalForDownDetectedNICInMilliseconds:    100,  // Required for Ethernet monitor
	}

	actualEvents, err := ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)

	// Only eth* and rdma* interfaces should be monitored, ens* should be filtered out
	expectedHealthyEvents := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: "Device is healthy", IsHealthyEvent: true, LinkLayer: "Ethernet"},
		{NicType: Ethernet, Name: "rdma0", Message: "Device is healthy", IsHealthyEvent: true, LinkLayer: "Ethernet"},
	}

	sort.Slice(actualEvents, func(i, j int) bool {
		return actualEvents[i].Name < actualEvents[j].Name
	})
	sort.Slice(expectedHealthyEvents, func(i, j int) bool {
		return expectedHealthyEvents[i].Name < expectedHealthyEvents[j].Name
	})

	require.Equal(t, expectedHealthyEvents, actualEvents)
}
