// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
	"strconv"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/klog/v2"

	lsnvlink "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/nvswitch-health-monitor/pkg/lsnvlink"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/common"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/types"
)

func NewSXIDHandler(errorMappingFilePath, nodeName, defaultAgentName,
	defaultComponentClass, checkName string) (*SXIDHandler, error) {
	errorResolutionMap, err := common.LoadErrorResolutionMap(errorMappingFilePath)
	if err != nil {
		klog.Errorf("error loading resolution map from %s: %s", errorMappingFilePath, err.Error())
		return nil, fmt.Errorf("error loading resolution map: %w", err)
	}

	return &SXIDHandler{
		errorResolutionMap:    errorResolutionMap,
		nodeName:              nodeName,
		defaultAgentName:      defaultAgentName,
		defaultComponentClass: defaultComponentClass,
		checkName:             checkName,
	}, nil
}

func (sxidHandler *SXIDHandler) ProcessLine(message string) (*pb.HealthEvents, error) {
	sxidErrorEvent, err := sxidHandler.extractInfoFromNVSwitchErrorMsg(message)
	if err != nil {
		klog.Errorf("error parsing line %s: %v", message, err)
		return nil, err
	}

	if sxidErrorEvent == nil {
		return nil, nil
	}

	gpuID, err := sxidHandler.getGPUID(sxidErrorEvent.PCI, sxidErrorEvent.Link)
	if err != nil {
		klog.Errorf("error in finding GPU ID with PCI %s and NVLink %d: %s",
			sxidErrorEvent.PCI,
			sxidErrorEvent.Link,
			err.Error(),
		)

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

	errRes := sxidHandler.determineSXIDRecommendedAction(sxidErrorEvent.ErrorNum)

	event := &pb.HealthEvent{
		Version:            1,
		Agent:              sxidHandler.defaultAgentName,
		CheckName:          sxidHandler.checkName,
		ComponentClass:     sxidHandler.defaultComponentClass,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		EntitiesImpacted: []*pb.Entity{
			{EntityType: "NVSWITCH", EntityValue: strconv.Itoa(sxidErrorEvent.NVSwitch)},
			{EntityType: "PCI", EntityValue: sxidErrorEvent.PCI},
			{EntityType: "NVLINK", EntityValue: strconv.Itoa(sxidErrorEvent.Link)},
			{EntityType: "GPU", EntityValue: strconv.Itoa(gpuID)},
		},
		Message:           sxidErrorEvent.Message,
		IsFatal:           sxidErrorEvent.IsFatal,
		IsHealthy:         false,
		NodeName:          sxidHandler.nodeName,
		RecommendedAction: errRes.RecommendedAction,
		ErrorCode:         []string{fmt.Sprint(sxidErrorEvent.ErrorNum)},
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

func (sxidHandler *SXIDHandler) getGPUID(pciAddress string, nvlink int) (int, error) {
	provider := lsnvlink.GetTopologyProvider()

	if !provider.HasNVSwitch() {
		return -1, fmt.Errorf("no NVSwitches present in system")
	}

	// Try dynamic topology using PCI address
	gpuID, err := provider.GetGPUFromPCINVLink(pciAddress, nvlink)
	if err != nil {
		klog.Errorf("Dynamic topology lookup failed for PCI %s: %s", pciAddress, err.Error())
		return -1, fmt.Errorf("dynamic topology lookup failed for PCI %s: %w", pciAddress, err)
	}

	return gpuID, nil
}

func (sxidHandler *SXIDHandler) determineSXIDRecommendedAction(code int) types.ErrorResolution {
	if errRes, found := sxidHandler.errorResolutionMap[code]; found {
		klog.Infof("Found action %s for SXID code %d", errRes.RecommendedAction.String(), code)
		return errRes
	}

	klog.Infof("No action found for SXID code %d, default for REPORT_ISSUE", code)

	return types.ErrorResolution{
		RecommendedAction: pb.RecommenedAction_REPORT_ISSUE,
	}
}
