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
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcclient"
	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func main() {
	socketPath := "/var/run/nvsentinel.sock"
	port := "8080"
	// This client posts events naming arbitrary nodes, so platform-connector's
	// node-binding interceptor requires it to authenticate as an allowlisted
	// cross-node identity.
	tokenPath := os.Getenv("PLATFORM_CONNECTOR_TOKEN_PATH")

	// The HEALTH_PUBLISH_* environment selects a direct TLS connection to the
	// deployment platform connector; otherwise the fallback dials the
	// daemonset's node-local socket. The connection is lazy and safe for
	// concurrent use, so one client serves every request.
	conn, client, _, err := healthpub.DialFromEnvOr(func() (*grpc.ClientConn, error) {
		// The shared client helper, not a local copy: it also refuses an empty
		// token file rather than sending a bare "Bearer ".
		dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
		dialOpts = append(dialOpts, grpcclient.DialOptions(tokenPath)...)

		return grpc.NewClient(fmt.Sprintf("unix://%s", socketPath), dialOpts...)
	})
	if err != nil {
		log.Fatalf("Failed to dial platform connector: %v", err)
	}

	log.Printf("Starting health event API server on port %s", port)
	log.Printf("Using publish target: %s", conn.Target())

	http.HandleFunc("/health-event", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Only POST method allowed", http.StatusMethodNotAllowed)
			return
		}

		var healthEvent pb.HealthEvent
		if err := json.NewDecoder(r.Body).Decode(&healthEvent); err != nil {
			http.Error(w, fmt.Sprintf("Error parsing JSON: %v", err), http.StatusBadRequest)
			return
		}

		log.Printf("[DEBUG] Received health event - Node: %s, CheckName: %s, Agent: %s, IsFatal: %v, RecommendedAction: %v",
			healthEvent.NodeName, healthEvent.CheckName, healthEvent.Agent, healthEvent.IsFatal, healthEvent.RecommendedAction)

		healthEvent.GeneratedTimestamp = timestamppb.Now()

		healthEvents := &pb.HealthEvents{
			Version: 1,
			Events:  []*pb.HealthEvent{&healthEvent},
		}

		// The same request deadline as the real monitors' clients. The server
		// answers only once the batch is stored and waits up to its bounded
		// condition-update time (10 s by default) before replying, so a
		// shorter deadline here turns a slow API server into a test failure
		// even though the event was stored.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// The deployment platform connector requires an idempotency key on
		// every batch: unique per request since this test client never
		// retries, unless the caller supplies one (to exercise a resend).
		idempotencyKey := r.Header.Get("Idempotency-Key")
		if idempotencyKey == "" {
			idempotencyKey = fmt.Sprintf("shc-%d", time.Now().UnixNano())
		}

		ctx = metadata.AppendToOutgoingContext(ctx, healthpub.IdempotencyKeyHeader, idempotencyKey)

		log.Printf("[DEBUG] Sending health event to platform-connector - Node: %s, CheckName: %s, RecommendedAction: %v",
			healthEvent.NodeName, healthEvent.CheckName, healthEvent.RecommendedAction)

		if _, err := client.HealthEventOccurredV1(ctx, healthEvents); err != nil {
			log.Printf("[ERROR] Failed to send health event: %v", err)
			http.Error(w, fmt.Sprintf("Failed to send health event: %v", err), http.StatusInternalServerError)

			return
		}

		log.Printf("[DEBUG] SUCCESS: Health event sent for node %s with CheckName: %s",
			healthEvent.NodeName, healthEvent.CheckName)

		w.Header().Set("Content-Type", "application/json")

		response := map[string]string{"status": "success", "message": "Health event sent"}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			log.Printf("Failed to encode JSON response: %v", err)
		}
	})

	// #nosec G114 - test client, timeouts not critical
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		slog.Error("Failed to start HTTP server", "error", err)
		os.Exit(1)
	}
}
