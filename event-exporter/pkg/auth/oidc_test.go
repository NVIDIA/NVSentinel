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

package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCreateTokenRequest(t *testing.T) {
	tests := []struct {
		name         string
		tokenURL     string
		clientID     string
		clientSecret string
		scope        string
		wantErr      bool
		validateFunc func(t *testing.T, req *http.Request)
	}{
		{
			name:         "valid request",
			tokenURL:     "https://auth.example.com/token",
			clientID:     "test-client",
			clientSecret: "test-secret",
			scope:        "openid profile",
			wantErr:      false,
			validateFunc: func(t *testing.T, req *http.Request) {
				if req.Method != http.MethodPost {
					t.Errorf("Method = %v, want POST", req.Method)
				}
				if req.URL.String() != "https://auth.example.com/token" {
					t.Errorf("URL = %v, want https://auth.example.com/token", req.URL.String())
				}

				contentType := req.Header.Get("Content-Type")
				if contentType != "application/x-www-form-urlencoded" {
					t.Errorf("Content-Type = %v, want application/x-www-form-urlencoded", contentType)
				}

				expectedAuth := base64.StdEncoding.EncodeToString([]byte("test-client:test-secret"))
				auth := req.Header.Get("Authorization")
				if auth != fmt.Sprintf("Basic %s", expectedAuth) {
					t.Errorf("Authorization = %v, want Basic %s", auth, expectedAuth)
				}

				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("Failed to read body: %v", err)
				}
				formData, err := url.ParseQuery(string(body))
				if err != nil {
					t.Fatalf("Failed to parse form data: %v", err)
				}

				if formData.Get("scope") != "openid profile" {
					t.Errorf("scope = %v, want openid profile", formData.Get("scope"))
				}
				if formData.Get("grant_type") != "client_credentials" {
					t.Errorf("grant_type = %v, want client_credentials", formData.Get("grant_type"))
				}
			},
		},
		{
			name:         "empty scope",
			tokenURL:     "https://auth.example.com/token",
			clientID:     "test-client",
			clientSecret: "test-secret",
			scope:        "",
			wantErr:      false,
			validateFunc: func(t *testing.T, req *http.Request) {
				body, _ := io.ReadAll(req.Body)
				formData, _ := url.ParseQuery(string(body))
				if formData.Get("scope") != "" {
					t.Errorf("scope = %v, want empty", formData.Get("scope"))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := NewTokenProvider(tt.tokenURL, tt.clientID, tt.clientSecret, tt.scope)
			ctx := context.Background()

			req, err := provider.createTokenRequest(ctx)
			if (err != nil) != tt.wantErr {
				t.Errorf("createTokenRequest() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && tt.validateFunc != nil {
				tt.validateFunc(t, req)
			}
		})
	}
}

func TestExecuteTokenRequest(t *testing.T) {
	tests := []struct {
		name              string
		serverHandler     http.HandlerFunc
		wantErr           bool
		wantStatusCode    int
		validateTokenResp func(t *testing.T, resp *tokenResponse)
	}{
		{
			name: "successful token response",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(tokenResponse{
					AccessToken: "test-access-token",
					ExpiresIn:   3600,
				})
			},
			wantErr:        false,
			wantStatusCode: http.StatusOK,
			validateTokenResp: func(t *testing.T, resp *tokenResponse) {
				if resp.AccessToken != "test-access-token" {
					t.Errorf("AccessToken = %v, want test-access-token", resp.AccessToken)
				}
				if resp.ExpiresIn != 3600 {
					t.Errorf("ExpiresIn = %v, want 3600", resp.ExpiresIn)
				}
			},
		},
		{
			name: "server error (500)",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("internal server error"))
			},
			wantErr:        true,
			wantStatusCode: http.StatusInternalServerError,
		},
		{
			name: "rate limit (429)",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte("rate limit exceeded"))
			},
			wantErr:        true,
			wantStatusCode: http.StatusTooManyRequests,
		},
		{
			name: "unauthorized (401)",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte("invalid credentials"))
			},
			wantErr:        true,
			wantStatusCode: http.StatusUnauthorized,
		},
		{
			name: "invalid json response",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("not valid json"))
			},
			wantErr:        true,
			wantStatusCode: http.StatusOK,
		},
		{
			name: "missing access token",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(tokenResponse{
					AccessToken: "",
					ExpiresIn:   3600,
				})
			},
			wantErr:        true,
			wantStatusCode: http.StatusOK,
		},
		{
			name: "invalid expires_in",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(tokenResponse{
					AccessToken: "test-token",
					ExpiresIn:   0,
				})
			},
			wantErr:        true,
			wantStatusCode: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.serverHandler)
			defer server.Close()

			provider := NewTokenProvider(server.URL, "test-client", "test-secret", "test-scope")
			ctx := context.Background()

			req, err := provider.createTokenRequest(ctx)
			if err != nil {
				t.Fatalf("createTokenRequest() error = %v", err)
			}

			req.URL.Host = strings.TrimPrefix(server.URL, "http://")
			req.URL.Scheme = "http"

			resp, statusCode, err := provider.executeTokenRequest(req)

			if (err != nil) != tt.wantErr {
				t.Errorf("executeTokenRequest() error = %v, wantErr %v", err, tt.wantErr)
			}

			if statusCode != tt.wantStatusCode {
				t.Errorf("statusCode = %v, want %v", statusCode, tt.wantStatusCode)
			}

			if !tt.wantErr && tt.validateTokenResp != nil {
				tt.validateTokenResp(t, resp)
			}
		})
	}
}

func TestGetToken_Caching(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: fmt.Sprintf("token-%d", callCount),
			ExpiresIn:   3600,
		})
	}))
	defer server.Close()

	provider := NewTokenProvider(server.URL, "test-client", "test-secret", "test-scope")
	ctx := context.Background()

	token1, err := provider.GetToken(ctx)
	if err != nil {
		t.Fatalf("GetToken() error = %v", err)
	}

	token2, err := provider.GetToken(ctx)
	if err != nil {
		t.Fatalf("GetToken() error = %v", err)
	}

	if token1 != token2 {
		t.Errorf("Token not cached: token1 = %v, token2 = %v", token1, token2)
	}

	if callCount != 1 {
		t.Errorf("Server called %d times, want 1 (should use cache)", callCount)
	}

	provider.tokenExpiresAt = time.Now().Add(-1 * time.Minute)

	token3, err := provider.GetToken(ctx)
	if err != nil {
		t.Fatalf("GetToken() error = %v", err)
	}

	if token3 == token1 {
		t.Errorf("Token should be refreshed after expiration")
	}

	if callCount != 2 {
		t.Errorf("Server called %d times, want 2 (should refresh)", callCount)
	}
}
