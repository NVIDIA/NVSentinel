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

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/google/uuid"
)

type Config struct {
	Exporter struct {
		Metadata struct {
			Cluster     string `toml:"cluster"`
			Environment string `toml:"environment"`
		} `toml:"metadata"`
		Sink struct {
			Endpoint           string `toml:"endpoint"`
			InsecureSkipVerify bool   `toml:"insecure_skip_verify"`
		} `toml:"sink"`
		OIDC struct {
			TokenURL           string `toml:"token_url"`
			ClientID           string `toml:"client_id"`
			InsecureSkipVerify bool   `toml:"insecure_skip_verify"`
		} `toml:"oidc"`
	} `toml:"exporter"`
}

type HealthEvent struct {
	Version           int      `json:"version"`
	Agent             string   `json:"agent"`
	ComponentClass    string   `json:"componentClass"`
	CheckName         string   `json:"checkName"`
	IsFatal           bool     `json:"isFatal"`
	IsHealthy         bool     `json:"isHealthy"`
	Message           string   `json:"message"`
	RecommendedAction int      `json:"recommendedAction"`
	ErrorCode         []string `json:"errorCode"`
	EntitiesImpacted  []struct {
		EntityType  string `json:"entityType"`
		EntityValue string `json:"entityValue"`
	} `json:"entitiesImpacted"`
	NodeName string `json:"nodeName"`
}

type CloudEvent struct {
	SpecVersion string                 `json:"specversion"`
	Type        string                 `json:"type"`
	Source      string                 `json:"source"`
	ID          string                 `json:"id"`
	Time        string                 `json:"time"`
	Data        map[string]interface{} `json:"data"`
}

func main() {
	configPath := flag.String("config", "/etc/config/config.toml", "path to config file")
	eventPath := flag.String("event", "/data/healthevent.json", "path to health event file")
	flag.Parse()

	slog.Info("Health Events Exporter POC")

	slog.Info("Step 1: Loading configuration from ConfigMap")

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	slog.Info("Endpoint", "endpoint", cfg.Exporter.Sink.Endpoint)
	slog.Info("Cluster", "cluster", cfg.Exporter.Metadata.Cluster)
	slog.Info("Environment", "environment", cfg.Exporter.Metadata.Environment)

	slog.Info("Step 2: Loading OIDC client secret from Secret")

	clientSecret := os.Getenv("OIDC_CLIENT_SECRET")
	if clientSecret == "" {
		log.Fatalf("OIDC_CLIENT_SECRET environment variable not set")
	}

	slog.Info("Client secret loaded")

	slog.Info("Step 3: Getting OIDC token from IdP")

	token, err := getOIDCToken(
		cfg.Exporter.OIDC.TokenURL,
		cfg.Exporter.OIDC.ClientID,
		clientSecret,
		cfg.Exporter.OIDC.InsecureSkipVerify,
	)
	if err != nil {
		log.Fatalf("Failed to get OIDC token: %v", err)
	}

	slog.Info("Token acquired")

	slog.Info("Step 4: Reading health event from", "path", *eventPath)

	healthEvent, err := loadHealthEvent(*eventPath)
	if err != nil {
		log.Fatalf("Failed to load health event: %v", err)
	}

	slog.Info("Health event loaded", "componentClass", healthEvent.ComponentClass, "nodeName", healthEvent.NodeName)

	slog.Info("Step 5: Converting to CloudEvent format")

	cloudEvent := convertToCloudEvent(healthEvent, cfg)
	slog.Info("CloudEvent ID", "id", cloudEvent.ID)
	slog.Info("Type", "type", cloudEvent.Type)
	slog.Info("Source", "source", cloudEvent.Source)

	slog.Info("Step 6: Sending CloudEvent to", "endpoint", cfg.Exporter.Sink.Endpoint)

	err = sendCloudEvent(cloudEvent, cfg.Exporter.Sink.Endpoint, token, cfg.Exporter.Sink.InsecureSkipVerify)
	if err != nil {
		log.Fatalf("Failed to send event: %v", err)
	}

	slog.Info("Event successfully sent")

	slog.Info("POC Complete")
	os.Exit(0)
}

func loadConfig(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("decode failed: %w", err)
	}

	if cfg.Exporter.Sink.Endpoint == "" {
		return nil, fmt.Errorf("exporter.sink.endpoint is required")
	}

	if cfg.Exporter.Metadata.Cluster == "" {
		return nil, fmt.Errorf("exporter.metadata.cluster is required")
	}

	if cfg.Exporter.Metadata.Environment == "" {
		return nil, fmt.Errorf("exporter.metadata.environment is required")
	}

	if cfg.Exporter.OIDC.TokenURL == "" {
		return nil, fmt.Errorf("exporter.oidc.token_url is required")
	}

	if cfg.Exporter.OIDC.ClientID == "" {
		return nil, fmt.Errorf("exporter.oidc.client_id is required")
	}

	return &cfg, nil
}

var (
	cachedToken    string
	tokenExpiresAt time.Time
)

func getOIDCToken(tokenURL, clientID, clientSecret string, insecureSkipVerify bool) (string, error) {
	if cachedToken != "" && time.Now().Before(tokenExpiresAt.Add(-time.Minute)) {
		return cachedToken, nil
	}

	formData := url.Values{
		"scope":      {"telemetry-write"},
		"grant_type": {"client_credentials"},
	}
	payload := bytes.NewBufferString(formData.Encode())

	//nolint:gosec // G402: InsecureSkipVerify is configurable for POC/development use
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: insecureSkipVerify,
			},
		},
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, tokenURL, payload)
	if err != nil {
		return "", fmt.Errorf("unable to create HTTP request: %w", err)
	}

	auth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", clientID, clientSecret)))

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Add("Authorization", fmt.Sprintf("Basic %s", auth))

	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("unable to make HTTP request: %w", err)
	}

	defer func() {
		_ = res.Body.Close()
	}()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", fmt.Errorf("unable to read body of response: %w", err)
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("unable to fetch token: %s", string(body))
	}

	tokenResponse := make(map[string]interface{})
	if err = json.Unmarshal(body, &tokenResponse); err != nil {
		return "", fmt.Errorf("unable to unmarshal response: %w", err)
	}

	expiresInSeconds := tokenResponse["expires_in"].(float64)
	tokenExpiresAt = time.Now().Add(time.Duration(expiresInSeconds) * time.Second)
	cachedToken = tokenResponse["access_token"].(string)

	return cachedToken, nil
}

func loadHealthEvent(path string) (*HealthEvent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read failed: %w", err)
	}

	var event HealthEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, fmt.Errorf("unmarshal failed: %w", err)
	}

	return &event, nil
}

func convertToCloudEvent(event *HealthEvent, cfg *Config) *CloudEvent {
	now := time.Now()

	entities := make([]map[string]interface{}, len(event.EntitiesImpacted))
	for i, entity := range event.EntitiesImpacted {
		entities[i] = map[string]interface{}{
			"entityType":  entity.EntityType,
			"entityValue": entity.EntityValue,
		}
	}

	healthEventData := map[string]interface{}{
		"agent":              event.Agent,
		"componentClass":     event.ComponentClass,
		"checkName":          event.CheckName,
		"message":            event.Message,
		"nodeName":           event.NodeName,
		"isFatal":            event.IsFatal,
		"isHealthy":          event.IsHealthy,
		"recommendedAction":  event.RecommendedAction,
		"errorCode":          event.ErrorCode,
		"entitiesImpacted":   entities,
		"generatedTimestamp": now.Format(time.RFC3339Nano),
	}

	return &CloudEvent{
		SpecVersion: "1.0",
		Type:        "com.nvidia.nvsentinel.healthevents.v1",
		Source:      fmt.Sprintf("nvsentinel/%s", cfg.Exporter.Metadata.Cluster),
		ID:          uuid.New().String(),
		Time:        now.Format(time.RFC3339Nano),
		Data: map[string]interface{}{
			"metadata": map[string]interface{}{
				"cluster":     cfg.Exporter.Metadata.Cluster,
				"environment": cfg.Exporter.Metadata.Environment,
			},
			"healthEvent": healthEventData,
		},
	}
}

func sendCloudEvent(event *CloudEvent, endpoint, token string, insecureSkipVerify bool) error {
	body, err := json.MarshalIndent(event, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal failed: %w", err)
	}

	slog.Info("CloudEvent payload", "body", string(body))

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("request creation failed: %w", err)
	}

	req.Header.Set("Content-Type", "application/cloudevents+json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	//nolint:gosec // G402: InsecureSkipVerify is configurable for POC/development use
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: insecureSkipVerify,
			},
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	slog.Info("Response status", "status", resp.StatusCode)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	return nil
}
