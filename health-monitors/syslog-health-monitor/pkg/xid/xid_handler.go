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
	gpuResetFailureErrorCode  = "GPU_RESET_FAILURE"
	gpuResetFailureMessage    = "GPU reset failed, proceeding with a node reboot"
)

func NewXIDHandler(nodeName, defaultAgentName,
	defaultComponentClass, checkName, xidAnalyserEndpoint, metadataPath string,
	processingStrategy pb.ProcessingStrategy,
) (*XIDHandler, error) {
	metadataReader := metadata.NewReader(metadataPath)
	driverVersion := metadataReader.GetDriverVersion()

	config := parser.ParserConfig{
		NodeName:            nodeName,
		XidAnalyserEndpoint: xidAnalyserEndpoint,
		SidecarEnabled:      xidAnalyserEndpoint != "",
		DriverVersion:       driverVersion,
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
		processingStrategy:    processingStrategy,
		pciToGPUUUID:          make(map[string]string),
		parser:                xidParser,
		metadataReader:        metadataReader,
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

	if uuid, success := xidHandler.parseGPUResetLine(message); len(uuid) != 0 {
		slog.Info("GPU reset syslog event received, creating HealthEvent", "GPU_UUID", uuid, "success", success)
		return xidHandler.createHealthEventGPUResetEvent(uuid, success)
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

func (xidHandler *XIDHandler) parseGPUResetLine(message string) (uuid string, success bool) {
	m := gpuResetMap.FindStringSubmatch(message)
	if len(m) >= 3 {
		return m[1], m[2] == "true"
	}

	return "", false
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
GPU reset executed: GPU-455d8f70-2051-db6c-0430-ffc457bff834, success: true

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
		ProcessingStrategy: xidHandler.processingStrategy,
	}

	return &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}
}

/*
A COMPONENT_RESET remediation will result in the following log line being emitted to syslog and consumed by the
syslog-health-monitor (regardless of which health-monitor created the original COMPONENT_RESET unhealthy event):

GPU reset executed: GPU-455d8f70-2051-db6c-0430-ffc457bff834, success: <true/false>

1. Successful resets: will result in a healthy event that can clear XID errors from the SysLogsXIDError check for
matching impacted entities (GPU UUID and PCI ID). Note that if a different health-monitor emitted the original event,
it would need to also check syslog or implement a different detection mechanism for GPU resets (for example the
gpu-health-monitor relies on resets fixing the underlying DCGM watch or by having the nvidia-dcgm pod restarted as part
of the GPU reset workflow).

2. Failed resets: will result in a new unhealthy event from the SysLogsXIDError check with the same impacted entities
(GPU UUID and PCI ID) as the original health event and a RESTART_VM recommended action. Note that this flow will be
triggered regardless of which health-monitor emitted the original COMPONENT_RESET event. This serves as a fallback
where we will reboot a node if a GPU reset fails. This will result in the node being cordoned due to 2 events which are
the original XID event and this subsequent GPU reset failed event. The syslog-health-monitor has logic to clear all
unhealthy events for each of its checks in response to a reboot by sending a healthy event with empty impacted entities.
As a result, we should expect that the node will be uncordoned after the reboot completes.

Example event:

	{
	  createdAt: ISODate('2026-04-30T10:46:50.263Z'),
	  healthevent: {
	    agent: 'syslog-health-monitor',
	    componentclass: 'GPU',
	    checkname: 'SysLogsXIDError',
	    isfatal: true,
	    ishealthy: false,
	    message: ‘GPU reset failed, proceeding with a node reboot',
	    recommendedaction: RESTART_VM,
	    errorcode: [
	      'GPU_RESET_FAILURE'
	    ],
	    entitiesimpacted: [
	      {
	        entitytype: 'PCI',
	        entityvalue: '000b:00:00'
	      },
	      {
	        entitytype: 'GPU_UUID',
	        entityvalue: 'GPU-123’
	      }
	    ],
	    nodename: ‘node-123’,
	  }
	}

Notes:
- A follow-up unhealthy event is required to trigger a full drain in node-drainer because the original COMPONENT_RESET
event would've done a partial drain only against pods using the GPU needing reset.
- If a burst of XIDs occur with both COMPONENT_RESET and RESTART_VM recommended actions and the GPU reset fails, this
logic is not necessary because a reboot would already be triggered. We will rely on the existing fault-remediation
de-duplication logic to only process one of the reboots.
- This logic is meant as a fallback when there are one of more XIDs with COMPONENT_RESET recommended actions which all
result in failed GPU resets. When this logic is triggered, the following steps should be followed:
  - Fix the underlying cause for the GPU reset failure.
  - If the reset failure is isolated to a given XID, override the recommended action from COMPONENT_RESET to RESTART_VM.
  - If the GPU reset failure is not unique to a specific XID error, disable the GPU reset feature to always reboot.
*/
func (xidHandler *XIDHandler) createHealthEventGPUResetEvent(uuid string, success bool) (*pb.HealthEvents, error) {
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
		NodeName:           xidHandler.nodeName,
		ProcessingStrategy: xidHandler.processingStrategy,
	}

	if success {
		event.Message = healthyHealthEventMessage
		event.IsFatal = false
		event.IsHealthy = true
		event.RecommendedAction = pb.RecommendedAction_NONE
	} else {
		event.ErrorCode = []string{gpuResetFailureErrorCode}
		event.Message = gpuResetFailureMessage
		event.IsFatal = true
		event.IsHealthy = false
		event.RecommendedAction = pb.RecommendedAction_RESTART_VM
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
