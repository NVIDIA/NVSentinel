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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"k8s.io/klog"
)

const (

	// port healthy message
	portIsHealthy = "Port is healthy"

	// NIC is detected
	nicIsDetected = "IB NIC is detected"

	// infiniband states
	stateActive    = "4: ACTIVE"
	phyStateLinkup = "5: LinkUp"
)

type InfiniBandPort struct {
	Name      string
	State     string
	PhysState string
}

type InfiniBandDevice struct {
	Name  string
	Ports map[string]InfiniBandPort
}

type InfinibandDeviceMonitor struct {
	Devices map[string]InfiniBandDevice
}

func GetInfinibandDevices(exclusionRegexList []string,
	sysClassInfinibandPath string) (map[string]InfiniBandDevice, error) {
	deviceList := map[string]InfiniBandDevice{}

	dirs, err := fileSystem.ReadDir(sysClassInfinibandPath)
	if err != nil {
		return nil, err
	}

	for _, device := range dirs {
		deviceName := device.Name()

		if isExcluded(deviceName, exclusionRegexList) {
			continue
		}

		ports, err := GetInfinibandPorts(deviceName, sysClassInfinibandPath)
		if err != nil {
			return nil, err
		}

		deviceList[deviceName] = InfiniBandDevice{deviceName, ports}
	}

	return deviceList, nil
}

func GetInfinibandPorts(devName string, sysClassInfinibandPath string) (map[string]InfiniBandPort, error) {
	portList := map[string]InfiniBandPort{}

	path := filepath.Join(sysClassInfinibandPath, devName, "ports")

	dirs, err := fileSystem.ReadDir(path)
	if err != nil {
		return nil, err
	}

	for _, port := range dirs {
		portName := port.Name()
		state := GetPortState(devName, portName, sysClassInfinibandPath)
		phystate := GetPortPhysState(devName, portName, sysClassInfinibandPath)
		portList[portName] = InfiniBandPort{portName, state, phystate}
	}

	return portList, nil
}

func GetPortState(devname, portName string, sysClassInfinibandPath string) string {
	path := filepath.Join(sysClassInfinibandPath, devname, "ports", portName, "state")

	state, err := fileSystem.ReadFile(path)
	if err != nil {
		return "-1: Unknown"
	}

	return strings.TrimSpace(string(state))
}

func GetPortPhysState(devname, portName string, sysClassInfinibandPath string) string {
	path := filepath.Join(sysClassInfinibandPath, devname, "ports", portName, "phys_state")

	state, err := fileSystem.ReadFile(path)
	if err != nil {
		return "-1: Unknown"
	}

	return strings.TrimSpace(string(state))
}

func getLinkLayer(config *NicMonitorConfig, ibDeviceName string, portName string) (string, error) {
	linkLayerPath := filepath.Join(config.SysClassInfinibandPath, ibDeviceName, "ports", portName, "link_layer")

	content, err := fileSystem.ReadFile(linkLayerPath)
	if err != nil {
		return "", fmt.Errorf("failed to read link_layer for %s port %s: %w", ibDeviceName, portName, err)
	}

	return strings.TrimSpace(string(content)), nil
}

// hasMatchingRoCEInterface checks if an IB device has any network interfaces
// matching the specified regex patterns in /sys/class/infiniband/<device>/device/net
func hasMatchingRoCEInterface(
	ibDeviceName string, roCEInterfaceRegexes []string, config *NicMonitorConfig) (bool, error) {
	if len(roCEInterfaceRegexes) == 0 {
		// If no regex specified, allow all devices
		return true, nil
	}

	netPath := filepath.Join(config.SysClassInfinibandPath, ibDeviceName, "device", "net")

	// Check if the net directory exists
	entries, err := fileSystem.ReadDir(netPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No net directory means no network interfaces for this device
			return false, nil
		}

		return false, fmt.Errorf("failed to read net directory for %s: %w", ibDeviceName, err)
	}

	// Check each network interface against each regex
	for _, entry := range entries {
		interfaceName := entry.Name()

		for _, regexPattern := range roCEInterfaceRegexes {
			regex, err := regexp.Compile(regexPattern)
			if err != nil {
				return false, fmt.Errorf("invalid RoCE interface regex '%s': %w", regexPattern, err)
			}

			if regex.MatchString(interfaceName) {
				return true, nil
			}
		}
	}

	return false, nil
}

// nolint: gocognit, cyclop
func (m *InfinibandDeviceMonitor) Monitor(config *NicMonitorConfig) ([]NicHealthEvent, error) {
	deviceList, err := GetInfinibandDevices(config.ExclusionRegexes, config.SysClassInfinibandPath)
	if err != nil {
		return nil, err
	}

	events := []NicHealthEvent{}

	// Check for disappeared devices and ports
	events = append(events, m.checkDisappearedDevices(deviceList)...)
	events = append(events, m.checkDisappearedPorts(deviceList, config)...)

	// Monitor current devices
	deviceEvents, err := m.monitorDevices(deviceList, config)
	if err != nil {
		return nil, err
	}

	events = append(events, deviceEvents...)

	m.Devices = deviceList

	return events, nil
}

func (m *InfinibandDeviceMonitor) checkDisappearedDevices(
	deviceList map[string]InfiniBandDevice) []NicHealthEvent {
	var events []NicHealthEvent

	for name := range m.Devices {
		if _, ok := deviceList[name]; !ok {
			events = append(events, NicHealthEvent{
				NicType:        Infiniband,
				Name:           name,
				Message:        doesNotExistState,
				IsHealthyEvent: false,
				LinkLayer:      UNKNOWN_LINK_LAYER,
			})
		}
	}

	return events
}

func (m *InfinibandDeviceMonitor) checkDisappearedPorts(
	deviceList map[string]InfiniBandDevice,
	config *NicMonitorConfig) []NicHealthEvent {
	var events []NicHealthEvent

	for name := range m.Devices {
		device, ok := deviceList[name]
		if !ok {
			continue
		}

		// Check if any port is disappeared
		for portName := range m.Devices[name].Ports {
			if _, ok := device.Ports[portName]; !ok {
				// Try to get link layer, but use "unknown" if we can't determine it
				linkLayer := UNKNOWN_LINK_LAYER
				if ll, err := getLinkLayer(config, name, portName); err == nil {
					linkLayer = ll
				}

				events = append(events, NicHealthEvent{
					NicType:        Infiniband,
					Name:           name + "_" + portName,
					Message:        doesNotExistState,
					IsHealthyEvent: false,
					LinkLayer:      linkLayer,
				})
			}
		}
	}

	return events
}

func (m *InfinibandDeviceMonitor) monitorDevices(
	deviceList map[string]InfiniBandDevice,
	config *NicMonitorConfig) ([]NicHealthEvent, error) {
	var events []NicHealthEvent

	for deviceName, device := range deviceList {
		oldDevice, oldDeviceExist := m.Devices[deviceName]

		if !oldDeviceExist {
			events = append(events, NicHealthEvent{
				NicType:        Infiniband,
				Name:           device.Name,
				Message:        nicIsDetected,
				IsHealthyEvent: true,
				LinkLayer:      UNKNOWN_LINK_LAYER, // Device-level event, no specific port
			})
		}

		if !m.shouldMonitorDevice(device, config) {
			continue
		}

		deviceEvents, err := m.monitorDevicePorts(device, oldDevice, oldDeviceExist, config)
		if err != nil {
			return nil, err
		}

		events = append(events, deviceEvents...)
	}

	return events, nil
}

func (m *InfinibandDeviceMonitor) shouldMonitorDevice(device InfiniBandDevice, config *NicMonitorConfig) bool {
	if config.MonitorNetworkType != MonitorNetworkTypeRoCE {
		return true
	}

	hasMatch, err := hasMatchingRoCEInterface(device.Name, config.RoCEInterfaceRegexes, config)
	if err != nil {
		klog.Warningf("Could not check RoCE interfaces for device %s: %v, skipping device", device.Name, err)
		return false
	}

	return hasMatch
}

func (m *InfinibandDeviceMonitor) monitorDevicePorts(device InfiniBandDevice,
	oldDevice InfiniBandDevice,
	oldDeviceExist bool,
	config *NicMonitorConfig) ([]NicHealthEvent, error) {
	var events []NicHealthEvent

	for portName, port := range device.Ports {
		linkLayer, err := getLinkLayer(config, device.Name, portName)
		if err != nil {
			klog.Warningf("Could not determine link_layer for IB port %s on device %s: %v, treating as unknown",
				portName,
				device.Name,
				err,
			)

			linkLayer = UNKNOWN_LINK_LAYER
		}

		if !m.shouldMonitorPort(device, portName, linkLayer, config) {
			continue
		}

		var oldPort InfiniBandPort

		oldPortExist := false

		if oldDeviceExist {
			var exists bool
			oldPort, exists = oldDevice.Ports[portName]
			oldPortExist = exists
		}

		portEvents := m.createPortHealthEvents(device, port, oldPort, oldPortExist, linkLayer)
		events = append(events, portEvents...)
	}

	return events, nil
}

func (m *InfinibandDeviceMonitor) shouldMonitorPort(
	device InfiniBandDevice,
	portName string,
	linkLayer string,
	config *NicMonitorConfig) bool {
	// Apply RoCE interface filtering for Ethernet/unknown link layers
	if linkLayer == "Ethernet" || linkLayer == UNKNOWN_LINK_LAYER {
		hasMatch, err := hasMatchingRoCEInterface(device.Name, config.RoCEInterfaceRegexes, config)
		if err != nil {
			klog.Warningf("Could not check RoCE interfaces for device %s with %s link layer: %v, skipping port %s",
				device.Name,
				linkLayer,
				err,
				portName)

			return false
		}

		if !hasMatch {
			return false
		}
	}

	// Filter based on MonitorNetworkType and link_layer
	if config.MonitorNetworkType == MonitorNetworkTypeRoCE {
		return linkLayer == "Ethernet"
	}

	if config.MonitorNetworkType == MonitorNetworkTypeInfiniBand {
		return linkLayer == "InfiniBand"
	}

	return true
}

func (m *InfinibandDeviceMonitor) createPortHealthEvents(
	device InfiniBandDevice,
	port InfiniBandPort,
	oldPort InfiniBandPort,
	oldPortExist bool,
	linkLayer string) []NicHealthEvent {
	var events []NicHealthEvent

	portName := device.Name + "_" + port.Name

	// New port
	if !oldPortExist {
		if port.State == stateActive && port.PhysState == phyStateLinkup {
			events = append(events, NicHealthEvent{
				NicType:        Infiniband,
				Name:           portName,
				Message:        portIsHealthy,
				IsHealthyEvent: true,
				LinkLayer:      linkLayer,
			})
		} else {
			msg := m.buildUnhealthyMessage(port)
			events = append(events, NicHealthEvent{
				NicType:        Infiniband,
				Name:           portName,
				Message:        msg,
				IsHealthyEvent: false,
				LinkLayer:      linkLayer,
			})
		}

		return events
	}

	// Existing port with state changes
	if port.State != oldPort.State || port.PhysState != oldPort.PhysState {
		if port.State == stateActive && port.PhysState == phyStateLinkup {
			events = append(events, NicHealthEvent{
				NicType:        Infiniband,
				Name:           portName,
				Message:        portIsHealthy,
				IsHealthyEvent: true,
				LinkLayer:      linkLayer,
			})
		} else {
			msg := m.buildUnhealthyMessage(port)
			events = append(events, NicHealthEvent{
				NicType:        Infiniband,
				Name:           portName,
				Message:        msg,
				IsHealthyEvent: false,
				LinkLayer:      linkLayer,
			})
		}
	}

	return events
}

func (m *InfinibandDeviceMonitor) buildUnhealthyMessage(port InfiniBandPort) string {
	var msgParts []string

	if port.State != stateActive {
		msgParts = append(msgParts, "state: "+port.State)
	}

	if port.PhysState != phyStateLinkup {
		msgParts = append(msgParts, "phys_state: "+port.PhysState)
	}

	return strings.Join(msgParts, ", ")
}
