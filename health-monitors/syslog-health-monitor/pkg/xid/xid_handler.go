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

package xid

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/common"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/metadata"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/xid/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/xid/parser"
)

const (
	healthyHealthEventMessage = "No Health Failures"
)

func NewXIDHandler(nodeName, defaultAgentName,
	defaultComponentClass, checkName, xidAnalyserEndpoint, metadataPath string) (*XIDHandler, error) {
	config := parser.ParserConfig{
		NodeName:            nodeName,
		XidAnalyserEndpoint: xidAnalyserEndpoint,
		SidecarEnabled:      xidAnalyserEndpoint != "",
	}

	xidParser, err := parser.CreateParser(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create XID parser: %w", err)
	}

	return &XIDHandler{
		nodeName:              nodeName,
		defaultAgentName:      defaultAgentName,
		defaultComponentClass: defaultComponentClass,
		checkName:             checkName,
		pciToGPUUUID:          make(map[string]string),
		parser:                xidParser,
		metadataReader:        metadata.NewReader(metadataPath),
	}, nil
}

func (xidHandler *XIDHandler) ProcessLine(message string) (*pb.HealthEvents, error) {
	start := time.Now()

	defer func() {
		metrics.XidProcessingLatency.Observe(time.Since(start).Seconds())
	}()

	if pciID, gpuUUID := xidHandler.parseNVRMGPUMapLine(message); pciID != "" && gpuUUID != "" {
		normPCI := xidHandler.normalizePCI(pciID)
		xidHandler.pciToGPUUUID[normPCI] = gpuUUID

		slog.Info("Updated PCI->GPU UUID mapping",
			"pci", normPCI,
			"gpuUUID", gpuUUID)

		return nil, nil
	}

	if uuid := xidHandler.parseGPUResetLine(message); len(uuid) != 0 {
		slog.Info("GPU was reset, creating healthy HealthEvent", "GPU_UUID", uuid)
		return xidHandler.createHealthEventGPUResetEvent(uuid)
	}

	xidResp, err := xidHandler.parser.Parse(message)
	if err != nil {
		slog.Debug("XID parsing failed for message",
			"message", message,
			"error", err)

		return nil, nil
	}

	if xidResp == nil || !xidResp.Success {
		slog.Debug("No XID found in parsing", "message", message)
		return nil, nil
	}

	return xidHandler.createHealthEventFromResponse(xidResp, message), nil
}

func (xidHandler *XIDHandler) parseGPUResetLine(message string) string {
	m := gpuResetMap.FindStringSubmatch(message)
	if len(m) >= 2 {
		return m[1]
	}

	return ""
}

func (xidHandler *XIDHandler) parseNVRMGPUMapLine(message string) (string, string) {
	m := reNvrmMap.FindStringSubmatch(message)
	if len(m) >= 3 {
		return m[1], m[2]
	}

	return "", ""
}

func (xidHandler *XIDHandler) normalizePCI(pci string) string {
	if idx := strings.Index(pci, "."); idx != -1 {
		return pci[:idx]
	}

	return pci
}

func (xidHandler *XIDHandler) determineFatality(recommendedAction pb.RecommendedAction) bool {
	return !slices.Contains([]pb.RecommendedAction{
		pb.RecommendedAction_NONE,
	}, recommendedAction)
}

func (xidHandler *XIDHandler) getGPUUUID(normPCI string) (uuid string, fromMetadata bool) {
	gpuInfo, err := xidHandler.metadataReader.GetGPUByPCI(normPCI)
	if err == nil && gpuInfo != nil {
		return gpuInfo.UUID, true
	}

	if err != nil {
		slog.Error("Error getting GPU UUID from metadata", "pci", normPCI, "error", err)
	}

	if uuid, ok := xidHandler.pciToGPUUUID[normPCI]; ok {
		return uuid, false
	}

	return "", false
}

/*
In addition to the PCI, we will always add the GPU UUID as an impacted entity if it is available from either dmesg
or from the metadata-collector. The COMPONENT_RESET remediation action requires the PCI and GPU UUID are available in
the initial unhealthy event we're sending. Additionally, the corresponding healthy event triggered after the
COMPONENT_RESET requires the same PCI and GPU UUID impacted entities are included as the initial event. As a result,
we will only permit the COMPONENT_RESET action if the GPU UUID was sourced from the metadata-collector to ensure that
the same impacted entities can be fetched after a reset occurs. If the GPU UUID does not exist or is sourced from dmesg,
we will still include it as an impacted entity but override the remediation action from COMPONENT_RESET to RESTART_VM.

Unhealthy event generation:
1. XID 48 error occurs in syslog which includes the PCI 0000:03:00:
Xid (PCI:0000:03:00): 48, pid=91237, name=nv-hostengine, Ch 00000076, errorString CTX SWITCH TIMEOUT, Info 0x3c046

2. Using the metadata-collector, look up the corresponding GPU UUID for PCI 0000:03:00 which is
GPU-455d8f70-2051-db6c-0430-ffc457bff834

3. Include this PCI and GPU UUID in the list of impacted entities in our unhealthy HealthEvent with the COMPONENT_RESET
remediation action.

Healthy event generation:
1. GPU reset occurs in syslog which includes the GPU UUID:
GPU reset executed: GPU-455d8f70-2051-db6c-0430-ffc457bff834

2. Using the metadata-collector, look up the corresponding PCI for the given GPU UUID.

3. Include this PCI and GPU UUID in the list of impacted entities in our healthy HealthEvent.

Implementation details:
- The xid-handler will take care of overriding the remediation action from COMPONENT_RESET to RESTART_VM if the GPU UUID
is not available in the HealthEvent. This prevents either a healthEventOverrides from being required or from each future
module needing to derive whether to proceed with a COMPONENT_RESET or RESTART_VM based on if the GPU UUID is present in
impacted entities (specifically node-drainer needs this determine if we do a partial drain and fault-remediation needs
this for the maintenance resource selection).
- Note that it would be possible to not include the PCI as an impacted entity in COMPONENT_RESET health events which
would allow us to always do a GPU reset if the GPU UUID could be fetched from any source (metadata-collector or dmesg).
Recall that the GPU UUID itself is provided in the syslog GPU reset log line (whereas the PCI needs to be dynamically
looked up from the metadata-collector because Janitor does not accept the PCI as input nor does it look up the PCI
before writing the syslog event). However, we do not want to conditionally add entity impact depending on the needs of
healthy event generation nor do we want to add custom logic to allow the fault-quarantine-module to clear conditions on
a subset of impacted entities recovering.
*/
func (xidHandler *XIDHandler) createHealthEventFromResponse(
	xidResp *parser.Response,
	message string,
) *pb.HealthEvents {
	normPCI := xidHandler.normalizePCI(xidResp.Result.PCIE)
	uuid, fromMetadata := xidHandler.getGPUUUID(normPCI)

	entities := getDefaultImpactedEntities(normPCI, uuid)

	if xidResp.Result.Metadata != nil {
		var metadata []*pb.Entity

		switch xidResp.Result.DecodedXIDStr {
		case "13":
			metadata = getXID13Metadata(xidResp.Result.Metadata)
		case "74":
			metadata = getXID74Metadata(xidResp.Result.Metadata)
		}

		entities = append(entities, metadata...)
	}

	metadata := make(map[string]string)
	if chassisSerial := xidHandler.metadataReader.GetChassisSerial(); chassisSerial != nil {
		metadata["chassis_serial"] = *chassisSerial
	}

	metrics.XidCounterMetric.WithLabelValues(
		xidHandler.nodeName,
		xidResp.Result.DecodedXIDStr,
	).Inc()

	recommendedAction := common.MapActionStringToProto(xidResp.Result.Resolution)
	// If we couldn't look up the GPU UUID from metadata (and either couldn't fetch it or retrieved it from dmesg),
	// then override the recommended action from COMPONENT_RESET to RESTART_VM.
	if !fromMetadata && recommendedAction == pb.RecommendedAction_COMPONENT_RESET {
		slog.Info("Overriding recommended action from COMPONENT_RESET to RESTART_VM", "pci", normPCI, "gpuUUID", uuid)

		recommendedAction = pb.RecommendedAction_RESTART_VM
	}

	event := &pb.HealthEvent{
		Version:            1,
		Agent:              xidHandler.defaultAgentName,
		CheckName:          xidHandler.checkName,
		ComponentClass:     xidHandler.defaultComponentClass,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		EntitiesImpacted:   entities,
		Message:            message,
		IsFatal:            xidHandler.determineFatality(recommendedAction),
		IsHealthy:          false,
		NodeName:           xidHandler.nodeName,
		RecommendedAction:  recommendedAction,
		ErrorCode:          []string{xidResp.Result.DecodedXIDStr},
		Metadata:           metadata,
	}

	return &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}
}

func (xidHandler *XIDHandler) createHealthEventGPUResetEvent(uuid string) (*pb.HealthEvents, error) {
	gpuInfo, err := xidHandler.metadataReader.GetInfoByUUID(uuid)
	// There's no point in sending a healthy HealthEvent with only GPU UUID and not PCI because that healthy HealthEvent
	// will not match all impacted entities tracked by the fault-quarantine-module so we will return an error rather than
	// send the event with partial information.
	if err != nil {
		return nil, fmt.Errorf("failed to look up GPU info using UUID %s: %w", uuid, err)
	}

	if len(gpuInfo.PCIAddress) == 0 {
		return nil, fmt.Errorf("failed to look up PCI info using UUID %s", uuid)
	}

	normPCI := xidHandler.normalizePCI(gpuInfo.PCIAddress)
	entities := getDefaultImpactedEntities(normPCI, uuid)
	event := &pb.HealthEvent{
		Version:            1,
		Agent:              xidHandler.defaultAgentName,
		CheckName:          xidHandler.checkName,
		ComponentClass:     xidHandler.defaultComponentClass,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		EntitiesImpacted:   entities,
		Message:            healthyHealthEventMessage,
		IsFatal:            false,
		IsHealthy:          true,
		NodeName:           xidHandler.nodeName,
		RecommendedAction:  pb.RecommendedAction_NONE,
	}

	return &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}, nil
}

func getXID13Metadata(metadata map[string]string) []*pb.Entity {
	entities := []*pb.Entity{}

	if gpc, ok := metadata["GPC"]; ok {
		entities = append(entities, &pb.Entity{
			EntityType: "GPC", EntityValue: gpc,
		})
	}

	if tpc, ok := metadata["TPC"]; ok {
		entities = append(entities, &pb.Entity{
			EntityType: "TPC", EntityValue: tpc,
		})
	}

	if sm, ok := metadata["SM"]; ok {
		entities = append(entities, &pb.Entity{
			EntityType: "SM", EntityValue: sm,
		})
	}

	return entities
}

func getXID74Metadata(metadata map[string]string) []*pb.Entity {
	entities := []*pb.Entity{}

	if nvlink, ok := metadata["NVLINK"]; ok {
		entities = append(entities, &pb.Entity{
			EntityType: "NVLINK", EntityValue: nvlink,
		})
	}

	for i := 0; i <= 6; i++ {
		key := fmt.Sprintf("REG%d", i)
		if reg, ok := metadata[key]; ok {
			entities = append(entities, &pb.Entity{
				EntityType: key, EntityValue: reg,
			})
		}
	}

	return entities
}

func getDefaultImpactedEntities(pci, uuid string) []*pb.Entity {
	entities := []*pb.Entity{
		{
			EntityType:  "PCI",
			EntityValue: pci,
		},
	}
	if len(uuid) > 0 {
		entities = append(entities, &pb.Entity{
			EntityType:  "GPU_UUID",
			EntityValue: uuid,
		})
	}

	return entities
}
