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
	"time"

	"k8s.io/klog"
)

type NicType int

const (
	Ethernet NicType = iota
	Infiniband
)

type NicMonitor interface {
	Monitor() ([]NicErrorEvent, error)
}

type NicErrorMonitor struct {
	EventChan chan *[]NicErrorEvent
	Monitors  []NicMonitor
}

func NewNicErrorMonitor() (*NicErrorMonitor, error) {
	collector := &NicErrorMonitor{
		EventChan: make(chan *[]NicErrorEvent),
	}

	// TODO (https://jirasw.nvidia.com/browse/NGCC-19001)
	// Replace with register function and make this configurable
	collector.Monitors = append(collector.Monitors, &InfinibandDeviceMonitor{})
	collector.Monitors = append(collector.Monitors, &EthernetDeviceMonitor{})

	return collector, nil
}

func (c *NicErrorMonitor) Close() error {
	return nil
}

func (c *NicErrorMonitor) Run() error {
	klog.Info("Collecting Nic events")

	ticker := time.NewTicker(time.Second)

	func() {
		for range ticker.C {
			for _, monitor := range c.Monitors {
				events, err := monitor.Monitor()
				if err != nil {
					klog.Errorf("error occurred: %v", err)
				} else if len(events) != 0 {
					c.EventChan <- &events
				}
			}
		}
	}()

	return nil
}

type NicErrorEvent struct {
	NicType NicType // e.g., "Ethernet", "Infiniband"
	Name    string
	Message string
}
