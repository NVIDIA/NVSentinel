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

package checks

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/discovery"
)

// NewHealthEvent builds a HealthEvent populated with the fields that are
// constant across all NIC checks (agent name, component class, timestamp).
// The caller supplies the per-event fields.
func NewHealthEvent(
	nodeName, checkName, message string,
	entities []*pb.Entity,
	isFatal, isHealthy bool,
	action pb.RecommendedAction,
	processingStrategy pb.ProcessingStrategy,
) *pb.HealthEvent {
	return &pb.HealthEvent{
		Version:            1,
		Agent:              AgentName,
		CheckName:          checkName,
		ComponentClass:     ComponentClass,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		Message:            message,
		IsFatal:            isFatal,
		IsHealthy:          isHealthy,
		NodeName:           nodeName,
		RecommendedAction:  action,
		EntitiesImpacted:   entities,
		ProcessingStrategy: processingStrategy,
	}
}

// PortEntities returns the NIC + NICPort entity pair used on port-level
// events so downstream analyzers can pinpoint both the card and the port.
func PortEntities(device string, port int) []*pb.Entity {
	return []*pb.Entity{
		{EntityType: EntityTypeNIC, EntityValue: device},
		{EntityType: EntityTypePort, EntityValue: discovery.PortEntityValue(port)},
	}
}

// DeviceEntities returns a single NIC entity used on device-level events
// (e.g., disappearance).
func DeviceEntities(device string) []*pb.Entity {
	return []*pb.Entity{
		{EntityType: EntityTypeNIC, EntityValue: device},
	}
}
