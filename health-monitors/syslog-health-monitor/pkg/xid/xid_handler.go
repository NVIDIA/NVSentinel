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

package xid

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/klog/v2"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/common"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/protos"
)

func NewXIDHandler(nodeName, defaultAgentName,
	defaultComponentClass, checkName, xidAnalyserEndpoint string) (*XIDHandler, error) {
	return &XIDHandler{
		nodeName:              nodeName,
		defaultAgentName:      defaultAgentName,
		defaultComponentClass: defaultComponentClass,
		checkName:             checkName,
		pciToGPUUUID:          make(map[string]string),
		url:                   fmt.Sprintf("%s/decode-xid", xidAnalyserEndpoint),
		client:                retryablehttp.NewClient(),
	}, nil
}

func (xidHandler *XIDHandler) ProcessLine(message string) (*pb.HealthEvents, error) {
	if pciID, gpuUUID := xidHandler.parseNVRMGPUMapLine(message); pciID != "" && gpuUUID != "" {
		normPCI := xidHandler.normalizePCI(pciID)
		xidHandler.pciToGPUUUID[normPCI] = gpuUUID

		klog.Infof("Updated PCI->GPU UUID mapping: %s -> %s", normPCI, gpuUUID)

		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()

	xidResp, err := xidHandler.sendRequestToSidecar(ctx, message)
	if err != nil {
		klog.Errorf("error sending request to sidecar: %v", err.Error())
		return nil, fmt.Errorf("error sending request to sidecar: %w", err)
	}

	if !xidResp.Success {
		klog.V(4).Infof("no XID found in %s", message)
		return nil, nil
	}

	xidProcessingLatency.Observe(time.Since(start).Seconds())

	entitesImpacted := []*pb.Entity{
		{EntityType: "PCI", EntityValue: xidResp.Result.PCIE},
	}

	normPCI := xidHandler.normalizePCI(xidResp.Result.PCIE)
	if uuid, ok := xidHandler.pciToGPUUUID[normPCI]; ok && uuid != "" {
		entitesImpacted = append(entitesImpacted, &pb.Entity{
			EntityType: "GPUID", EntityValue: uuid,
		})
	}

	xidCounterMetric.WithLabelValues(
		xidHandler.nodeName,
		xidResp.Result.DecodedXIDStr,
	).Inc()

	recommendedAction, _ := common.MapActionStringToProto(xidResp.Result.Resolution)
	event := &pb.HealthEvent{
		Version:            1,
		Agent:              xidHandler.defaultAgentName,
		CheckName:          xidHandler.checkName,
		ComponentClass:     xidHandler.defaultComponentClass,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		EntitiesImpacted:   entitesImpacted,
		Message:            xidResp.Result.Mnemonic,
		IsFatal:            xidHandler.determineFatality(recommendedAction),
		IsHealthy:          false,
		NodeName:           xidHandler.nodeName,
		RecommendedAction:  recommendedAction,
		ErrorCode:          []string{xidResp.Result.DecodedXIDStr},
	}

	return &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}, nil
}

func (xidHandler *XIDHandler) sendRequestToSidecar(context context.Context, message string) (*Response, error) {
	reqBody := Request{XIDMessage: message}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		klog.Errorf("error marshalling xid message: %v", err.Error())
		xidProcessingErrors.WithLabelValues("json_marshal_error", xidHandler.nodeName).Inc()

		return nil, fmt.Errorf("error marshalling xid message: %w", err)
	}

	req, err := http.NewRequestWithContext(context, "POST", xidHandler.url, bytes.NewBuffer(jsonBody))
	if err != nil {
		klog.Errorf("error creating request: %v", err.Error())
		xidProcessingErrors.WithLabelValues("request_creation_error", xidHandler.nodeName).Inc()

		return nil, fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := xidHandler.client.Do(&retryablehttp.Request{Request: req})
	if err != nil {
		klog.Errorf("error sending request: %v", err.Error())
		xidProcessingErrors.WithLabelValues("request_sending_error", xidHandler.nodeName).Inc()

		return nil, fmt.Errorf("error sending request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		klog.Errorf("error reading response body: %v", err.Error())
		xidProcessingErrors.WithLabelValues("response_reading_error", xidHandler.nodeName).Inc()

		return nil, fmt.Errorf("error reading response body: %w", err)
	}

	klog.V(4).Infof("Response body: %s", string(bodyBytes))

	var xidResp Response

	err = json.Unmarshal(bodyBytes, &xidResp)
	if err != nil {
		klog.Errorf("error decoding xid response: %v", err.Error())
		xidProcessingErrors.WithLabelValues("response_decoding_error", xidHandler.nodeName).Inc()

		return nil, fmt.Errorf("error decoding xid response: %w", err)
	}

	return &xidResp, nil
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

func (xidHandler *XIDHandler) determineFatality(recommendedAction pb.RecommenedAction) bool {
	nonFatalActions := []pb.RecommenedAction{
		pb.RecommenedAction_NONE,
		pb.RecommenedAction_APPLICATION_RESTART,
	}

	return !slices.Contains(nonFatalActions, recommendedAction)
}
