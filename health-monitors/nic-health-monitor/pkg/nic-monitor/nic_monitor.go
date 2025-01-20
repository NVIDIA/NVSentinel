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
	"regexp"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"k8s.io/klog"
)

type NicType int

const (
	// states
	doesNotExistState = "state: Does Not Exist"
	existsState       = "state: Exists"
)

const (
	Ethernet NicType = iota
	Infiniband
)

var (
	pollingLoopProcessingDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "nic_monitor_polling_loop_processing_duration_milliseconds",
		Help:    "The processing time for each polling loop in milliseconds (excluding the polling interval wait time)",
		Buckets: prometheus.LinearBuckets(0, 10, 500),
	})
)

type NicMonitorConfig struct {
	ExclusionRegexes                                 []string
	PollingIntervalInMilliseconds                    int
	MaxRetryDurationForDownDetectedNICInMilliseconds int
	RetryIntervalForDownDetectedNICInMilliseconds    int
	MaxRetriesForRetryableError                      int
	RetryDelaySecondsForRetryableError               int
}

type NicMonitor interface {
	Monitor(config *NicMonitorConfig) ([]NicHealthEvent, error)
}

type NicHealthMonitor struct {
	EventChan     chan *[]NicHealthEvent
	Monitors      []NicMonitor
	monitorConfig *NicMonitorConfig
}

func NewNicHealthMonitor(config *NicMonitorConfig) (*NicHealthMonitor, error) {
	collector := &NicHealthMonitor{
		EventChan:     make(chan *[]NicHealthEvent),
		monitorConfig: config,
	}

	scanAndRegisterNics(collector)

	return collector, nil
}

func scanAndRegisterNics(collector *NicHealthMonitor) {
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

func (c *NicHealthMonitor) Close() error {
	return nil
}

func (c *NicHealthMonitor) Run() error {
	klog.Info("Collecting NIC events")

	ticker := time.NewTicker(time.Duration(c.monitorConfig.PollingIntervalInMilliseconds) * time.Millisecond)

	defer ticker.Stop()

	for range ticker.C {
		start := time.Now()

		for _, monitor := range c.Monitors {
			events, err := monitor.Monitor(c.monitorConfig)
			if err != nil {
				return fmt.Errorf("error occurred while monitoring: %w", err)
			}

			if len(events) != 0 {
				c.EventChan <- &events
			}
		}

		duration := float64(time.Since(start).Milliseconds())
		pollingLoopProcessingDuration.Observe(duration)
	}

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

type NicHealthEvent struct {
	NicType        NicType // e.g., "Ethernet", "Infiniband"
	Name           string
	Message        string
	IsHealthyEvent bool
}
