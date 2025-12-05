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

const GCPMethodUpcomingMaintenance = "compute.instances.upcomingMaintenance"

type GCPHandler struct {
	store *store.EventStore
}

func NewGCPHandler(eventStore *store.EventStore) *GCPHandler {
	return &GCPHandler{store: eventStore}
}

func (h *GCPHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/gcp/inject", h.handleInject)
	mux.HandleFunc("/gcp/events/clear", h.handleClear)
}

func (h *GCPHandler) handleInject(w http.ResponseWriter, r *http.Request) {
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
		req.ID = fmt.Sprintf("gcp-event-%d", time.Now().UnixNano())
	}
	req.CSP = store.CSPGCP

	existing, exists := h.store.Get(req.ID)
	statusCode := http.StatusCreated

	if exists {
		mergeEvent(existing, &req)
		h.store.Update(existing)
		statusCode = http.StatusOK
		log.Printf("GCP: Updated event %s", existing.ID)
	} else {
		setDefaults(&req)
		h.store.Add(&req)
		log.Printf("GCP: Created event %s", req.ID)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]string{"eventId": req.ID})
}

func (h *GCPHandler) handleClear(w http.ResponseWriter, r *http.Request) {
	h.store.ClearByCSP(store.CSPGCP)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "cleared"})
}

func setDefaults(e *store.MaintenanceEvent) {
	if e.Status == "" {
		e.Status = "PENDING"
	}
	if e.EventTypeCode == "" {
		e.EventTypeCode = "compute.instances.upcomingMaintenance"
	}
	if e.MaintenanceType == "" {
		e.MaintenanceType = "SCHEDULED"
	}
}
