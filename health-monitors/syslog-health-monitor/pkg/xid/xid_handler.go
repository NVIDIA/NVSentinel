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

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/common"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/xid/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/xid/parser"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func NewXIDHandler(nodeName, defaultAgentName,
	defaultComponentClass, checkName, xidAnalyserEndpoint string) (*XIDHandler, error) {
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
	}, nil
}

func (xidHandler *XIDHandler) ProcessLine(message string) (*pb.HealthEvents, error) {
	if pciID, gpuUUID := xidHandler.parseNVRMGPUMapLine(message); pciID != "" && gpuUUID != "" {
		normPCI := xidHandler.normalizePCI(pciID)
		xidHandler.pciToGPUUUID[normPCI] = gpuUUID

		slog.Info("Updated PCI->GPU UUID mapping: %s -> %s", normPCI, gpuUUID)

		return nil, nil
	}

	xidResp, err := xidHandler.parser.Parse(message)
	if err != nil {
		slog.Debug("XID parsing failed for message: %s, error: %v", message, err)
		return nil, nil
	}

	if xidResp == nil || !xidResp.Success {
		slog.Debug("No XID found in parsing", "message", message)
		return nil, nil
	}

	return xidHandler.createHealthEventFromResponse(xidResp, message), nil
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

// createHealthEventFromResponse creates a health event from Response (works for both sidecar and local parsing)
func (xidHandler *XIDHandler) createHealthEventFromResponse(xidResp *parser.Response, message string) *pb.HealthEvents {
	entitesImpacted := []*pb.Entity{
		{EntityType: "PCI", EntityValue: xidResp.Result.PCIE},
	}

	normPCI := xidHandler.normalizePCI(xidResp.Result.PCIE)
	if uuid, ok := xidHandler.pciToGPUUUID[normPCI]; ok && uuid != "" {
		entitesImpacted = append(entitesImpacted, &pb.Entity{
			EntityType: "GPU", EntityValue: uuid,
		})
	}

	metrics.XidCounterMetric.WithLabelValues(
		xidHandler.nodeName,
		xidResp.Result.DecodedXIDStr,
	).Inc()

	recommendedAction := common.MapActionStringToProto(xidResp.Result.Resolution)
	event := &pb.HealthEvent{
		Version:            1,
		Agent:              xidHandler.defaultAgentName,
		CheckName:          xidHandler.checkName,
		ComponentClass:     xidHandler.defaultComponentClass,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		EntitiesImpacted:   entitesImpacted,
		Message:            fmt.Sprintf("%s; Resolution: %s", xidResp.Result.Mnemonic, xidResp.Result.Resolution),
		IsFatal:            xidHandler.determineFatality(recommendedAction),
		IsHealthy:          false,
		NodeName:           xidHandler.nodeName,
		RecommendedAction:  recommendedAction,
		ErrorCode:          []string{xidResp.Result.DecodedXIDStr},
		Metadata: map[string]string{
			"JOURNAL_MESSAGE": message,
		},
	}

	return &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}
}
