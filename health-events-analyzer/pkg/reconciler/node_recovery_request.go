// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package reconciler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
)

const (
	annotationMetadataKey = "analyzer_recovery_annotation"
	annotationNodeUIDKey  = "analyzer_recovery_node_uid"
	annotationRequestKey  = "analyzer_recovery_request"
)

type annotationRecoveryRequest struct {
	RecoveredAt string                     `json:"recoveredAt"`
	Entities    []annotationRecoveryEntity `json:"entities,omitempty"`
}

type annotationRecoveryEntity struct {
	EntityType  string `json:"entityType"`
	EntityValue string `json:"entityValue"`
}

func parseAnnotationRecovery(node *corev1.Node, rule config.HealthEventsAnalyzerRule, value string, now time.Time) (
	*datamodels.HealthEventWithStatus, error,
) {
	request := annotationRecoveryRequest{RecoveredAt: strings.TrimSpace(value)}
	if strings.HasPrefix(request.RecoveredAt, "{") {
		decoder := json.NewDecoder(strings.NewReader(value))
		decoder.DisallowUnknownFields()

		if err := decoder.Decode(&request); err != nil {
			return nil, fmt.Errorf("decode recovery request: %w", err)
		}

		if err := decoder.Decode(new(any)); err != io.EOF {
			return nil, fmt.Errorf("recovery request must contain one JSON object")
		}
	}

	timestamp, err := time.Parse(time.RFC3339Nano, request.RecoveredAt)
	if err != nil {
		return nil, fmt.Errorf("recoveredAt must be an RFC 3339 timestamp: %w", err)
	}

	if timestamp.After(now) || timestamp.Before(node.CreationTimestamp.Time) {
		return nil, fmt.Errorf("recoveredAt must be between node creation and the current time")
	}

	entities, err := annotationEntities(rule.Recovery, request.Entities)
	if err != nil {
		return nil, err
	}

	requestParts := [][]byte{[]byte(node.UID), []byte(rule.Recovery.AnnotationKey), []byte(value)}
	digest := sha256.Sum256(bytes.Join(requestParts, []byte{0}))

	return &datamodels.HealthEventWithStatus{
		CreatedAt: timestamp,
		HealthEvent: &protos.HealthEvent{
			Version: 1, Agent: agentName, CheckName: rule.Name, NodeName: node.Name, IsHealthy: true,
			GeneratedTimestamp: timestamppb.New(timestamp), EntitiesImpacted: entities,
			Metadata: map[string]string{
				annotationMetadataKey: rule.Recovery.AnnotationKey,
				annotationNodeUIDKey:  string(node.UID),
				annotationRequestKey:  hex.EncodeToString(digest[:]),
			},
		},
		HealthEventStatus: &protos.HealthEventStatus{},
	}, nil
}

func annotationEntities(mapping *config.RecoveryMapping, requested []annotationRecoveryEntity) (
	[]*protos.Entity, error,
) {
	if len(requested) == 0 {
		return nil, nil
	}

	if mapping.Scope != config.RecoveryScopeEntity || len(requested) != len(mapping.EntityTypes) {
		return nil, fmt.Errorf("entities must identify exactly one value for every configured entity type")
	}

	allowed := make(map[string]bool, len(mapping.EntityTypes))
	for _, entityType := range mapping.EntityTypes {
		allowed[entityType] = true
	}

	entities := make([]*protos.Entity, 0, len(requested))
	for _, entity := range requested {
		if !allowed[entity.EntityType] || strings.TrimSpace(entity.EntityValue) == "" {
			return nil, fmt.Errorf("invalid or duplicate recovery entity %q", entity.EntityType)
		}

		delete(allowed, entity.EntityType)
		entities = append(entities, &protos.Entity{EntityType: entity.EntityType, EntityValue: entity.EntityValue})
	}

	return entities, nil
}
