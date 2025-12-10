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

package sxid

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/metadata"
)

func NewSXIDHandler(nodeName, defaultAgentName,
	defaultComponentClass, checkName, metadataPath string) (*SXIDHandler, error) {
	return &SXIDHandler{
		nodeName:              nodeName,
		defaultAgentName:      defaultAgentName,
		defaultComponentClass: defaultComponentClass,
		checkName:             checkName,
		metadataReader:        metadata.NewReader(metadataPath),
	}, nil
}

func (sxidHandler *SXIDHandler) ProcessLine(message string) (*pb.HealthEvents, error) {
	sxidErrorEvent, err := sxidHandler.extractInfoFromNVSwitchErrorMsg(message)
	if err != nil {
		slog.Error("error parsing line", "line", message, "error", err.Error())
		return nil, fmt.Errorf("failed to extract info from NVSwitch error message: %w", err)
	}

	if sxidErrorEvent == nil {
		return nil, nil
	}

	gpuID, gpuInfo, err := sxidHandler.getGPUID(sxidErrorEvent.PCI, sxidErrorEvent.Link)
	if err != nil {
		slog.Error("Error finding GPU ID",
			"pci", sxidErrorEvent.PCI,
			"link", sxidErrorEvent.Link,
			"error", err.Error())

		return nil, fmt.Errorf(
			"error in finding GPU ID with PCI %s and NVLink %d: %w",
			sxidErrorEvent.PCI,
			sxidErrorEvent.Link,
			err,
		)
	}

	sxidCounterMetric.WithLabelValues(
		sxidHandler.nodeName,
		fmt.Sprint(sxidErrorEvent.ErrorNum),
		fmt.Sprint(sxidErrorEvent.Link),
		fmt.Sprint(sxidErrorEvent.NVSwitch),
	).Inc()

	errRes := pb.RecommendedAction_NONE
	if sxidErrorEvent.IsFatal {
		errRes = pb.RecommendedAction_CONTACT_SUPPORT
	}

	entities := []*pb.Entity{
		{EntityType: "NVSWITCH", EntityValue: strconv.Itoa(sxidErrorEvent.NVSwitch)},
		{EntityType: "PCI", EntityValue: sxidErrorEvent.PCI},
		{EntityType: "NVLINK", EntityValue: strconv.Itoa(sxidErrorEvent.Link)},
		{EntityType: "GPU", EntityValue: strconv.Itoa(gpuID)},
		{EntityType: "GPU_UUID", EntityValue: gpuInfo.UUID},
	}

	metadata := make(map[string]string)
	if chassisSerial := sxidHandler.metadataReader.GetChassisSerial(); chassisSerial != nil {
		metadata["chassis_serial"] = *chassisSerial
	}

	event := &pb.HealthEvent{
		Version:            1,
		Agent:              sxidHandler.defaultAgentName,
		CheckName:          sxidHandler.checkName,
		ComponentClass:     sxidHandler.defaultComponentClass,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		EntitiesImpacted:   entities,
		Message:            message,
		IsFatal:            sxidErrorEvent.IsFatal,
		IsHealthy:          false,
		NodeName:           sxidHandler.nodeName,
		RecommendedAction:  errRes,
		ErrorCode:          []string{fmt.Sprint(sxidErrorEvent.ErrorNum)},
		Metadata:           metadata,
	}

	return &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}, nil
}

func (sxidHandler *SXIDHandler) extractInfoFromNVSwitchErrorMsg(line string) (*sxidErrorEvent, error) {
	m := reSXIDPattern.FindStringSubmatch(line)
	if len(m) < 7 {
		return nil, nil
	}

	nvswitch, err := strconv.Atoi(m[1])
	if err != nil {
		return nil, fmt.Errorf("error converting nvswitch ID to int %s: %w", m[1], err)
	}

	errorNum, err := strconv.Atoi(m[3])
	if err != nil {
		return nil, fmt.Errorf("error converting nvswitch SXID to int %s: %w", m[3], err)
	}

	link, err := strconv.Atoi(m[5])
	if err != nil {
		return nil, fmt.Errorf("error converting nvlink ID to int %s: %w", m[5], err)
	}

	return &sxidErrorEvent{
		NVSwitch: nvswitch,
		PCI:      m[2],
		ErrorNum: errorNum,
		IsFatal:  m[4] != "Non-fatal",
		Link:     link,
		Message:  m[6],
	}, nil
}

func (sxidHandler *SXIDHandler) getGPUID(
	pciAddress string,
	nvlink int,
) (int, *model.GPUInfo, error) {
	gpuInfo, _, err := sxidHandler.metadataReader.GetGPUByNVSwitchLink(pciAddress, nvlink)
	if err != nil {
		return -1, nil, fmt.Errorf("failed to lookup GPU from metadata: %w", err)
	}

	return gpuInfo.GPUID, gpuInfo, nil
}
