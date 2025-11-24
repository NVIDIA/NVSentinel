// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sink

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/nvidia/nvsentinel/event-exporter/pkg/auth"
	"github.com/nvidia/nvsentinel/event-exporter/pkg/transformer"
)

type HTTPSink struct {
	endpoint      string
	timeout       time.Duration
	tokenProvider *auth.TokenProvider
	client        *http.Client
}

func NewHTTPSink(
	endpoint string,
	timeout time.Duration,
	tokenProvider *auth.TokenProvider,
) *HTTPSink {
	return &HTTPSink{
		endpoint:      endpoint,
		timeout:       timeout,
		tokenProvider: tokenProvider,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					MinVersion: tls.VersionTLS12,
				},
			},
		},
	}
}

func (s *HTTPSink) Publish(ctx context.Context, event *transformer.CloudEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	token, err := s.tokenProvider.GetToken(ctx)
	if err != nil {
		slog.Error("Failed to get token", "error", err)
		return fmt.Errorf("get token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		slog.Error("Failed to create request", "error", err)
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/cloudevents+json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	resp, err := s.client.Do(req)
	if err != nil {
		slog.Error("Failed to execute request", "error", err)
		return fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	slog.Debug("Event published successfully", "status", resp.StatusCode)

	return nil
}

func (s *HTTPSink) Close(ctx context.Context) error {
	s.client.CloseIdleConnections()
	return nil
}
