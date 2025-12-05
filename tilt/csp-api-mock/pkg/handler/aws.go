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

package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"csp-api-mock/pkg/store"
)

type AWSHandler struct {
	store *store.EventStore
}

func NewAWSHandler(eventStore *store.EventStore) *AWSHandler {
	return &AWSHandler{store: eventStore}
}

func (h *AWSHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/aws/health", h.handleHealth)
	mux.HandleFunc("/aws/inject", h.handleInject)
	mux.HandleFunc("/aws/events/clear", h.handleClear)
}

func (h *AWSHandler) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	target := r.Header.Get("X-Amz-Target")
	switch target {
	case "AWSHealth_20160804.DescribeEvents":
		h.describeEvents(w)
	case "AWSHealth_20160804.DescribeAffectedEntities":
		h.describeEntities(w, r)
	case "AWSHealth_20160804.DescribeEventDetails":
		h.describeDetails(w, r)
	default:
		http.Error(w, "Unknown operation", http.StatusBadRequest)
	}
}

func (h *AWSHandler) describeEvents(w http.ResponseWriter) {
	events := h.store.ListByCSP(store.CSPAWS)
	awsEvents := make([]map[string]interface{}, 0, len(events))

	for _, e := range events {
		awsEvents = append(awsEvents, h.toAWSEvent(e))
	}

	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	json.NewEncoder(w).Encode(map[string]interface{}{"events": awsEvents})
}

func (h *AWSHandler) describeEntities(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Filter struct {
			EventArns []string `json:"eventArns"`
		} `json:"filter"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	events := h.store.ListByCSP(store.CSPAWS)
	var entities []map[string]interface{}

	for _, e := range events {
		arn := h.eventARN(e)
		for _, targetArn := range req.Filter.EventArns {
			if arn == targetArn {
				entities = append(entities, map[string]interface{}{
					"entityValue":     e.InstanceID,
					"eventArn":        arn,
					"entityArn":       e.EntityARN,
					"awsAccountId":    e.AccountID,
					"statusCode":      "IMPAIRED",
					"lastUpdatedTime": float64(e.UpdatedAt.Unix()),
				})
			}
		}
	}

	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	json.NewEncoder(w).Encode(map[string]interface{}{"entities": entities})
}

func (h *AWSHandler) describeDetails(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EventArns []string `json:"eventArns"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	events := h.store.ListByCSP(store.CSPAWS)
	var details []map[string]interface{}

	for _, e := range events {
		arn := h.eventARN(e)
		for _, targetArn := range req.EventArns {
			if arn == targetArn {
				details = append(details, map[string]interface{}{
					"event":            h.toAWSEvent(e),
					"eventDescription": map[string]string{"latestDescription": e.Description},
				})
			}
		}
	}

	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"successfulSet": details,
		"failedSet":     []interface{}{},
	})
}

func (h *AWSHandler) toAWSEvent(e *store.MaintenanceEvent) map[string]interface{} {
	status := e.Status
	if status == "" {
		status = "upcoming"
	}
	eventType := e.EventTypeCode
	if eventType == "" {
		eventType = "AWS_EC2_MAINTENANCE_SCHEDULED"
	}

	var startTime, endTime float64
	if e.ScheduledStart != nil {
		startTime = float64(e.ScheduledStart.Unix())
	}
	if e.ScheduledEnd != nil {
		endTime = float64(e.ScheduledEnd.Unix())
	}

	return map[string]interface{}{
		"arn":               h.eventARN(e),
		"availabilityZone":  e.Zone,
		"service":           "EC2",
		"eventTypeCode":     eventType,
		"eventTypeCategory": "scheduledChange",
		"region":            e.Region,
		"statusCode":        status,
		"startTime":         startTime,
		"endTime":           endTime,
		"lastUpdatedTime":   float64(e.UpdatedAt.Unix()),
		"eventScopeCode":    "ACCOUNT_SPECIFIC",
	}
}

func (h *AWSHandler) eventARN(e *store.MaintenanceEvent) string {
	if e.EventARN != "" {
		return e.EventARN
	}
	eventType := e.EventTypeCode
	if eventType == "" {
		eventType = "AWS_EC2_MAINTENANCE_SCHEDULED"
	}
	return fmt.Sprintf("arn:aws:health:%s::event/EC2/%s/%s", e.Region, eventType, e.ID)
}

func (h *AWSHandler) handleInject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req store.MaintenanceEvent
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.ID == "" {
		req.ID = fmt.Sprintf("aws-event-%d", time.Now().UnixNano())
	}
	if req.Region == "" {
		req.Region = "us-east-1"
	}
	req.CSP = store.CSPAWS

	existing, exists := h.store.Get(req.ID)
	statusCode := http.StatusCreated

	if exists {
		mergeEvent(existing, &req)
		h.store.Update(existing)
		statusCode = http.StatusOK
		log.Printf("AWS: Updated event %s", existing.ID)
	} else {
		if req.Status == "" {
			req.Status = "upcoming"
		}
		if req.EventTypeCode == "" {
			req.EventTypeCode = "AWS_EC2_MAINTENANCE_SCHEDULED"
		}
		if req.EventARN == "" {
			req.EventARN = h.eventARN(&req)
		}
		if req.EntityARN == "" {
			req.EntityARN = fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", req.Region, req.AccountID, req.InstanceID)
		}
		req.AffectedEntities = []string{req.InstanceID}
		h.store.Add(&req)
		log.Printf("AWS: Created event %s", req.ID)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]string{
		"eventId":   req.ID,
		"eventArn":  h.eventARN(&req),
		"entityArn": req.EntityARN,
	})
}

func (h *AWSHandler) handleClear(w http.ResponseWriter, r *http.Request) {
	h.store.ClearByCSP(store.CSPAWS)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "cleared"})
}
