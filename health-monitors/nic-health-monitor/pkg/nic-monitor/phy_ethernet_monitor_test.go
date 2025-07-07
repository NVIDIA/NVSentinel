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

const (
	// Test message constants
	DEVICE_HEALTHY_MESSAGE = "Device is healthy"
	STATE_DOWN_MESSAGE     = "state: down"

	// File system path suffixes
	DEVICE_SUFFIX    = "/device"
	OPERSTATE_SUFFIX = "/operstate"
	TYPE_SUFFIX      = "/type"

	// Common network interface base paths
	ETH_BASE_PATH         = "sys/class/net"
	ETH0_BASE_PATH        = ETH_BASE_PATH + "/eth0"
	ETH1_BASE_PATH        = ETH_BASE_PATH + "/eth1"
	ETH_NEW_BASE_PATH     = ETH_BASE_PATH + "/eth_new"
	RDMA0_BASE_PATH       = ETH_BASE_PATH + "/rdma0"
	ENS340F1NP1_BASE_PATH = ETH_BASE_PATH + "/ens340f1np1"
	ENS1100VO_BASE_PATH   = ETH_BASE_PATH + "/ens1100v0"

	// Network interface states
	STATE_UP   = "up"
	STATE_DOWN = "down"

	// Network interface types
	ETHERNET_TYPE = "1"

	// Link layer type
	ETHERNET_LINK_LAYER = "Ethernet"

	// Timeout configuration values (in milliseconds)
	DEFAULT_MAX_RETRY_DURATION = 500
	DEFAULT_RETRY_INTERVAL     = 100
	LARGER_MAX_RETRY_DURATION  = 1000
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
			"sys/class/net/docker0/operstate": {Data: []byte(STATE_DOWN)},
			"sys/class/net/docker0/type":      {Data: []byte(ETHERNET_TYPE)},
			// DO NOT MONITOR: infiniband device is not monitored here (use ib_monitor)
			"sys/class/net/ibP259s154043/":          {Mode: fs.ModeDir},
			"sys/class/net/ibP259s154043/operstate": {Data: []byte(STATE_DOWN)},
			"sys/class/net/ibP259s154043/type":      {Data: []byte("32")},
			// DO NOT MONITOR: virtual interface should not be monitored
			"sys/class/net/enP17912s1/device":    {Mode: fs.ModeDir},
			"sys/class/net/enP17912s1/master":    {Mode: fs.ModeDir},
			"sys/class/net/enP17912s1/operstate": {Data: []byte(STATE_DOWN)},
			"sys/class/net/enP17912s1/type":      {Data: []byte(ETHERNET_TYPE)},
			// DO NOT MONITOR: skip when type attribute is not available
			"sys/class/net/enP17912s2/device":    {Mode: fs.ModeDir},
			"sys/class/net/enP17912s2/operstate": {Data: []byte(STATE_DOWN)},
			// MONITOR: ethernet interface with device - up and down
			ETH0_BASE_PATH + DEVICE_SUFFIX:    {Mode: fs.ModeDir},
			ETH0_BASE_PATH + OPERSTATE_SUFFIX: {Data: []byte(STATE_UP)},
			ETH0_BASE_PATH + TYPE_SUFFIX:      {Data: []byte(ETHERNET_TYPE)},
			ETH1_BASE_PATH + DEVICE_SUFFIX:    {Mode: fs.ModeDir},
			ETH1_BASE_PATH + OPERSTATE_SUFFIX: {Data: []byte(STATE_DOWN)},
			ETH1_BASE_PATH + TYPE_SUFFIX:      {Data: []byte(ETHERNET_TYPE)},
		},
	}

	expected := map[string]EthernetDevice{
		"eth1": {Name: "eth1", Operstate: STATE_DOWN},
		"eth0": {Name: "eth0", Operstate: STATE_UP},
	}

	actualDevs, err := GetPhyEthernetDevices(nil, sysClassNetPath)
	require.NoError(t, err)
	require.NotNil(t, actualDevs)
	require.Equal(t, expected, actualDevs)
}

func TestPhyEthernetMonitor(t *testing.T) {
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			ETH0_BASE_PATH + DEVICE_SUFFIX:    {Mode: fs.ModeDir},
			ETH0_BASE_PATH + OPERSTATE_SUFFIX: {Data: []byte(STATE_UP)},
			ETH0_BASE_PATH + TYPE_SUFFIX:      {Data: []byte(ETHERNET_TYPE)},
		},
	}

	mockFS := fileSystem.(*MockFileSystem)

	expectedHealthyEvent := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: DEVICE_HEALTHY_MESSAGE, IsHealthyEvent: true, LinkLayer: ETHERNET_LINK_LAYER},
	}
	expectedDownEvent := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: STATE_DOWN_MESSAGE, LinkLayer: ETHERNET_LINK_LAYER},
	}

	ethMonitor := &EthernetDeviceMonitor{}
	ethMonitor.devices = make(map[string]EthernetDevice)

	nicConfig := &NicMonitorConfig{
		ExclusionRegexes: nil,
		MaxRetryDurationForDownDetectedNICInMilliseconds: DEFAULT_MAX_RETRY_DURATION,
		RetryIntervalForDownDetectedNICInMilliseconds:    DEFAULT_RETRY_INTERVAL,
		MonitorNetworkType: MonitorNetworkTypeAll,
		SysClassNetPath:    sysClassNetPath,
	}

	// eth0 is up, so expect a healthy event
	actualEvents, err := ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)
	require.Equal(t, expectedHealthyEvent, actualEvents)

	// eth0 becomes down, so report NIC down event
	mockFS.Fs[ETH0_BASE_PATH+OPERSTATE_SUFFIX].Data = []byte(STATE_DOWN)
	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)
	require.Equal(t, expectedDownEvent, actualEvents)

	// eth0 is still down, but it is not a new event, so do not report it
	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.Empty(t, actualEvents)

	// eth0 becomes up again, expect a healthy event
	mockFS.Fs[ETH0_BASE_PATH+OPERSTATE_SUFFIX].Data = []byte(STATE_UP)
	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)
	require.Equal(t, expectedHealthyEvent, actualEvents)

	// eth0 becomes down again, so report NIC down event
	mockFS.Fs[ETH0_BASE_PATH+OPERSTATE_SUFFIX].Data = []byte(STATE_DOWN)
	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)
	require.Equal(t, expectedDownEvent, actualEvents)

	// Test for newly discovered unhealthy Ethernet device
	ethMonitor = &EthernetDeviceMonitor{}
	ethMonitor.devices = make(map[string]EthernetDevice)
	mockFS.Fs = fstest.MapFS{
		ETH_NEW_BASE_PATH + DEVICE_SUFFIX:    {Mode: fs.ModeDir},
		ETH_NEW_BASE_PATH + OPERSTATE_SUFFIX: {Data: []byte("lowerlayerdown")},
		ETH_NEW_BASE_PATH + TYPE_SUFFIX:      {Data: []byte(ETHERNET_TYPE)},
	}
	expectedNewUnhealthyEthDeviceEvents := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth_new", Message: "state: lowerlayerdown", IsHealthyEvent: false, LinkLayer: ETHERNET_LINK_LAYER},
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
			ETH0_BASE_PATH + DEVICE_SUFFIX:    {Mode: fs.ModeDir},
			ETH0_BASE_PATH + OPERSTATE_SUFFIX: {Data: []byte(STATE_UP)},
			ETH0_BASE_PATH + TYPE_SUFFIX:      {Data: []byte(ETHERNET_TYPE)},
			ETH1_BASE_PATH + DEVICE_SUFFIX:    {Mode: fs.ModeDir},
			ETH1_BASE_PATH + OPERSTATE_SUFFIX: {Data: []byte(STATE_DOWN)},
			ETH1_BASE_PATH + TYPE_SUFFIX:      {Data: []byte(ETHERNET_TYPE)},
			"sys/class/net/eth2/device":       {Mode: fs.ModeDir},
			"sys/class/net/eth2/operstate":    {Data: []byte(STATE_UP)},
			"sys/class/net/eth2/type":         {Data: []byte(ETHERNET_TYPE)},
			"sys/class/net/eth3/device":       {Mode: fs.ModeDir},
			"sys/class/net/eth3/operstate":    {Data: []byte(STATE_DOWN)},
			"sys/class/net/eth3/type":         {Data: []byte(ETHERNET_TYPE)},
		},
	}

	mockFS := fileSystem.(*MockFileSystem)

	expectedHealthyEvents := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: DEVICE_HEALTHY_MESSAGE, IsHealthyEvent: true, LinkLayer: ETHERNET_LINK_LAYER},
		{NicType: Ethernet, Name: "eth2", Message: DEVICE_HEALTHY_MESSAGE, IsHealthyEvent: true, LinkLayer: ETHERNET_LINK_LAYER},
	}

	ethMonitor := &EthernetDeviceMonitor{}

	// exclusion regexes to exclude eth1 and eth3
	nicConfig := &NicMonitorConfig{
		ExclusionRegexes: []string{"^eth1$", "^eth3$"},
		MaxRetryDurationForDownDetectedNICInMilliseconds: DEFAULT_MAX_RETRY_DURATION,
		RetryIntervalForDownDetectedNICInMilliseconds:    DEFAULT_RETRY_INTERVAL,
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
	mockFS.Fs[ETH1_BASE_PATH+OPERSTATE_SUFFIX].Data = []byte(STATE_UP)
	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.Empty(t, actualEvents)

	// update eth2 to have a state change, expect a down event
	mockFS.Fs["sys/class/net/eth2/operstate"].Data = []byte(STATE_DOWN)
	expectedDownEvent := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth2", Message: STATE_DOWN_MESSAGE, LinkLayer: ETHERNET_LINK_LAYER},
	}
	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.NotNil(t, actualEvents)
	require.Equal(t, expectedDownEvent, actualEvents)

	// update eth3 state to "up" and verify it is not detected as it is excluded
	mockFS.Fs["sys/class/net/eth3/operstate"].Data = []byte(STATE_UP)
	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.Empty(t, actualEvents)
}

func TestMonitorDeviceGoesDownAndStaysDown(t *testing.T) {
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			ETH0_BASE_PATH + DEVICE_SUFFIX:    {Mode: fs.ModeDir},
			ETH0_BASE_PATH + OPERSTATE_SUFFIX: {Data: []byte(STATE_UP)},
			ETH0_BASE_PATH + TYPE_SUFFIX:      {Data: []byte(ETHERNET_TYPE)},
		},
	}

	mockFS := fileSystem.(*MockFileSystem)
	ethMonitor := &EthernetDeviceMonitor{}

	nicConfig := &NicMonitorConfig{
		ExclusionRegexes: nil,
		MaxRetryDurationForDownDetectedNICInMilliseconds: DEFAULT_MAX_RETRY_DURATION,
		RetryIntervalForDownDetectedNICInMilliseconds:    DEFAULT_RETRY_INTERVAL,
		SysClassNetPath: sysClassNetPath,
	}

	// initial state is up
	actualEvents, err := ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.Equal(t, []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: DEVICE_HEALTHY_MESSAGE, IsHealthyEvent: true, LinkLayer: ETHERNET_LINK_LAYER},
	}, actualEvents)

	// change state to down
	mockFS.Fs[ETH0_BASE_PATH+OPERSTATE_SUFFIX].Data = []byte(STATE_DOWN)

	startTime := time.Now()
	actualEvents, err = ethMonitor.Monitor(nicConfig)
	elapsedTime := time.Since(startTime)
	require.NoError(t, err)
	require.Equal(t, []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: STATE_DOWN_MESSAGE, LinkLayer: ETHERNET_LINK_LAYER},
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
			ETH0_BASE_PATH + DEVICE_SUFFIX:    &fstest.MapFile{Mode: fs.ModeDir},
			ETH0_BASE_PATH + OPERSTATE_SUFFIX: &fstest.MapFile{Data: []byte(STATE_UP)},
			ETH0_BASE_PATH + TYPE_SUFFIX:      &fstest.MapFile{Data: []byte(ETHERNET_TYPE)},
		},
	}

	mockFS := fileSystem.(*MockFileSystem)
	ethMonitor := &EthernetDeviceMonitor{}

	nicConfig := &NicMonitorConfig{
		ExclusionRegexes: nil,
		MaxRetryDurationForDownDetectedNICInMilliseconds: DEFAULT_MAX_RETRY_DURATION,
		RetryIntervalForDownDetectedNICInMilliseconds:    DEFAULT_RETRY_INTERVAL,
		SysClassNetPath: sysClassNetPath,
	}

	// initial state is up
	actualEvents, err := ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.Equal(t, []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: DEVICE_HEALTHY_MESSAGE, IsHealthyEvent: true, LinkLayer: ETHERNET_LINK_LAYER},
	}, actualEvents)

	mockFS.mu.Lock()
	mockFS.Fs[ETH0_BASE_PATH+OPERSTATE_SUFFIX].Data = []byte(STATE_DOWN)
	mockFS.mu.Unlock()

	// After 200ms, change state back to up with locking
	time.AfterFunc(200*time.Millisecond, func() {
		mockFS.mu.Lock()
		mockFS.Fs[ETH0_BASE_PATH+OPERSTATE_SUFFIX].Data = []byte(STATE_UP)
		mockFS.mu.Unlock()
	})

	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	require.Empty(t, actualEvents)
}

func TestMonitorDevicesAddedAndRemoved(t *testing.T) {
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			ETH0_BASE_PATH + DEVICE_SUFFIX:    {Mode: fs.ModeDir},
			ETH0_BASE_PATH + OPERSTATE_SUFFIX: {Data: []byte(STATE_UP)},
			ETH0_BASE_PATH + TYPE_SUFFIX:      {Data: []byte(ETHERNET_TYPE)},
		},
	}

	mockFS := fileSystem.(*MockFileSystem)
	ethMonitor := &EthernetDeviceMonitor{}

	nicConfig := &NicMonitorConfig{
		ExclusionRegexes: nil,
		MaxRetryDurationForDownDetectedNICInMilliseconds: DEFAULT_MAX_RETRY_DURATION,
		RetryIntervalForDownDetectedNICInMilliseconds:    DEFAULT_RETRY_INTERVAL,
		SysClassNetPath: sysClassNetPath,
	}

	actualEvents, err := ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	expectedEvents := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: DEVICE_HEALTHY_MESSAGE, IsHealthyEvent: true, LinkLayer: ETHERNET_LINK_LAYER},
	}
	require.Equal(t, expectedEvents, actualEvents)

	// add eth1
	mockFS.Fs[ETH1_BASE_PATH+DEVICE_SUFFIX] = &fstest.MapFile{Mode: fs.ModeDir}
	mockFS.Fs[ETH1_BASE_PATH+OPERSTATE_SUFFIX] = &fstest.MapFile{Data: []byte(STATE_UP)}
	mockFS.Fs[ETH1_BASE_PATH+TYPE_SUFFIX] = &fstest.MapFile{Data: []byte(ETHERNET_TYPE)}

	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	expectedEvents = []NicHealthEvent{
		{NicType: Ethernet, Name: "eth1", Message: DEVICE_HEALTHY_MESSAGE, IsHealthyEvent: true, LinkLayer: ETHERNET_LINK_LAYER},
	}
	require.Equal(t, expectedEvents, actualEvents)

	// remove eth0
	delete(mockFS.Fs, ETH0_BASE_PATH+DEVICE_SUFFIX)
	delete(mockFS.Fs, ETH0_BASE_PATH+OPERSTATE_SUFFIX)
	delete(mockFS.Fs, ETH0_BASE_PATH+TYPE_SUFFIX)

	actualEvents, err = ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)
	// no events because eth0 removal is not reported
	require.Empty(t, actualEvents)
}

func TestPhyEthernetMonitorWithRoCEInterfaceFiltering(t *testing.T) {
	fileSystem = &MockFileSystem{
		Fs: fstest.MapFS{
			// MONITOR: These match the default RoCE regex patterns
			ETH0_BASE_PATH + DEVICE_SUFFIX:     {Mode: fs.ModeDir},
			ETH0_BASE_PATH + OPERSTATE_SUFFIX:  {Data: []byte(STATE_UP)},
			ETH0_BASE_PATH + TYPE_SUFFIX:       {Data: []byte(ETHERNET_TYPE)},
			RDMA0_BASE_PATH + DEVICE_SUFFIX:    {Mode: fs.ModeDir},
			RDMA0_BASE_PATH + OPERSTATE_SUFFIX: {Data: []byte(STATE_UP)},
			RDMA0_BASE_PATH + TYPE_SUFFIX:      {Data: []byte(ETHERNET_TYPE)},
			// DO NOT MONITOR: These don't match the RoCE regex patterns
			ENS340F1NP1_BASE_PATH + DEVICE_SUFFIX:    {Mode: fs.ModeDir},
			ENS340F1NP1_BASE_PATH + OPERSTATE_SUFFIX: {Data: []byte(STATE_DOWN)},
			ENS340F1NP1_BASE_PATH + TYPE_SUFFIX:      {Data: []byte(ETHERNET_TYPE)},
			ENS1100VO_BASE_PATH + DEVICE_SUFFIX:      {Mode: fs.ModeDir},
			ENS1100VO_BASE_PATH + OPERSTATE_SUFFIX:   {Data: []byte(STATE_DOWN)},
			ENS1100VO_BASE_PATH + TYPE_SUFFIX:        {Data: []byte(ETHERNET_TYPE)},
		},
	}

	expectedHealthyEvents := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: DEVICE_HEALTHY_MESSAGE, IsHealthyEvent: true, LinkLayer: ETHERNET_LINK_LAYER},
		{NicType: Ethernet, Name: "rdma0", Message: DEVICE_HEALTHY_MESSAGE, IsHealthyEvent: true, LinkLayer: ETHERNET_LINK_LAYER},
	}

	ethMonitor := &EthernetDeviceMonitor{}

	// RoCE configuration with default regex patterns
	nicConfig := &NicMonitorConfig{
		ExclusionRegexes: []string{},
		MaxRetryDurationForDownDetectedNICInMilliseconds: DEFAULT_MAX_RETRY_DURATION,
		RetryIntervalForDownDetectedNICInMilliseconds:    DEFAULT_RETRY_INTERVAL,
		MonitorNetworkType:   MonitorNetworkTypeRoCE,
		RoCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX, ETH_INTERFACE_REGEX},
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
			ETH0_BASE_PATH + DEVICE_SUFFIX:     {Mode: fs.ModeDir},
			ETH0_BASE_PATH + OPERSTATE_SUFFIX:  {Data: []byte(STATE_UP)},
			ETH0_BASE_PATH + TYPE_SUFFIX:       {Data: []byte(ETHERNET_TYPE)},
			RDMA0_BASE_PATH + DEVICE_SUFFIX:    {Mode: fs.ModeDir},
			RDMA0_BASE_PATH + OPERSTATE_SUFFIX: {Data: []byte(STATE_UP)},
			RDMA0_BASE_PATH + TYPE_SUFFIX:      {Data: []byte(ETHERNET_TYPE)},
			// DO NOT MONITOR: These don't match the RoCE regex patterns (should be filtered out even with "all")
			ENS340F1NP1_BASE_PATH + DEVICE_SUFFIX:    {Mode: fs.ModeDir},
			ENS340F1NP1_BASE_PATH + OPERSTATE_SUFFIX: {Data: []byte(STATE_UP)},
			ENS340F1NP1_BASE_PATH + TYPE_SUFFIX:      {Data: []byte(ETHERNET_TYPE)},
		},
	}

	expectedHealthyEvents := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: DEVICE_HEALTHY_MESSAGE, IsHealthyEvent: true, LinkLayer: ETHERNET_LINK_LAYER},
		{NicType: Ethernet, Name: "rdma0", Message: DEVICE_HEALTHY_MESSAGE, IsHealthyEvent: true, LinkLayer: ETHERNET_LINK_LAYER},
	}

	ethMonitor := &EthernetDeviceMonitor{}

	// All network type configuration - should still apply RoCE filtering for Ethernet interfaces
	nicConfig := &NicMonitorConfig{
		ExclusionRegexes: []string{},
		MaxRetryDurationForDownDetectedNICInMilliseconds: DEFAULT_MAX_RETRY_DURATION,
		RetryIntervalForDownDetectedNICInMilliseconds:    DEFAULT_RETRY_INTERVAL,
		MonitorNetworkType:   MonitorNetworkTypeAll,
		RoCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX, ETH_INTERFACE_REGEX},
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
			roCEInterfaceRegexes: []string{ETH_INTERFACE_REGEX},
			expectedAllowed:      true,
		},
		{
			name:                 "EthPattern_DoesNotMatchEns",
			deviceName:           "ens340f1np1",
			roCEInterfaceRegexes: []string{ETH_INTERFACE_REGEX},
			expectedAllowed:      false,
		},
		{
			name:                 "RdmaPattern_MatchesRdma0",
			deviceName:           "rdma0",
			roCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX},
			expectedAllowed:      true,
		},
		{
			name:                 "DefaultPatterns_MatchesEth",
			deviceName:           "eth1",
			roCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX, ETH_INTERFACE_REGEX},
			expectedAllowed:      true,
		},
		{
			name:                 "DefaultPatterns_MatchesRdma",
			deviceName:           "rdma2",
			roCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX, ETH_INTERFACE_REGEX},
			expectedAllowed:      true,
		},
		{
			name:                 "DefaultPatterns_DoesNotMatchEns",
			deviceName:           "ens1100v0",
			roCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX, ETH_INTERFACE_REGEX},
			expectedAllowed:      false,
		},
		{
			name:                 "InvalidRegex_SkipsAndContinues",
			deviceName:           "eth0",
			roCEInterfaceRegexes: []string{"[invalid(regex", ETH_INTERFACE_REGEX},
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
			ETH0_BASE_PATH + DEVICE_SUFFIX:           {Mode: fs.ModeDir},
			ETH0_BASE_PATH + OPERSTATE_SUFFIX:        {Data: []byte(STATE_UP)},
			ETH0_BASE_PATH + TYPE_SUFFIX:             {Data: []byte(ETHERNET_TYPE)},
			ENS340F1NP1_BASE_PATH + DEVICE_SUFFIX:    {Mode: fs.ModeDir},
			ENS340F1NP1_BASE_PATH + OPERSTATE_SUFFIX: {Data: []byte(STATE_UP)},
			ENS340F1NP1_BASE_PATH + TYPE_SUFFIX:      {Data: []byte(ETHERNET_TYPE)},
			ENS1100VO_BASE_PATH + DEVICE_SUFFIX:      {Mode: fs.ModeDir},
			ENS1100VO_BASE_PATH + OPERSTATE_SUFFIX:   {Data: []byte(STATE_UP)},
			ENS1100VO_BASE_PATH + TYPE_SUFFIX:        {Data: []byte(ETHERNET_TYPE)},
		},
	}

	ethMonitor := &EthernetDeviceMonitor{}
	ethMonitor.devices = make(map[string]EthernetDevice)

	nicConfig := &NicMonitorConfig{
		ExclusionRegexes:     nil,
		MonitorNetworkType:   MonitorNetworkTypeAll,
		RoCEInterfaceRegexes: []string{}, // Empty - should allow all devices
		SysClassNetPath:      sysClassNetPath,
		MaxRetryDurationForDownDetectedNICInMilliseconds: LARGER_MAX_RETRY_DURATION, // Required for Ethernet monitor
		RetryIntervalForDownDetectedNICInMilliseconds:    DEFAULT_RETRY_INTERVAL,    // Required for Ethernet monitor
	}

	actualEvents, err := ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)

	// When RoCEInterfaceRegexes is empty, all devices should be monitored
	expectedHealthyEvents := []NicHealthEvent{
		{NicType: Ethernet, Name: "ens1100v0", Message: DEVICE_HEALTHY_MESSAGE, IsHealthyEvent: true, LinkLayer: ETHERNET_LINK_LAYER},
		{NicType: Ethernet, Name: "ens340f1np1", Message: DEVICE_HEALTHY_MESSAGE, IsHealthyEvent: true, LinkLayer: ETHERNET_LINK_LAYER},
		{NicType: Ethernet, Name: "eth0", Message: DEVICE_HEALTHY_MESSAGE, IsHealthyEvent: true, LinkLayer: ETHERNET_LINK_LAYER},
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
			ETH0_BASE_PATH + DEVICE_SUFFIX:     {Mode: fs.ModeDir},
			ETH0_BASE_PATH + OPERSTATE_SUFFIX:  {Data: []byte(STATE_UP)},
			ETH0_BASE_PATH + TYPE_SUFFIX:       {Data: []byte(ETHERNET_TYPE)},
			RDMA0_BASE_PATH + DEVICE_SUFFIX:    {Mode: fs.ModeDir},
			RDMA0_BASE_PATH + OPERSTATE_SUFFIX: {Data: []byte(STATE_UP)},
			RDMA0_BASE_PATH + TYPE_SUFFIX:      {Data: []byte(ETHERNET_TYPE)},
			// DO NOT MONITOR: These don't match the RoCE regex patterns
			ENS340F1NP1_BASE_PATH + DEVICE_SUFFIX:    {Mode: fs.ModeDir},
			ENS340F1NP1_BASE_PATH + OPERSTATE_SUFFIX: {Data: []byte(STATE_UP)},
			ENS340F1NP1_BASE_PATH + TYPE_SUFFIX:      {Data: []byte(ETHERNET_TYPE)},
			ENS1100VO_BASE_PATH + DEVICE_SUFFIX:      {Mode: fs.ModeDir},
			ENS1100VO_BASE_PATH + OPERSTATE_SUFFIX:   {Data: []byte(STATE_UP)},
			ENS1100VO_BASE_PATH + TYPE_SUFFIX:        {Data: []byte(ETHERNET_TYPE)},
		},
	}

	ethMonitor := &EthernetDeviceMonitor{}
	ethMonitor.devices = make(map[string]EthernetDevice)

	nicConfig := &NicMonitorConfig{
		ExclusionRegexes:     nil,
		MonitorNetworkType:   MonitorNetworkTypeAll,
		RoCEInterfaceRegexes: []string{RDMA_INTERFACE_REGEX, ETH_INTERFACE_REGEX}, // Only rdma* and eth* patterns
		SysClassNetPath:      sysClassNetPath,
		MaxRetryDurationForDownDetectedNICInMilliseconds: LARGER_MAX_RETRY_DURATION, // Required for Ethernet monitor
		RetryIntervalForDownDetectedNICInMilliseconds:    DEFAULT_RETRY_INTERVAL,    // Required for Ethernet monitor
	}

	actualEvents, err := ethMonitor.Monitor(nicConfig)
	require.NoError(t, err)

	// Only eth* and rdma* interfaces should be monitored, ens* should be filtered out
	expectedHealthyEvents := []NicHealthEvent{
		{NicType: Ethernet, Name: "eth0", Message: DEVICE_HEALTHY_MESSAGE, IsHealthyEvent: true, LinkLayer: ETHERNET_LINK_LAYER},
		{NicType: Ethernet, Name: "rdma0", Message: DEVICE_HEALTHY_MESSAGE, IsHealthyEvent: true, LinkLayer: ETHERNET_LINK_LAYER},
	}

	sort.Slice(actualEvents, func(i, j int) bool {
		return actualEvents[i].Name < actualEvents[j].Name
	})
	sort.Slice(expectedHealthyEvents, func(i, j int) bool {
		return expectedHealthyEvents[i].Name < expectedHealthyEvents[j].Name
	})

	require.Equal(t, expectedHealthyEvents, actualEvents)
}
