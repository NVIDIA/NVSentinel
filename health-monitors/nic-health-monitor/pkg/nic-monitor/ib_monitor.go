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
	"path/filepath"
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

// nolint: gocognit, cyclop
func (m *InfinibandDeviceMonitor) Monitor(config *NicMonitorConfig) ([]NicHealthEvent, error) {
	deviceList, err := GetInfinibandDevices(config.ExclusionRegexes, config.SysClassInfinibandPath)
	if err != nil {
		return nil, err
	}

	events := []NicHealthEvent{}

	// Check if any nic device is disappeared
	for name := range m.Devices {
		device, ok := deviceList[name]
		if !ok {
			events = append(events, NicHealthEvent{
				NicType:        Infiniband,
				Name:           name,
				Message:        doesNotExistState,
				IsHealthyEvent: false,
			})

			continue
		}

		// Check if any port is disappeared
		for portName := range m.Devices[name].Ports {
			if _, ok := device.Ports[portName]; !ok {
				events = append(events, NicHealthEvent{
					NicType:        Infiniband,
					Name:           name + "_" + portName,
					Message:        doesNotExistState,
					IsHealthyEvent: false,
				})
			}
		}
	}

	for deviceName, device := range deviceList {
		oldDevice, oldDeviceExist := m.Devices[deviceName]

		if !oldDeviceExist {
			events = append(events, NicHealthEvent{
				NicType:        Infiniband,
				Name:           device.Name,
				Message:        nicIsDetected,
				IsHealthyEvent: true,
			})
		}

		for portName, port := range device.Ports {
			// Filter based on MonitorNetworkType and link_layer
			if config.MonitorNetworkType == MonitorNetworkTypeRoCE ||
				config.MonitorNetworkType == MonitorNetworkTypeInfiniBand {
				linkLayer, err := getLinkLayer(config, device.Name, portName)
				if err != nil {
					klog.Warningf(
						"Could not determine link_layer for IB port %s on device %s, skipping: %v",
						portName,
						device.Name,
						err,
					)

					continue // Skip this port if link_layer cannot be determined
				}

				if config.MonitorNetworkType == MonitorNetworkTypeRoCE && linkLayer != "Ethernet" {
					continue
				}

				if config.MonitorNetworkType == MonitorNetworkTypeInfiniBand && linkLayer != "InfiniBand" {
					continue
				}
			}

			var oldPort InfiniBandPort

			oldPortExist := false

			if oldDeviceExist {
				var exists bool
				oldPort, exists = oldDevice.Ports[portName]
				oldPortExist = exists
			}

			// port is new
			//nolint
			if !oldPortExist {
				// if port is new and healthy, then create a healthy event
				if port.State == stateActive && port.PhysState == phyStateLinkup {
					events = append(events, NicHealthEvent{
						NicType:        Infiniband,
						Name:           device.Name + "_" + port.Name,
						Message:        portIsHealthy,
						IsHealthyEvent: true,
					})
				} else {
					// Port is new and not healthy, create an unhealthy event
					var msgParts []string
					if port.State != stateActive {
						msgParts = append(msgParts, "state: "+port.State)
					}

					if port.PhysState != phyStateLinkup {
						msgParts = append(msgParts, "phys_state: "+port.PhysState)
					}

					msg := strings.Join(msgParts, ", ")

					events = append(events, NicHealthEvent{
						NicType:        Infiniband,
						Name:           device.Name + "_" + port.Name,
						Message:        msg,
						IsHealthyEvent: false,
					})
				}

				continue
			}

			// old port exists and the state or PhysState have changed
			if port.State != oldPort.State || port.PhysState != oldPort.PhysState {
				if port.State == stateActive && port.PhysState == phyStateLinkup {
					events = append(events, NicHealthEvent{
						NicType:        Infiniband,
						Name:           device.Name + "_" + port.Name,
						Message:        portIsHealthy,
						IsHealthyEvent: true,
					})

					continue
				}

				var msgParts []string

				if port.State != stateActive {
					msgParts = append(msgParts, "state: "+port.State)
				}

				if port.PhysState != phyStateLinkup {
					msgParts = append(msgParts, "phys_state: "+port.PhysState)
				}

				msg := strings.Join(msgParts, ", ")
				events = append(events, NicHealthEvent{
					NicType:        Infiniband,
					Name:           device.Name + "_" + port.Name,
					Message:        msg,
					IsHealthyEvent: false,
				})
			}
		}
	}

	m.Devices = deviceList

	return events, nil
}
