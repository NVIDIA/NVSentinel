/*
Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package pkg

import (
	"context"
	"fmt"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-event-client/protos"
)

// HealthEventManager handles health event operations
type HealthEventManager struct {
	client pb.PlatformConnectorClient
	conn   *grpc.ClientConn
}

// NewHealthEventManager creates a new health event manager
func NewHealthEventManager(socketPath string) (*HealthEventManager, error) {
	log.Printf("Attempting to connect to socket: unix://%s", socketPath)
	conn, err := grpc.NewClient(
		fmt.Sprintf("unix://%s", socketPath),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)

	if err != nil {
		return nil, fmt.Errorf("failed to connect to platform connector: %w", err)
	}

	client := pb.NewPlatformConnectorClient(conn)

	return &HealthEventManager{
		client: client,
		conn:   conn,
	}, nil
}

// Close closes the gRPC connection
func (s *HealthEventManager) Close() error {
	if s.conn != nil {
		return s.conn.Close()
	}

	return nil
}

// CreateHealthEvent creates a health event from configuration
func (s *HealthEventManager) CreateHealthEvent(config *Config) (*pb.HealthEvent, error) {
	healthEvent := &pb.HealthEvent{
		Version:           1,
		Agent:             "dgxcops",
		CheckName:         config.ErrorCode,
		ComponentClass:    "NODE",
		Message:           config.Reason,
		RecommendedAction: pb.RecommenedAction(config.RecommendedAction),
		ErrorCode:         []string{config.ErrorCode},
		IsHealthy:         config.IsHealthy,
		EntitiesImpacted: []*pb.Entity{
			{
				EntityType:  "node",
				EntityValue: config.NodeName,
			},
		},
		Metadata: map[string]string{
			"creator_id": config.CreatorID,
		},
		GeneratedTimestamp: timestamppb.Now(),
		NodeName:           config.NodeName,
		QuarantineOverrides: &pb.BehaviourOverrides{
			Force: true,
			Skip:  config.SkipQuarantine,
		},
		DrainOverrides: &pb.BehaviourOverrides{
			Force: config.Force,
			Skip:  config.SkipDrain,
		},
	}

	return healthEvent, nil
}

// SendHealthEvent sends a health event to the platform connector
func (s *HealthEventManager) SendHealthEvent(ctx context.Context, healthEvent *pb.HealthEvent) error {
	healthEvents := &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{healthEvent},
	}

	return s.sendWithRetries(ctx, healthEvents)
}

// sendWithRetries sends health events with retry logic
func (s *HealthEventManager) sendWithRetries(ctx context.Context, healthEvents *pb.HealthEvents) error {
	maxRetries := 10
	initialDelay := 5 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		_, err := s.client.HealthEventOccuredV1(ctx, healthEvents)
		if err == nil {
			return nil // Success
		}

		log.Printf("Attempt %d failed: %v", attempt, err)

		if attempt == maxRetries {
			return fmt.Errorf("failed to send health event after %d attempts: %w", maxRetries, err)
		}

		// Linear backoff
		delay := time.Duration(float64(initialDelay) * float64(attempt))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
			continue
		}
	}

	return fmt.Errorf("failed to send health event after %d attempts", maxRetries)
}

// ProcessHealthEvent is the main business logic method that orchestrates the entire process
func (s *HealthEventManager) ProcessHealthEvent(config *Config) error {
	log.Printf("Processing health event for node: %s", config.NodeName)
	log.Printf("Error code: %s, Reason: %s, Force: %t", config.ErrorCode, config.Reason, config.Force)

	// Create health event
	healthEvent, err := s.CreateHealthEvent(config)
	if err != nil {
		return fmt.Errorf("failed to create health event: %w", err)
	}

	// Debug: Log the health event structure
	log.Printf("Health event created - QuarantineOverrides: %+v, DrainOverrides: %+v",
		healthEvent.QuarantineOverrides, healthEvent.DrainOverrides)
	log.Printf("Health event structure: %+v", healthEvent)

	// Send health event with retries
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	log.Printf("Sending health event to platform connector...")

	if err := s.SendHealthEvent(ctx, healthEvent); err != nil {
		return fmt.Errorf("failed to send health event: %w", err)
	}

	log.Printf("=== SUCCESS: Health event sent for node %s ===", config.NodeName)

	return nil
}
