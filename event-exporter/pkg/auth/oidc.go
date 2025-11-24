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
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nvidia/nvsentinel/event-exporter/pkg/metrics"
	"k8s.io/apimachinery/pkg/util/wait"
)

type TokenProvider struct {
	tokenURL           string
	clientID           string
	clientSecret       string
	scope              string
	insecureSkipVerify bool

	mu             sync.Mutex
	cachedToken    string
	tokenExpiresAt time.Time
}

type tokenResponse struct {
	AccessToken string  `json:"access_token"`
	ExpiresIn   float64 `json:"expires_in"`
}

func NewTokenProvider(tokenURL, clientID, clientSecret, scope string, insecureSkipVerify bool) *TokenProvider {
	return &TokenProvider{
		tokenURL:           tokenURL,
		clientID:           clientID,
		clientSecret:       clientSecret,
		scope:              scope,
		insecureSkipVerify: insecureSkipVerify,
	}
}

func (p *TokenProvider) GetToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cachedToken != "" && time.Now().Before(p.tokenExpiresAt.Add(-time.Minute)) {
		return p.cachedToken, nil
	}

	return p.fetchNewToken(ctx)
}

func (p *TokenProvider) fetchNewToken(ctx context.Context) (string, error) {
	var tokenResp *tokenResponse

	attempt := 0

	backoffConfig := wait.Backoff{
		Steps:    8,
		Duration: 1 * time.Second,
		Factor:   2.0,
		Jitter:   0.1,
		Cap:      10 * time.Second,
	}

	err := wait.ExponentialBackoffWithContext(ctx, backoffConfig, func(ctx context.Context) (bool, error) {
		req, reqErr := p.createTokenRequest(ctx)
		if reqErr != nil {
			return false, reqErr
		}

		resp, statusCode, execErr := p.executeTokenRequest(req)
		if execErr != nil {
			if isRetriableHTTPStatus(statusCode) {
				attempt++
				slog.Warn("Token fetch failed, will retry",
					"attempt", attempt,
					"statusCode", statusCode,
					"error", execErr)

				return false, nil
			}

			return false, execErr
		}

		tokenResp = resp

		if attempt > 0 {
			slog.Info("Token fetch succeeded after retry", "attempt", attempt)
		}

		return true, nil
	})
	if err != nil {
		metrics.TokenRefreshErrors.Inc()
		return "", fmt.Errorf("token fetch failed after retries: %w", err)
	}

	p.cachedToken = tokenResp.AccessToken
	p.tokenExpiresAt = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)

	return p.cachedToken, nil
}

func isRetriableHTTPStatus(statusCode int) bool {
	if statusCode == 0 {
		return true
	}

	return statusCode == http.StatusTooManyRequests || (statusCode >= 500 && statusCode < 600)
}

func (p *TokenProvider) createTokenRequest(ctx context.Context) (*http.Request, error) {
	formData := url.Values{
		"scope":      {p.scope},
		"grant_type": {"client_credentials"},
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		p.tokenURL,
		strings.NewReader(formData.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	authBytes := fmt.Appendf(nil, "%s:%s", p.clientID, p.clientSecret)
	auth := base64.StdEncoding.EncodeToString(authBytes)

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", fmt.Sprintf("Basic %s", auth))

	return req, nil
}

func (p *TokenProvider) executeTokenRequest(req *http.Request) (*tokenResponse, int, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: p.insecureSkipVerify, //nolint:gosec // This is only used for testing
			},
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("token request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp tokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse response: %w", err)
	}

	if tokenResp.AccessToken == "" || tokenResp.ExpiresIn <= 0 {
		return nil, resp.StatusCode, fmt.Errorf("invalid token response: access_token or expires_in missing")
	}

	return &tokenResp, resp.StatusCode, nil
}
