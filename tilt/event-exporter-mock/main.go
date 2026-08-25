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
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// unknownValue is the placeholder used when an incoming CloudEvent omits a field.
const unknownValue = "unknown"

var (
	tokenRequestCount   int64
	eventsReceivedCount int64
	receivedEvents      []map[string]any
	eventsMutex         sync.RWMutex
)

// writeJSON encodes v to w, logging (rather than propagating) encode failures:
// the status line has already been written by the time this runs.
func writeJSON(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/token", handleTokenRequest)
	mux.HandleFunc("/events", handleEventSink)
	mux.HandleFunc("/events/list", handleEventsList)
	mux.HandleFunc("/metrics", handleMetrics)
	mux.HandleFunc("/healthz", handleHealth)

	server := &http.Server{
		Addr:         ":8443",
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	log.Println("Mock service starting on :8443")
	log.Println("  - POST /token   (OIDC token endpoint)")
	log.Println("  - POST /events  (CloudEvents sink)")
	log.Println("  - GET  /metrics (service metrics)")
	log.Println("  - GET  /healthz (health check)")

	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func handleTokenRequest(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&tokenRequestCount, 1)

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// G706: RemoteAddr is set by the server from the socket peer, not client-controlled text.
	log.Printf("Token request from %s", r.RemoteAddr) //nolint:gosec

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	response := map[string]any{
		"access_token": fmt.Sprintf("mock-token-%d", time.Now().Unix()),
		"expires_in":   3600,
		"token_type":   "Bearer",
	}

	writeJSON(w, response)
}

func handleEventSink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Failed to read request body: %v", err)
		http.Error(w, "Failed to read body", http.StatusBadRequest)

		return
	}
	defer r.Body.Close()

	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		log.Printf("Invalid JSON: %v", err)
		http.Error(w, "Invalid JSON", http.StatusBadRequest)

		return
	}

	atomic.AddInt64(&eventsReceivedCount, 1)

	eventID := unknownValue
	if id, ok := event["id"].(string); ok {
		eventID = id
	}

	eventType := unknownValue
	if typ, ok := event["type"].(string); ok {
		eventType = typ
	}

	source := unknownValue
	if src, ok := event["source"].(string); ok {
		source = src
	}

	eventsMutex.Lock()

	receivedEvents = append(receivedEvents, event)
	if len(receivedEvents) > 100 {
		receivedEvents = receivedEvents[1:]
	}
	eventsMutex.Unlock()

	log.Printf("CloudEvent received: id=%s type=%s source=%s", eventID, eventType, source)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	writeJSON(w, map[string]string{"status": "accepted"})
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "# HELP mock_token_requests_total Total token requests received\n")
	fmt.Fprintf(w, "# TYPE mock_token_requests_total counter\n")
	fmt.Fprintf(w, "mock_token_requests_total %d\n", atomic.LoadInt64(&tokenRequestCount))
	fmt.Fprintf(w, "\n")
	fmt.Fprintf(w, "# HELP mock_events_received_total Total CloudEvents received\n")
	fmt.Fprintf(w, "# TYPE mock_events_received_total counter\n")
	fmt.Fprintf(w, "mock_events_received_total %d\n", atomic.LoadInt64(&eventsReceivedCount))
}

func handleEventsList(w http.ResponseWriter, r *http.Request) {
	eventsMutex.RLock()
	defer eventsMutex.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	writeJSON(w, map[string]any{
		"events": receivedEvents,
		"count":  len(receivedEvents),
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write([]byte("ok")); err != nil {
		log.Printf("write health response: %v", err)
	}
}
