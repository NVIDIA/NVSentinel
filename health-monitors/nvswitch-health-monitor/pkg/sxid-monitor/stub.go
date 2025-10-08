//go:build !linux

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

package sxid_monitor

import (
	"fmt"
)

// SXIDErrorEvent is a stub for non-Linux platforms
type SXIDErrorEvent struct {
	ErrorNum  int
	IsFatal   bool
	IsHealthy bool
	Message   string
	PCI       string
	NVSwitch  int
	Link      int
}

// SxidEventMonitorConfig is a stub for non-Linux platforms
type SxidEventMonitorConfig struct {
	StateFilePath                 string
	PollingIntervalInMilliseconds int
}

// SxidEventMonitor is a stub for non-Linux platforms
type SxidEventMonitor struct {
	EventChan chan *SXIDErrorEvent
}

// NewSxidEventMonitor is a stub for non-Linux platforms
func NewSxidEventMonitor(config *SxidEventMonitorConfig) (*SxidEventMonitor, error) {
	return &SxidEventMonitor{
		EventChan: make(chan *SXIDErrorEvent),
	}, nil
}

// Run is a stub for non-Linux platforms
func (m *SxidEventMonitor) Run() error {
	return fmt.Errorf("SXID monitoring is only supported on Linux")
}

// Close is a stub for non-Linux platforms
func (m *SxidEventMonitor) Close() {
    close(m.EventChan)
}
