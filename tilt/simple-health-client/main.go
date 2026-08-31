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
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcclient"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// tokenInterceptor attaches a projected ServiceAccount token as a Bearer
// credential on every call. This client posts events naming arbitrary nodes,
// so platform-connector's node-binding interceptor requires it to authenticate
// as an allowlisted cross-node identity. Kept inline rather than importing
// commons/pkg/grpcclient to keep this dev-only image's dependency set small.
func main() {
	socketPath := "/var/run/nvsentinel.sock"
	port := "8080"
	tokenPath := os.Getenv("PLATFORM_CONNECTOR_TOKEN_PATH")

	// The shared client helper, not a local copy: it also refuses an empty
	// token file rather than sending a bare "Bearer ".
	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	dialOpts = append(dialOpts, grpcclient.DialOptions(tokenPath)...)

	log.Printf("Starting health event API server on port %s", port)
	// G706: socketPath is this process's own flag/env configuration, not request data.
	log.Printf("Using socket path: %s (token auth: %v)", socketPath, tokenPath != "") //nolint:gosec

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

		conn, err := grpc.NewClient(fmt.Sprintf("unix://%s", socketPath), dialOpts...)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to connect to socket: %v", err), http.StatusInternalServerError)
			return
		}
		defer conn.Close()

		client := pb.NewPlatformConnectorClient(conn)

		healthEvents := &pb.HealthEvents{
			Version: 1,
			Events:  []*pb.HealthEvent{&healthEvent},
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		log.Printf("[DEBUG] Sending health event to platform-connector - Node: %s, CheckName: %s, RecommendedAction: %v",
			healthEvent.NodeName, healthEvent.CheckName, healthEvent.RecommendedAction)

		_, err = client.HealthEventOccurredV1(ctx, healthEvents)
		if err != nil {
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
