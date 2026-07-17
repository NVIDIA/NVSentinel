// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package checks defines the Check contract implemented by every NIC
// health check. Each check polls sysfs once per invocation and returns
// zero or more HealthEvent protos describing what changed since the last
// poll. The orchestrator in pkg/monitor handles batching and delivery to
// the platform connector.
package checks

import (
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

const (
	// Check names match the enabledChecks keys in the Helm values.
	InfiniBandStateCheckName       = "InfiniBandStateCheck"
	InfiniBandDegradationCheckName = "InfiniBandDegradationCheck"
	EthernetStateCheckName         = "EthernetStateCheck"
	EthernetDegradationCheckName   = "EthernetDegradationCheck"

	// Agent / component identifiers used in every HealthEvent.
	AgentName      = "nic-health-monitor"
	ComponentClass = "NIC"

	// Entity type labels for events.
	EntityTypeNIC  = "NIC"
	EntityTypePort = "NICPort"

	// InfiniBand logical-state labels (parsed from "N: NAME").
	IBStateActive = "ACTIVE"
	IBStateDown   = "DOWN"

	// InfiniBand physical-state labels.
	IBPhysLinkUp            = "LinkUp"
	IBPhysDisabled          = "Disabled"
	IBPhysPolling           = "Polling"
	IBPhysLinkErrorRecovery = "LinkErrorRecovery"
)

// Check is the interface every poll-driven health check implements.
type Check interface {
	// Name returns the check identifier (e.g., InfiniBandStateCheckName).
	Name() string
	// Run executes a single poll cycle and returns zero or more events.
	Run() ([]*pb.HealthEvent, error)
}

// TransactionalCheck separates observation/event generation from advancement
// of the check's committed state. The monitor uses this contract so a failed
// publication cannot consume a health boundary: Prepare stages the candidate
// poll state, Commit makes it durable after successful delivery (or immediately
// for a zero-event poll), and Discard abandons it after an error.
//
// Run remains part of Check for direct callers and performs Prepare+Commit in
// concrete implementations. Production orchestration should prefer this
// interface whenever it is available.
type TransactionalCheck interface {
	Check
	Prepare() ([]*pb.HealthEvent, error)
	Commit()
	Discard()
}

// CheckCategory indicates whether a check monitors port/device state or
// counters. The orchestrator runs each category on its own polling loop
// so counter checks can run on a fixed fast cadence regardless of the
// configurable state polling interval.
type CheckCategory int

const (
	// StateCheck runs at the user-configurable state polling interval.
	StateCheck CheckCategory = iota
	// CounterCheck runs at a fixed counter polling interval (1s).
	CounterCheck
)

// CategoryOf returns the polling category for a given check name.
func CategoryOf(checkName string) CheckCategory {
	switch checkName {
	case InfiniBandDegradationCheckName, EthernetDegradationCheckName:
		return CounterCheck
	default:
		return StateCheck
	}
}
