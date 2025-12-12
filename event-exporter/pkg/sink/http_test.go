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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/event-exporter/pkg/auth"
	"github.com/nvidia/nvsentinel/event-exporter/pkg/transformer"
)

func TestPublish(t *testing.T) {
	tests := []struct {
		name         string
		serverStatus int
		tokenStatus  int
		wantErr      bool
		validateFunc func(t *testing.T, r *http.Request)
	}{
		{
			name:         "successful publish with correct headers",
			serverStatus: http.StatusOK,
			tokenStatus:  http.StatusOK,
			wantErr:      false,
			validateFunc: func(t *testing.T, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("Method = %v, want POST", r.Method)
				}

				contentType := r.Header.Get("Content-Type")
				if contentType != "application/cloudevents+json" {
					t.Errorf("Content-Type = %v, want application/cloudevents+json", contentType)
				}

				auth := r.Header.Get("Authorization")
				if auth != "Bearer test-token-123" {
					t.Errorf("Authorization = %v, want Bearer test-token-123", auth)
				}

				var event transformer.CloudEvent
				if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
					t.Fatalf("Failed to decode body: %v", err)
				}

				if event.ID != "test-event-id" {
					t.Errorf("Event ID = %v, want test-event-id", event.ID)
				}
			},
		},
		{
			name:         "server error",
			serverStatus: http.StatusInternalServerError,
			tokenStatus:  http.StatusOK,
			wantErr:      true,
		},
		{
			name:         "token fetch failure",
			serverStatus: http.StatusOK,
			tokenStatus:  http.StatusUnauthorized,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.validateFunc != nil {
					tt.validateFunc(t, r)
				}
				w.WriteHeader(tt.serverStatus)
			}))
			defer server.Close()

			tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.tokenStatus)
				if tt.tokenStatus == http.StatusOK {
					json.NewEncoder(w).Encode(map[string]any{
						"access_token": "test-token-123",
						"expires_in":   3600,
					})
				}
			}))
			defer tokenServer.Close()

			tokenProvider := auth.NewTokenProvider(tokenServer.URL, "client-id", "client-secret", "scope", false)
			sink := NewHTTPSink(server.URL, 10*time.Second, tokenProvider, false)

			event := &transformer.CloudEvent{
				SpecVersion: "1.0",
				Type:        "com.nvidia.nvsentinel.health.v1",
				Source:      "nvsentinel://test-cluster/healthevents",
				ID:          "test-event-id",
				Time:        "2025-01-15T10:30:00Z",
				Data:        map[string]any{},
			}

			ctx := context.Background()
			err := sink.Publish(ctx, event)

			if (err != nil) != tt.wantErr {
				t.Errorf("Publish() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
