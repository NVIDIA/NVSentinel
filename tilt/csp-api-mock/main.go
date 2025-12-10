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
	"log"
	"net"
	"net/http"

	loggingpb "cloud.google.com/go/logging/apiv2/loggingpb"
	"csp-api-mock/pkg/handler"
	"csp-api-mock/pkg/store"
	"google.golang.org/grpc"
)

func main() {
	eventStore := store.NewEventStore()
	mux := http.NewServeMux()

	handler.NewGCPHandler(eventStore).RegisterRoutes(mux)
	handler.NewAWSHandler(eventStore).RegisterRoutes(mux)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	go startGRPCServer(handler.NewGCPLoggingServer(eventStore))

	log.Printf("CSP API Mock: HTTP on :8080, gRPC on :50051")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

func startGRPCServer(gcpServer *handler.GCPLoggingServer) {
	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("Failed to listen on gRPC port: %v", err)
	}

	grpcServer := grpc.NewServer()
	loggingpb.RegisterLoggingServiceV2Server(grpcServer, gcpServer)

	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("gRPC server failed: %v", err)
	}
}
