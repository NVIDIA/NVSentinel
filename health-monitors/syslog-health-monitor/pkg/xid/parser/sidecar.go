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

package parser

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/xid/metrics"
	"k8s.io/klog/v2"
)

// SidecarParser implements Parser interface using external sidecar service
type SidecarParser struct {
	url      string
	client   *retryablehttp.Client
	nodeName string
}

// NewSidecarParser creates a new sidecar parser
func NewSidecarParser(endpoint, nodeName string) *SidecarParser {
	return &SidecarParser{
		url:      fmt.Sprintf("%s/decode-xid", endpoint),
		client:   retryablehttp.NewClient(),
		nodeName: nodeName,
	}
}

// Parse sends the message to sidecar service for XID parsing
func (p *SidecarParser) Parse(message string) (*Response, error) {
	reqBody := Request{XIDMessage: message}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		klog.Errorf("error marshalling xid message: %v", err.Error())
		metrics.XidProcessingErrors.WithLabelValues("json_marshal_error", p.nodeName).Inc()

		return nil, fmt.Errorf("error marshalling xid message: %w", err)
	}

	req, err := retryablehttp.NewRequest("POST", p.url, bytes.NewBuffer(jsonBody))
	if err != nil {
		klog.Errorf("error creating request: %v", err.Error())
		metrics.XidProcessingErrors.WithLabelValues("request_creation_error", p.nodeName).Inc()

		return nil, fmt.Errorf("error creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		klog.Errorf("error sending request: %v", err.Error())
		metrics.XidProcessingErrors.WithLabelValues("request_sending_error", p.nodeName).Inc()

		return nil, fmt.Errorf("error sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		klog.Errorf("HTTP request failed with status code: %d", resp.StatusCode)
		metrics.XidProcessingErrors.WithLabelValues("http_status_error", p.nodeName).Inc()

		return nil, fmt.Errorf("HTTP request failed with status code: %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		klog.Errorf("error reading response body: %v", err.Error())
		metrics.XidProcessingErrors.WithLabelValues("response_reading_error", p.nodeName).Inc()

		return nil, fmt.Errorf("error reading response body: %w", err)
	}

	klog.V(4).Infof("Response body: %s", string(bodyBytes))

	var xidResp Response

	err = json.Unmarshal(bodyBytes, &xidResp)
	if err != nil {
		klog.Errorf("error decoding xid response: %v", err.Error())
		metrics.XidProcessingErrors.WithLabelValues("response_decoding_error", p.nodeName).Inc()

		return nil, fmt.Errorf("error decoding xid response: %w", err)
	}

	return &xidResp, nil
}
