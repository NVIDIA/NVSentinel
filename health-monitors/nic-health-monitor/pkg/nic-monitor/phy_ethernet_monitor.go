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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"k8s.io/klog"
)

const (

	// device healthy message
	deviceIsHealthy = "Device is healthy"

	// ethernet operstate
	operstateUp = "up"
)

type EthernetDevice struct {
	Name      string
	Operstate string
}

type EthernetDeviceMonitor struct {
	devices map[string]EthernetDevice
}

// if this function return err (err != nil), then ignore the bool value
func IsPhyEthernet(dev string, sysClassNetPath string) (bool, error) {
	// This is for creating a dummy interface for testing purpose
	// Creating a dummy interface
	//   $ sudo modprobe dummy
	//   $ sudo ip link add dummy1 type dummy
	// Test by up down the interface
	//   $ sudo ip link set dummy1 [ up | down ]
	// Remove dummy interface
	//   $ sudo ip link delete dummy1 type dummy
	//   $ sudo rmmod dummy
	if strings.Contains(dev, "dummy") {
		return true, nil
	}

	// physical device must be exist
	path := filepath.Join(sysClassNetPath, dev, "device")
	if _, err := fileSystem.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		} else {
			return false, err
		}
	}

	// type must be an ethernet
	path = filepath.Join(sysClassNetPath, dev, "type")
	if _, err := fileSystem.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		} else {
			return false, err
		}
	}

	devtype, err := fileSystem.ReadFile(path)
	if err != nil {
		return false, err
	}

	if strings.TrimSpace(string(devtype)) != "1" {
		return false, nil
	}

	// must be a master interface
	path = filepath.Join(sysClassNetPath, dev, "master")
	if _, err := fileSystem.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return true, nil
		} else {
			return false, err
		}
	}

	return false, nil
}

func GetEthernetOperstate(devname string, sysClassNetPath string) string {
	path := filepath.Join(sysClassNetPath, devname, "operstate")

	state, err := fileSystem.ReadFile(path)
	if err != nil {
		return UNKNOWN_LINK_LAYER
	}

	return strings.TrimSpace(string(state))
}

// isRoCEInterfaceAllowed checks if a device name matches the RoCE interface regexes
func isRoCEInterfaceAllowed(deviceName string, roCEInterfaceRegexes []string) bool {
	if len(roCEInterfaceRegexes) == 0 {
		// If no regex specified, allow all devices
		return true
	}

	for _, regexPattern := range roCEInterfaceRegexes {
		regex, err := regexp.Compile(regexPattern)
		if err != nil {
			klog.Errorf("invalid RoCE interface regex '%s': %v", regexPattern, err)
			continue
		}

		if regex.MatchString(deviceName) {
			return true
		}
	}

	return false
}

func GetPhyEthernetDevices(exclusionRegexList []string, sysClassNetPath string) (map[string]EthernetDevice, error) {
	deviceList := map[string]EthernetDevice{}

	dirs, err := fileSystem.ReadDir(sysClassNetPath)
	if err != nil {
		return nil, err
	}

	for _, device := range dirs {
		deviceName := device.Name()

		if isExcluded(deviceName, exclusionRegexList) {
			continue
		}

		isPhy, err := IsPhyEthernet(deviceName, sysClassNetPath)
		if err != nil {
			klog.Errorf("error on IsPhyEthernet(%s): %v", deviceName, err)
		} else if !isPhy {
			continue
		}

		operstate := GetEthernetOperstate(deviceName, sysClassNetPath)

		deviceList[deviceName] = EthernetDevice{deviceName, operstate}
	}

	return deviceList, nil
}

// GetPhyEthernetDevicesWithRoCEFilter is the new function that supports RoCE filtering
func GetPhyEthernetDevicesWithRoCEFilter(config *NicMonitorConfig) (map[string]EthernetDevice, error) {
	deviceList := map[string]EthernetDevice{}

	dirs, err := fileSystem.ReadDir(config.SysClassNetPath)
	if err != nil {
		return nil, err
	}

	for _, device := range dirs {
		deviceName := device.Name()

		if isExcluded(deviceName, config.ExclusionRegexes) {
			continue
		}

		// Apply RoCE interface filter for all Ethernet devices since they all have Ethernet link layer
		// Only monitor devices that match the RoCE interface patterns (e.g., eth*, rdma*)
		if !isRoCEInterfaceAllowed(deviceName, config.RoCEInterfaceRegexes) {
			// Device doesn't match RoCE interface patterns, skip it
			continue
		}

		isPhy, err := IsPhyEthernet(deviceName, config.SysClassNetPath)
		if err != nil {
			klog.Errorf("error on IsPhyEthernet(%s): %v", deviceName, err)
		} else if !isPhy {
			continue
		}

		operstate := GetEthernetOperstate(deviceName, config.SysClassNetPath)

		deviceList[deviceName] = EthernetDevice{deviceName, operstate}
	}

	return deviceList, nil
}

func (m *EthernetDeviceMonitor) Monitor(config *NicMonitorConfig) ([]NicHealthEvent, error) {
	maxRetryDuration := time.Duration(config.MaxRetryDurationForDownDetectedNICInMilliseconds) * time.Millisecond
	retryInterval := time.Duration(config.RetryIntervalForDownDetectedNICInMilliseconds) * time.Millisecond

	var events []NicHealthEvent

	startTime := time.Now()
	timeout := time.After(maxRetryDuration)
	ticker := time.NewTicker(retryInterval)

	defer ticker.Stop()

tickerLoop:
	for ; true; <-ticker.C {
		select {
		case <-timeout:
			// maxRetryDuration exceeded, perform final check
			deviceList, err := GetPhyEthernetDevicesWithRoCEFilter(config)
			if err != nil {
				return nil, err
			}

			events, _ = m.checkDevices(deviceList, startTime, maxRetryDuration)
			m.updateStoredDevices(deviceList, true)
			break tickerLoop

		default:
			deviceList, err := GetPhyEthernetDevicesWithRoCEFilter(config)
			if err != nil {
				return nil, err
			}

			retryNeeded := false
			events, retryNeeded = m.checkDevices(deviceList, startTime, maxRetryDuration)

			if !retryNeeded {
				// update stored devices with all devices
				m.updateStoredDevices(deviceList, true)
				break tickerLoop
			}

			// update stored devices, but include only devices that are up
			m.updateStoredDevices(deviceList, false)
		}
	}

	return events, nil
}

func (m *EthernetDeviceMonitor) checkDevices(deviceList map[string]EthernetDevice, startTime time.Time,
	maxRetryDuration time.Duration,
) ([]NicHealthEvent, bool) {
	var events []NicHealthEvent

	retryNeeded := false

	for name, device := range deviceList {
		oldDevice, exists := m.devices[name]

		if !exists {
			// device is new
			if device.Operstate == operstateUp {
				events = append(events, createNicHealthEvent(device, true, deviceIsHealthy))
			} else {
				// Device is new and not healthy, create an unhealthy event
				events = append(events, createNicHealthEvent(device, false, "state: "+device.Operstate))
			}

			continue
		}

		// Handle existing device state changes
		event, needsRetry := m.handleExistingDevice(device, oldDevice, startTime, maxRetryDuration)
		if event != nil {
			events = append(events, *event)
		}

		if needsRetry {
			retryNeeded = true
		}
	}

	return events, retryNeeded
}

func (m *EthernetDeviceMonitor) handleExistingDevice(
	device EthernetDevice,
	oldDevice EthernetDevice,
	startTime time.Time,
	maxRetryDuration time.Duration) (*NicHealthEvent, bool) {
	// device existed before and Operstate has changed
	if device.Operstate != oldDevice.Operstate {
		if device.Operstate == operstateUp {
			event := createNicHealthEvent(device, true, deviceIsHealthy)
			return &event, false
		} else {
			// device is down
			if time.Since(startTime) < maxRetryDuration {
				return nil, true
			}

			event := createNicHealthEvent(device, false, "state: "+device.Operstate)

			return &event, false
		}
	}

	return nil, false
}

func createNicHealthEvent(device EthernetDevice, isHealthy bool, message string) NicHealthEvent {
	return NicHealthEvent{
		NicType:        Ethernet,
		Name:           device.Name,
		Message:        message,
		IsHealthyEvent: isHealthy,
		LinkLayer:      "Ethernet", // All interfaces in /sys/class/net are Ethernet link layer
	}
}

func (m *EthernetDeviceMonitor) updateStoredDevices(deviceList map[string]EthernetDevice, includeAllDevices bool) {
	if m.devices == nil {
		m.devices = map[string]EthernetDevice{}
	}

	// delete the devices in old list which are no longer present
	for name := range m.devices {
		if _, exists := deviceList[name]; !exists {
			delete(m.devices, name)
		}
	}

	// update m.Devices with devices from deviceList whose state is not down
	for name, device := range deviceList {
		if includeAllDevices || device.Operstate == operstateUp {
			m.devices[name] = device
		}
	}
}
