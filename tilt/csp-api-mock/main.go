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
	"log"
	"net"
	"net/http"
	"time"

	"csp-api-mock/pkg/handler"
	"csp-api-mock/pkg/store"

	loggingpb "cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/grpc"
)

// This is an in-cluster mock served to the Tilt dev environment only, so both
// listeners intentionally bind all interfaces (gosec G102).
const (
	httpAddr = ":8080"
	grpcAddr = ":50051"

	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
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

	log.Printf("CSP API Mock: HTTP on %s, gRPC on %s", httpAddr, grpcAddr)

	server := &http.Server{
		Addr:              httpAddr, //nolint:gosec // G102: dev-only mock, binds all interfaces by design
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
	}

	log.Fatal(server.ListenAndServe())
}

func startGRPCServer(gcpServer *handler.GCPLoggingServer) {
	var lc net.ListenConfig

	//nolint:gosec // G102: dev-only mock, binds all interfaces by design
	lis, err := lc.Listen(context.Background(), "tcp", grpcAddr)
	if err != nil {
		log.Fatalf("Failed to listen on gRPC port: %v", err)
	}

	grpcServer := grpc.NewServer()
	loggingpb.RegisterLoggingServiceV2Server(grpcServer, gcpServer)

	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("gRPC server failed: %v", err)
	}
}
