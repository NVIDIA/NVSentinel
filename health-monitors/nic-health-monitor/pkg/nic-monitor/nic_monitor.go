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
	"regexp"
	"time"

	"k8s.io/klog"
)

type NicType int

const (
	Ethernet NicType = iota
	Infiniband
)

type NicMonitorConfig struct {
	ExclusionRegexes []string
}

type NicMonitor interface {
	Monitor(config *NicMonitorConfig) ([]NicErrorEvent, error)
}

type NicErrorMonitor struct {
	EventChan     chan *[]NicErrorEvent
	Monitors      []NicMonitor
	monitorConfig *NicMonitorConfig
}

func NewNicErrorMonitor(config *NicMonitorConfig) (*NicErrorMonitor, error) {
	collector := &NicErrorMonitor{
		EventChan:     make(chan *[]NicErrorEvent),
		monitorConfig: config,
	}

	scanAndRegisterNics(collector)

	return collector, nil
}

func scanAndRegisterNics(collector *NicErrorMonitor) {
	// check if the infiniband directory exists
	if _, err := os.Stat(SYS_CLASS_INFINIBAND_PATH); err != nil {
		if !os.IsNotExist(err) {
			klog.Errorf("error occurred while reading directory info: %v", err)
		}
	} else {
		collector.Monitors = append(collector.Monitors, &InfinibandDeviceMonitor{})
	}

	// check if the ethernet directory exists
	if _, err := os.Stat(SYS_CLASS_NET_PATH); err != nil {
		if !os.IsNotExist(err) {
			klog.Errorf("error occurred while reading directory info: %v", err)
		}
	} else {
		collector.Monitors = append(collector.Monitors, &EthernetDeviceMonitor{})
	}
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
				events, err := monitor.Monitor(c.monitorConfig)
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

// check if a nic name matches any exclusion regex
func isExcluded(name string, exclusionRegexes []string) bool {
	for _, regex := range exclusionRegexes {
		if match, _ := regexp.MatchString(regex, name); match {
			return true
		}
	}

	return false
}

type NicErrorEvent struct {
	NicType NicType // e.g., "Ethernet", "Infiniband"
	Name    string
	Message string
}
